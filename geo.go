package main

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oschwald/maxminddb-golang"
)

// Country and network (autonomous system) of peer endpoints come from the
// free DB-IP Lite databases (CC BY 4.0, https://db-ip.com). They are
// downloaded once a month and searched locally, so endpoint addresses never
// leave the server.

// GeoInfo describes where an address is.
type GeoInfo struct {
	Country     string `json:"country,omitempty"`     // ISO code, e.g. "DE"
	CountryName string `json:"countryName,omitempty"` // e.g. "Germany"
	ASN         uint   `json:"asn,omitempty"`
	Network     string `json:"network,omitempty"` // operator, e.g. "Deutsche Telekom AG"
}

const (
	geoMaxAge    = 32 * 24 * time.Hour // DB-IP publishes monthly
	geoCheckFreq = 24 * time.Hour
	geoBaseURL   = "https://download.db-ip.com/free/dbip-%s-lite-%s.mmdb.gz"
)

type Geo struct {
	dir     string
	enabled atomic.Bool
	kick    chan struct{}

	mu      sync.RWMutex
	country *maxminddb.Reader
	asn     *maxminddb.Reader
}

func newGeo(dir string, enabled bool) *Geo {
	g := &Geo{dir: dir, kick: make(chan struct{}, 1)}
	g.enabled.Store(enabled)
	if enabled {
		g.open()
	}
	return g
}

func (g *Geo) path(kind string) string { return filepath.Join(g.dir, "geo-"+kind+".mmdb") }

// open (re)loads whichever database files exist.
func (g *Geo) open() {
	load := func(kind string) *maxminddb.Reader {
		r, err := maxminddb.Open(g.path(kind))
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				slog.Warn("geo database unreadable", "file", g.path(kind), "err", err)
			}
			return nil
		}
		return r
	}
	c, a := load("country"), load("asn")
	g.mu.Lock()
	old := []*maxminddb.Reader{g.country, g.asn}
	g.country, g.asn = c, a
	g.mu.Unlock()
	for _, r := range old {
		if r != nil {
			r.Close()
		}
	}
}

// SetEnabled turns lookups and monthly downloads on or off.
func (g *Geo) SetEnabled(on bool) {
	if g == nil || g.enabled.Swap(on) == on {
		return
	}
	select {
	case g.kick <- struct{}{}:
	default:
	}
}

// Lookup returns where ipPort ("203.0.113.7:51820" or a bare address) is.
// Private addresses are reported as the local network.
func (g *Geo) Lookup(ipPort string) *GeoInfo {
	host := ipPort
	if h, _, err := net.SplitHostPort(ipPort); err == nil {
		host = h
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return nil
	}
	ip = ip.Unmap()
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || isCGNAT(ip) {
		return &GeoInfo{Network: "Local network"}
	}
	if g == nil || !g.enabled.Load() {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	info := GeoInfo{}
	if g.country != nil {
		var rec struct {
			Country struct {
				ISOCode string            `maxminddb:"iso_code"`
				Names   map[string]string `maxminddb:"names"`
			} `maxminddb:"country"`
		}
		if g.country.Lookup(net.IP(ip.AsSlice()), &rec) == nil {
			info.Country, info.CountryName = rec.Country.ISOCode, rec.Country.Names["en"]
		}
	}
	if g.asn != nil {
		var rec struct {
			Number uint   `maxminddb:"autonomous_system_number"`
			Org    string `maxminddb:"autonomous_system_organization"`
		}
		if g.asn.Lookup(net.IP(ip.AsSlice()), &rec) == nil {
			info.ASN, info.Network = rec.Number, rec.Org
		}
	}
	if info == (GeoInfo{}) {
		return nil
	}
	return &info
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func isCGNAT(ip netip.Addr) bool { return cgnat.Contains(ip) }

// GeoStatus is shown in the settings.
type GeoStatus struct {
	Enabled bool       `json:"enabled"`
	Updated *time.Time `json:"updated"` // date of the country database file
}

func (g *Geo) Status() GeoStatus {
	st := GeoStatus{Enabled: g.enabled.Load()}
	if fi, err := os.Stat(g.path("country")); err == nil {
		t := fi.ModTime()
		st.Updated = &t
	}
	return st
}

// Run downloads missing or outdated databases daily while enabled, and
// deletes them when the feature is switched off.
func (g *Geo) Run(stop <-chan struct{}) {
	t := time.NewTicker(geoCheckFreq)
	defer t.Stop()
	for {
		g.maintain()
		select {
		case <-stop:
			return
		case <-t.C:
		case <-g.kick:
		}
	}
}

func (g *Geo) maintain() {
	if !g.enabled.Load() {
		g.mu.Lock()
		for _, r := range []*maxminddb.Reader{g.country, g.asn} {
			if r != nil {
				r.Close()
			}
		}
		g.country, g.asn = nil, nil
		g.mu.Unlock()
		for _, kind := range []string{"country", "asn"} {
			_ = os.Remove(g.path(kind))
		}
		return
	}
	changed := false
	for _, kind := range []string{"country", "asn"} {
		if fi, err := os.Stat(g.path(kind)); err == nil && time.Since(fi.ModTime()) < geoMaxAge {
			continue
		}
		if err := g.download(kind); err != nil {
			slog.Warn("geo database download failed", "db", kind, "err", err)
			continue
		}
		changed = true
		slog.Info("geo database updated", "db", kind)
	}
	if changed {
		g.open()
	}
}

// download fetches this month's database, or last month's if this month's
// is not published yet, and replaces the local file atomically.
func (g *Geo) download(kind string) error {
	now := time.Now().UTC()
	var lastErr error
	for _, month := range []time.Time{now, now.AddDate(0, -1, 0)} {
		url := fmt.Sprintf(geoBaseURL, kind, month.Format("2006-01"))
		if lastErr = g.fetch(url, g.path(kind)); lastErr == nil {
			return nil
		}
	}
	return lastErr
}

func (g *Geo) fetch(url, dest string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".geo-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, io.LimitReader(zr, 512<<20)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Refuse a file that is not a readable database.
	r, err := maxminddb.Open(tmp.Name())
	if err != nil {
		return fmt.Errorf("downloaded file is not a valid database: %w", err)
	}
	r.Close()
	return os.Rename(tmp.Name(), dest)
}

func (g *Geo) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, r := range []*maxminddb.Reader{g.country, g.asn} {
		if r != nil {
			r.Close()
		}
	}
}
