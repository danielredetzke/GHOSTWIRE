package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewerVersion(t *testing.T) {
	for _, tc := range []struct {
		latest, running string
		want            bool
	}{
		{"v0.4.0", "v0.3.2", true},
		{"v0.4.0", "0.3.2", true},
		{"v0.10.0", "v0.9.9", true},
		{"v1.0.0", "v0.99.0", true},
		{"v0.4.0", "v0.4.0", false},
		{"v0.4.0", "v0.4.0-3-gb18d16a", false}, // a build after the release
		{"v0.3.2", "v0.4.0", false},
		{"v0.4.0", "dev", false}, // not a version: never out of date
		{"latest", "v0.3.2", false},
	} {
		if got := newerVersion(tc.latest, tc.running); got != tc.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", tc.latest, tc.running, got, tc.want)
		}
	}
}

func TestFetchRelease(t *testing.T) {
	var body string
	var status int
	var ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	status, body = 200, `{"tag_name":"v0.4.0","body":"Fixes.","published_at":"2026-10-05T06:15:28Z","html_url":"https://example.net/r/v0.4.0","draft":false,"prerelease":false}`
	r, err := fetchRelease(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != "v0.4.0" || r.Notes != "Fixes." || r.URL != "https://example.net/r/v0.4.0" || r.Published.IsZero() {
		t.Fatalf("release = %+v", r)
	}
	if !strings.HasPrefix(ua, appName+"/") {
		t.Errorf("User-Agent = %q", ua)
	}

	status, body = 200, `{"tag_name":"v0.5.0-rc1","prerelease":true}`
	if _, err := fetchRelease(context.Background(), srv.URL); err == nil {
		t.Error("a pre-release was accepted")
	}
	status, body = 404, `{}`
	if _, err := fetchRelease(context.Background(), srv.URL); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("HTTP 404: err = %v", err)
	}
}

func TestUpdater(t *testing.T) {
	old := version
	version = "v0.3.2"
	defer func() { version = old }()

	u := newUpdater(UpdatesConfig{Source: "gitea"})
	var asked string
	u.fetch = func(_ context.Context, url string) (*Release, error) {
		asked = url
		return &Release{Version: "v0.4.0"}, nil
	}
	u.Check(context.Background())
	if asked != updateSources["gitea"].API {
		t.Errorf("asked %q", asked)
	}
	st := u.Status()
	if !st.Available || u.Available() != "v0.4.0" || st.Checked == nil || st.LastOK == nil {
		t.Fatalf("status = %+v", st)
	}
	if st.Arch != "" {
		want := "https://git.redetzke.aero/Redetzke/GHOSTWIRE/releases/download/v0.4.0/GHOSTWIRE-v0.4.0-linux-" + st.Arch
		if st.FileURL != want || !strings.HasSuffix(st.SumsURL, "/v0.4.0/SHA256SUMS") {
			t.Errorf("downloads = %q, %q", st.FileURL, st.SumsURL)
		}
	}

	// A failed check keeps the last good answer and reports the error.
	u.fetch = func(context.Context, string) (*Release, error) { return nil, errors.New("no route to host") }
	u.Check(context.Background())
	if st := u.Status(); st.Error != "no route to host" || st.Latest == nil {
		t.Errorf("after a failed check: %+v", st)
	}

	// Another source forgets what the old one said; switching off hides it.
	u.Set(UpdatesConfig{Source: "github"})
	if st := u.Status(); st.Latest != nil || st.Error != "" || st.SourceURL != updateSources["github"].Repo {
		t.Errorf("after changing the source: %+v", st)
	}
	off := false
	u.fetch = func(context.Context, string) (*Release, error) { return &Release{Version: "v0.4.0"}, nil }
	u.Check(context.Background())
	u.Set(UpdatesConfig{Source: "github", Check: &off})
	if u.Available() != "" || u.Status().Enabled {
		t.Error("still reports an update with the check off")
	}
}
