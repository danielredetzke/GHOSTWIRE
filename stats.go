package main

import (
	"encoding/json"
	"errors"
	"log/slog"
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
	LastRx        int64     `json:"lastRx"` // last raw counter values
	LastTx        int64     `json:"lastTx"`
	TotalRx       int64     `json:"totalRx"`
	TotalTx       int64     `json:"totalTx"`
	LastHandshake time.Time `json:"lastHandshake"`
	Endpoint      string    `json:"endpoint"`
	Hourly        []bucket  `json:"hourly"`
	Daily         []bucket  `json:"daily"`
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
		h, dl := len(ps.Hourly), len(ps.Daily)
		ps.Hourly = dropBefore(ps.Hourly, hourCut)
		ps.Daily = dropBefore(ps.Daily, dayCut)
		if len(ps.Hourly) != h || len(ps.Daily) != dl {
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
		s.dirty = true
	}
	for id := range s.data.Peers {
		if !exists[id] {
			delete(s.data.Peers, id)
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
