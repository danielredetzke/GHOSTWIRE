package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"
)

// Traffic is counted as deltas between samples of the kernel counters, so a
// counter reset (interface re-created, peer re-enabled, new key) never makes
// numbers jump backwards. Counters are from the server's view: Rx is what the
// server received = the peer's upload, Tx is the peer's download.

const (
	sampleInterval = 30 * time.Second
	saveInterval   = 5 * time.Minute
	onlineWindow   = 3 * time.Minute
)

type bucket struct {
	T  int64 `json:"t"` // unix start of the hour or local day
	Rx int64 `json:"rx"`
	Tx int64 `json:"tx"`
}

type peerStats struct {
	LastRx        int64         `json:"lastRx"` // last raw counter values
	LastTx        int64         `json:"lastTx"`
	TotalRx       int64         `json:"totalRx"`
	TotalTx       int64         `json:"totalTx"`
	LastHandshake time.Time     `json:"lastHandshake"`
	Endpoint      string        `json:"endpoint"`
	Hourly        []bucket      `json:"hourly"`
	Daily         []bucket      `json:"daily"`
	Sessions      []connSession `json:"sessions,omitempty"`
}

// session is one stretch of a peer being online from one address. When the
// device changes networks (Wi-Fi to mobile), a new session starts.
type connSession struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"` // last time the peer was seen online
	Open     bool      `json:"open,omitempty"`
	Endpoint string    `json:"endpoint"`
	Rx       int64     `json:"rx"`
	Tx       int64     `json:"tx"`
	Geo      *GeoInfo  `json:"geo,omitempty"` // looked up when the session started
}

const maxSessions = 1000 // per peer, besides the retention window

func (ps *peerStats) openSession() *connSession {
	if n := len(ps.Sessions); n > 0 && ps.Sessions[n-1].Open {
		return &ps.Sessions[n-1]
	}
	return nil
}

func hostOf(endpoint string) string {
	if h, _, err := net.SplitHostPort(endpoint); err == nil {
		return h
	}
	return endpoint
}

// track updates the session list after a sample. Online means a handshake
// within the online window, as in the peer lists.
func (s *Stats) track(ps *peerStats, smp PeerSample, rx, tx int64, now time.Time) {
	cur := ps.openSession()
	online := !smp.LastHandshake.IsZero() && now.Sub(smp.LastHandshake) < onlineWindow
	if !online {
		if cur != nil {
			cur.Open = false
		}
		return
	}
	if cur != nil && smp.Endpoint != "" && hostOf(cur.Endpoint) != hostOf(smp.Endpoint) {
		cur.Open = false // roamed to another network
		cur = nil
	}
	if cur == nil {
		start := smp.LastHandshake
		if start.After(now) {
			start = now
		}
		ps.Sessions = append(ps.Sessions, connSession{Start: start, Open: true, Endpoint: smp.Endpoint, Geo: s.geo.Lookup(smp.Endpoint)})
		if len(ps.Sessions) > maxSessions {
			ps.Sessions = append([]connSession(nil), ps.Sessions[len(ps.Sessions)-maxSessions:]...)
		}
		cur = &ps.Sessions[len(ps.Sessions)-1]
	}
	if smp.Endpoint != "" {
		cur.Endpoint = smp.Endpoint // same network, the port may change
	}
	cur.End = now
	cur.Rx += rx
	cur.Tx += tx
}

type statsFile struct {
	Version int                   `json:"version"`
	Peers   map[string]*peerStats `json:"peers"` // by peer ID
}

type Stats struct {
	mu     sync.Mutex
	path   string
	data   statsFile
	dirty  bool
	store  *Store
	kernel Kernel
	geo    *Geo // nil: no country and network lookups
}

func openStats(path string, store *Store, k Kernel) (*Stats, error) {
	s := &Stats{path: path, store: store, kernel: k, data: statsFile{Version: 1, Peers: map[string]*peerStats{}}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		slog.Error("stats file is damaged, starting fresh", "path", path, "err", err)
		s.data = statsFile{Version: 1, Peers: map[string]*peerStats{}}
	}
	if s.data.Peers == nil {
		s.data.Peers = map[string]*peerStats{}
	}
	return s, nil
}

func hourStart(t time.Time) int64 { return t.Truncate(time.Hour).Unix() }

func dayStart(t time.Time) int64 {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location()).Unix()
}

func addTo(list []bucket, start, rx, tx int64) []bucket {
	if n := len(list); n > 0 && list[n-1].T == start {
		list[n-1].Rx += rx
		list[n-1].Tx += tx
		return list
	}
	return append(list, bucket{T: start, Rx: rx, Tx: tx})
}

