package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The update check asks one of the two places releases are published for
// the latest one. Both carry the same tags and files.
var updateSources = map[string]struct {
	Name string // shown in the web interface
	API  string // latest release, as JSON
	Repo string // web page of the repository; downloads are under it
}{
	"gitea":  {"Gitea", "https://git.redetzke.aero/api/v1/repos/Redetzke/GHOSTWIRE/releases/latest", "https://git.redetzke.aero/Redetzke/GHOSTWIRE"},
	"github": {"GitHub", "https://api.github.com/repos/danielredetzke/GHOSTWIRE/releases/latest", "https://github.com/danielredetzke/GHOSTWIRE"},
}

const updateCheckFreq = 24 * time.Hour

// Release is the latest published release as the source reports it.
type Release struct {
	Version   string    `json:"version"` // tag, e.g. "v0.4.0"
	Published time.Time `json:"published"`
	Notes     string    `json:"notes"` // Markdown
	URL       string    `json:"url"`   // release page
}

// UpdateStatus is shown in the settings; Available also reaches the sidebar
// and the Dashboard through /auth/me.
type UpdateStatus struct {
	Enabled   bool       `json:"enabled"`
	Source    string     `json:"source"`
	Current   string     `json:"current"`
	Latest    *Release   `json:"latest"`
	Available bool       `json:"available"` // Latest is newer than Current
	Checked   *time.Time `json:"checked"`   // last attempt
	Error     string     `json:"error,omitempty"`
	LastOK    *time.Time `json:"lastOk"` // last attempt that worked
	// Download links for this server's platform; empty when no release
	// file is built for it.
	Arch      string `json:"arch"`
	File      string `json:"file,omitempty"`
	FileURL   string `json:"fileUrl,omitempty"`
	SumsURL   string `json:"sumsUrl,omitempty"`
	SourceURL string `json:"sourceUrl"` // repository page of the source
}

type Updater struct {
	enabled atomic.Bool
	kick    chan struct{}
	fetch   func(ctx context.Context, url string) (*Release, error) // replaced in tests

	mu      sync.Mutex
	source  string
	latest  *Release
	checked *time.Time
	lastOK  *time.Time
	err     string
}

func newUpdater(c UpdatesConfig) *Updater {
	u := &Updater{kick: make(chan struct{}, 1), fetch: fetchRelease, source: c.Source}
	u.enabled.Store(c.checkEnabled())
	return u
}

// Set applies the settings. A new source or switching the check on checks
// at once; switching it off forgets what the last check found.
func (u *Updater) Set(c UpdatesConfig) {
	if u == nil {
		return
	}
	on := c.checkEnabled()
	u.mu.Lock()
	changed := u.source != c.Source || u.enabled.Load() != on
	if u.source != c.Source || !on {
		u.latest, u.checked, u.lastOK, u.err = nil, nil, nil, ""
	}
	u.source = c.Source
	u.enabled.Store(on)
	u.mu.Unlock()
	if changed && on {
		select {
		case u.kick <- struct{}{}:
		default:
		}
	}
}

// Run checks once a day while the check is on.
func (u *Updater) Run(stop <-chan struct{}) {
	t := time.NewTicker(updateCheckFreq)
	defer t.Stop()
	for {
		if u.enabled.Load() {
			u.Check(context.Background())
		}
		select {
		case <-stop:
			return
		case <-t.C:
		case <-u.kick:
		}
	}
}

// Check asks the source for the latest release now.
func (u *Updater) Check(ctx context.Context) {
	u.mu.Lock()
	source := u.source
	u.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rel, err := u.fetch(ctx, updateSources[source].API)
	now := time.Now()
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.source != source { // the source changed meanwhile; that check counts
		return
	}
	u.checked = &now
	if err != nil {
		u.err = err.Error()
		slog.Warn("update check failed", "source", source, "err", err)
		return
	}
	u.latest, u.lastOK, u.err = rel, &now, ""
	if newerVersion(rel.Version, version) {
		slog.Info("update available", "version", rel.Version, "running", version)
	}
}

func (u *Updater) Status() UpdateStatus {
	if u == nil {
		return UpdateStatus{Current: version}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	src := updateSources[u.source]
	st := UpdateStatus{
		Enabled: u.enabled.Load(), Source: u.source, Current: version, Latest: u.latest,
		Checked: u.checked, Error: u.err, LastOK: u.lastOK, Arch: releaseArch(), SourceURL: src.Repo,
	}
	if u.latest != nil {
		st.Available = newerVersion(u.latest.Version, version)
		if st.Arch != "" {
			st.File = fmt.Sprintf("%s-%s-linux-%s", appName, u.latest.Version, st.Arch)
			base := src.Repo + "/releases/download/" + u.latest.Version + "/"
			st.FileURL, st.SumsURL = base+st.File, base+"SHA256SUMS"
		}
	}
	return st
}

// Available returns the newer release's version, or "".
func (u *Updater) Available() string {
	if st := u.Status(); st.Enabled && st.Available {
		return st.Latest.Version
	}
	return ""
}

// releaseArch names this platform the way the release files do, or "" when
// no file is built for it.
func releaseArch() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	switch runtime.GOARCH {
	case "amd64", "arm64":
		return runtime.GOARCH
	case "arm":
		return "armv7"
	}
	return ""
}

func fetchRelease(ctx context.Context, url string) (*Release, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", appName+"/"+strings.TrimPrefix(version, "v"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, req.URL.Host)
	}
	// GitHub and Gitea name these fields the same.
	var r struct {
		Tag        string    `json:"tag_name"`
		Body       string    `json:"body"`
		Published  time.Time `json:"published_at"`
		URL        string    `json:"html_url"`
		Draft      bool      `json:"draft"`
		Prerelease bool      `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return nil, fmt.Errorf("unreadable answer from %s: %w", req.URL.Host, err)
	}
	if r.Draft || r.Prerelease || parseVersion(r.Tag) == nil {
		return nil, errors.New("the latest release is not a published version")
	}
	return &Release{Version: r.Tag, Published: r.Published, Notes: r.Body, URL: r.URL}, nil
}

var versionRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`)

// parseVersion reads "v0.4.0", "0.4.0" or "v0.4.0-3-gb18d16a" (a build
// after v0.4.0) as major, minor and patch, or nil.
func parseVersion(s string) []int {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return nil
	}
	out := make([]int, 3)
	for i := range out {
		out[i], _ = strconv.Atoi(m[i+1])
	}
	return out
}

// newerVersion reports whether latest is a higher version than running.
// A running version that is not a version number is never out of date.
func newerVersion(latest, running string) bool {
	l, r := parseVersion(latest), parseVersion(running)
	if l == nil || r == nil {
		return false
	}
	for i := range l {
		if l[i] != r[i] {
			return l[i] > r[i]
		}
	}
	return false
}