// dropBefore removes buckets that start before cutoff. Lists are in time
// order, so only a prefix is removed.
func dropBefore(list []bucket, cutoff int64) []bucket {
	i := 0
	for i < len(list) && list[i].T < cutoff {
		i++
	}
	if i == 0 {
		return list
	}
	return append([]bucket(nil), list[i:]...)
}

// prune applies the retention settings to every peer's history; the oldest
// kept bucket is the one that holds the start of the retention window.
func (s *Stats) prune(c StatsConfig, now time.Time) {
	hourCut := hourStart(now.Add(-time.Duration(c.HourlyHours-1) * time.Hour))
	y, m, d := now.Date()
	dayCut := time.Date(y, m, d-(c.DailyDays-1), 0, 0, 0, 0, now.Location()).Unix()
	for _, ps := range s.data.Peers {
		h, dl, sl := len(ps.Hourly), len(ps.Daily), len(ps.Sessions)
		ps.Hourly = dropBefore(ps.Hourly, hourCut)
		ps.Daily = dropBefore(ps.Daily, dayCut)
		// Connection history is kept as long as the daily traffic history.
		i := 0
		for i < len(ps.Sessions) && !ps.Sessions[i].Open && ps.Sessions[i].End.Unix() < dayCut {
			i++
		}
		if i > 0 {
			ps.Sessions = append([]connSession(nil), ps.Sessions[i:]...)
		}
		if len(ps.Hourly) != h || len(ps.Daily) != dl || len(ps.Sessions) != sl {
			s.dirty = true
		}
	}
}

func (s *Stats) sample() {
	cfg := s.store.Get()
	samples, err := s.kernel.Sample(cfg.Server.Interface)
	if err != nil {
		slog.Debug("stats sample failed", "err", err)
		return
	}
	idByKey := map[string]string{}
	exists := map[string]bool{}
	for _, p := range cfg.Peers {
		idByKey[p.PublicKey] = p.ID
		exists[p.ID] = true
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for _, smp := range samples {
		id := idByKey[smp.PublicKey]
		if id == "" {
			continue
		}
		ps := s.data.Peers[id]
		if ps == nil {
			ps = &peerStats{}
			s.data.Peers[id] = ps
		}
		seen[id] = true
		dRx, dTx := smp.RxBytes-ps.LastRx, smp.TxBytes-ps.LastTx
		if dRx < 0 || dTx < 0 { // counters were reset
			dRx, dTx = smp.RxBytes, smp.TxBytes
		}
		ps.LastRx, ps.LastTx = smp.RxBytes, smp.TxBytes
		if dRx > 0 || dTx > 0 {
			ps.TotalRx += dRx
			ps.TotalTx += dTx
			ps.Hourly = addTo(ps.Hourly, hourStart(now), dRx, dTx)
			ps.Daily = addTo(ps.Daily, dayStart(now), dRx, dTx)
		}
		if !smp.LastHandshake.IsZero() {
			ps.LastHandshake = smp.LastHandshake
		}
		if smp.Endpoint != "" {
			ps.Endpoint = smp.Endpoint
		}
		s.track(ps, smp, dRx, dTx, now)
		s.dirty = true
	}
	for id, ps := range s.data.Peers {
		if !exists[id] {
			delete(s.data.Peers, id)
			s.dirty = true
			continue
		}
		// Disabled peers are not in the kernel any more: end their session.
		if cur := ps.openSession(); cur != nil && !seen[id] {
			cur.Open = false
			s.dirty = true
		}
	}
	s.prune(cfg.Stats, now)
}

func (s *Stats) save() {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	b, _ := json.Marshal(s.data)
	s.dirty = false
	s.mu.Unlock()
	var raw json.RawMessage = b
	if err := writeFileAtomic(s.path, raw, 0o600); err != nil {
		slog.Error("saving stats failed", "err", err)
	}
}

func (s *Stats) Run(stop <-chan struct{}) {
	s.sample()
	st := time.NewTicker(sampleInterval)
	sv := time.NewTicker(saveInterval)
	defer st.Stop()
	defer sv.Stop()
	for {
		select {
		case <-stop:
			s.sample()
			s.save()
			return
		case <-st.C:
			s.sample()
		case <-sv.C:
			s.save()
		}
	}
}

// Point is one bar of a traffic chart, from the peer's point of view.
type Point struct {
	T    int64 `json:"t"`
	Down int64 `json:"down"`
	Up   int64 `json:"up"`
}

// series returns n points ending with the current hour or day, filling gaps
// with zeros. ids selects peers; nil means all peers.
func (s *Stats) series(ids []string, rng string) []Point {
	now := time.Now()
	var starts []int64
	daily := rng != "24h"
	n := map[string]int{"24h": 24, "7d": 7, "30d": 30, "90d": 90}[rng]
	if n == 0 {
		n, daily = 24, false
	}
	for i := n - 1; i >= 0; i-- {
		if daily {
			y, m, d := now.Date()
			starts = append(starts, time.Date(y, m, d-i, 0, 0, 0, 0, now.Location()).Unix())
		} else {
			starts = append(starts, now.Truncate(time.Hour).Add(-time.Duration(i)*time.Hour).Unix())
		}
	}
	idx := map[int64]int{}
	pts := make([]Point, n)
	for i, t := range starts {
		pts[i].T = t
		idx[t] = i
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	add := func(ps *peerStats) {
		list := ps.Hourly
		if daily {
			list = ps.Daily
		}
		for _, b := range list {
			if i, ok := idx[b.T]; ok {
				pts[i].Down += b.Tx
				pts[i].Up += b.Rx
			}
		}
	}
	if ids == nil {
		for _, ps := range s.data.Peers {
			add(ps)
		}
	} else {
		for _, id := range ids {
			if ps := s.data.Peers[id]; ps != nil {
				add(ps)
			}
		}
	}
	return pts
}

// PeerSummary is the live state shown in peer lists.
type PeerSummary struct {
	Online        bool       `json:"online"`
	LastHandshake *time.Time `json:"lastHandshake"`
	Endpoint      string     `json:"endpoint"`
	Down24h       int64      `json:"down24h"`
	Up24h         int64      `json:"up24h"`
	Down30d       int64      `json:"down30d"`
	Up30d         int64      `json:"up30d"`
	DownTotal     int64      `json:"downTotal"`
	UpTotal       int64      `json:"upTotal"`
	Location      *GeoInfo   `json:"location"` // of the current or last endpoint
}

func sumPoints(pts []Point) (down, up int64) {
	for _, p := range pts {
		down += p.Down
		up += p.Up
	}
	return
}

func (s *Stats) Summary(id string) PeerSummary {
	var out PeerSummary
	out.Down24h, out.Up24h = sumPoints(s.series([]string{id}, "24h"))
	out.Down30d, out.Up30d = sumPoints(s.series([]string{id}, "30d"))
	s.mu.Lock()
	defer s.mu.Unlock()
	if ps := s.data.Peers[id]; ps != nil {
		out.DownTotal, out.UpTotal = ps.TotalTx, ps.TotalRx
		out.Endpoint = ps.Endpoint
		if !ps.LastHandshake.IsZero() {
			t := ps.LastHandshake
			out.LastHandshake = &t
			out.Online = time.Since(t) < onlineWindow
		}
		if cur := ps.openSession(); cur != nil && cur.Geo != nil && hostOf(cur.Endpoint) == hostOf(ps.Endpoint) {
			out.Location = cur.Geo
		}
	}
	if out.Location == nil && out.Endpoint != "" {
		out.Location = s.geo.Lookup(out.Endpoint)
	}
	return out
}

// SessionView is one row of a peer's connection history, from the peer's
// point of view (down = downloaded by the peer).
type SessionView struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Open     bool      `json:"open"`
	Seconds  int64     `json:"seconds"`
	Endpoint string    `json:"endpoint"`
	IP       string    `json:"ip"`
	Geo      *GeoInfo  `json:"geo"`
	Down     int64     `json:"down"`
	Up       int64     `json:"up"`
}

// Sessions returns up to limit sessions, newest first.
func (s *Stats) Sessions(id string, limit int) []SessionView {
	s.mu.Lock()
	var list []connSession
	if ps := s.data.Peers[id]; ps != nil {
		list = append(list, ps.Sessions...)
	}
	s.mu.Unlock()
	out := []SessionView{}
	for i := len(list) - 1; i >= 0 && len(out) < limit; i-- {
		se := list[i]
		geo := se.Geo
		if geo == nil {
			geo = s.geo.Lookup(se.Endpoint) // database was missing when it started
		}
		out = append(out, SessionView{
			Start: se.Start, End: se.End, Open: se.Open, Seconds: int64(se.End.Sub(se.Start).Seconds()),
			Endpoint: se.Endpoint, IP: hostOf(se.Endpoint), Geo: geo, Down: se.Tx, Up: se.Rx,
		})
	}
	return out
}

// Forget drops a peer's live counters, e.g. after its key changed.
func (s *Stats) Forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ps := s.data.Peers[id]; ps != nil {
		ps.LastRx, ps.LastTx = 0, 0
		ps.Endpoint = ""
		s.dirty = true
	}
}
