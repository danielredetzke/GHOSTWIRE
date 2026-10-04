package main

import (
	"log/slog"
	"net/netip"
	"slices"
	"time"
)

// Latency is measured by pinging a peer's tunnel address from the server:
// WireGuard itself reports no round-trip time. A ping to an idle device
// wakes it and starts new handshakes, which would keep it "online" for good,
// so the "active" mode pings only while the device sends traffic of its own.

const (
	latencyOff    = ""
	latencyActive = "active"
	latencyAlways = "always"

	pingInterval  = 30 * time.Second
	pingTimeout   = 2 * time.Second
	latencyStep   = 5 * time.Minute // one point of the latency history
	latencyKeep   = 24 * time.Hour
	latencyWindow = 5 * time.Minute // the current value is the median of this window
	activeWindow  = 2 * time.Minute
	// activeRxBytes is what a peer must send in one sample interval to count
	// as active. Ping replies (~128 bytes) and keepalives stay well below it.
	activeRxBytes = 1024
)

func validLatencyCheck(m string) bool {
	return m == latencyOff || m == latencyActive || m == latencyAlways
}

// latBucket is one step of a peer's latency history, in milliseconds. The
// single round trips are kept only while the step is open; a closed step
// keeps its summary.
type latBucket struct {
	T    int64     `json:"t"`
	Sent int       `json:"sent"`
	Lost int       `json:"lost"`
	Min  float64   `json:"min,omitempty"`
	Med  float64   `json:"med,omitempty"`
	Max  float64   `json:"max,omitempty"`
	RTTs []float64 `json:"rtts,omitempty"`
}

// summary returns the bucket with Min, Med and Max filled in and no RTTs.
func (b latBucket) summary() latBucket {
	if len(b.RTTs) > 0 {
		b.Min, b.Med, b.Max = spread(b.RTTs)
	}
	b.RTTs = nil
	return b
}

// spread returns the minimum, median and maximum of v (not empty).
func spread(v []float64) (lo, med, hi float64) {
	s := slices.Clone(v)
	slices.Sort(s)
	n := len(s)
	med = s[n/2]
	if n%2 == 0 {
		med = (s[n/2-1] + s[n/2]) / 2
	}
	return s[0], med, s[n-1]
}

type latSample struct {
	t  time.Time
	ms float64
	ok bool
}

// latLive is the part of the latency state that is not saved.
type latLive struct {
	recent     []latSample // pings of the last latencyWindow
	lastActive time.Time   // last sample interval with traffic from the peer
}

func (s *Stats) liveFor(id string) *latLive {
	l := s.live[id]
	if l == nil {
		l = &latLive{}
		s.live[id] = l
	}
	return l
}

// record adds one ping result. s.mu must be held.
func (s *Stats) record(id string, now time.Time, rtt time.Duration, ok bool) {
	ps := s.data.Peers[id]
	if ps == nil {
		ps = &peerStats{}
		s.data.Peers[id] = ps
	}
	ms := float64(rtt.Microseconds()) / 1000
	t := now.Truncate(latencyStep).Unix()
	if n := len(ps.Latency); n == 0 || ps.Latency[n-1].T != t {
		if n > 0 {
			ps.Latency[n-1] = ps.Latency[n-1].summary()
		}
		ps.Latency = append(ps.Latency, latBucket{T: t})
	}
	b := &ps.Latency[len(ps.Latency)-1]
	b.Sent++
	if ok {
		b.RTTs = append(b.RTTs, ms)
	} else {
		b.Lost++
	}
	l := s.liveFor(id)
	i := 0
	for i < len(l.recent) && now.Sub(l.recent[i].t) >= latencyWindow {
		i++
	}
	l.recent = append(l.recent[i:], latSample{t: now, ms: ms, ok: ok})
	s.dirty = true
}

// pingTargets returns the tunnel addresses to ping now, by peer ID.
func (s *Stats) pingTargets(c *Config, now time.Time) map[netip.Addr]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[netip.Addr]string{}
	for _, p := range c.Peers {
		if !p.Enabled || !p.hasKey() || p.LatencyCheck == latencyOff {
			continue
		}
		ps := s.data.Peers[p.ID]
		if ps == nil || ps.LastHandshake.IsZero() {
			continue // never connected: the kernel has no endpoint to send to
		}
		if p.LatencyCheck == latencyActive && now.Sub(s.liveFor(p.ID).lastActive) > activeWindow {
			continue
		}
		if a, err := netip.ParseAddr(p.IPv4); err == nil {
			out[a] = p.ID
		}
	}
	return out
}

func (s *Stats) pingRound() {
	targets := s.pingTargets(s.store.Get(), time.Now())
	if len(targets) == 0 {
		return
	}
	dsts := make([]netip.Addr, 0, len(targets))
	for a := range targets {
		dsts = append(dsts, a)
	}
	start := time.Now()
	rtts, err := s.kernel.Ping(dsts, pingTimeout)
	s.mu.Lock()
	defer s.mu.Unlock()
	if (err == nil) != (s.pingErr == nil) {
		if err != nil {
			slog.Warn("latency check failed", "err", err)
		} else {
			slog.Info("latency check works again")
		}
	}
	s.pingErr = err
	if err != nil {
		return
	}
	for a, id := range targets {
		rtt, ok := rtts[a]
		s.record(id, start, rtt, ok)
	}
}

// RunPings runs the latency checks until stop is closed.
func (s *Stats) RunPings(stop <-chan struct{}) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.pingRound()
		}
	}
}

// PingCheck is the health line for the latency check; ok is false when no
// peer has the check turned on.
func (s *Stats) PingCheck(c *Config) (Check, bool) {
	on := slices.ContainsFunc(c.Peers, func(p Peer) bool { return p.Enabled && p.LatencyCheck != latencyOff })
	if !on {
		return Check{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pingErr != nil {
		return Check{Name: "Latency check", OK: false, Detail: s.pingErr.Error()}, true
	}
	return Check{Name: "Latency check", OK: true, Detail: "ping through the tunnel works"}, true
}

// LatencyView is a peer's current latency, in milliseconds.
type LatencyView struct {
	MS    *float64   `json:"ms"` // median; null = no reply
	Min   float64    `json:"min"`
	Max   float64    `json:"max"`
	Loss  int        `json:"loss"`  // percent of pings without a reply
	At    time.Time  `json:"at"`    // newest ping
	Spark []*float64 `json:"spark"` // medians of the last hour, oldest first; null = no reply
}

// latencyView returns the current latency, or nil when the peer was never
// pinged. s.mu must be held.
func (s *Stats) latencyView(id string, ps *peerStats, now time.Time) *LatencyView {
	if len(ps.Latency) == 0 {
		return nil
	}
	v := &LatencyView{}
	var rtts []float64
	var sent, lost int
	if l := s.live[id]; l != nil && len(l.recent) > 0 {
		v.At = l.recent[len(l.recent)-1].t
		for _, x := range l.recent {
			sent++
			if x.ok {
				rtts = append(rtts, x.ms)
			} else {
				lost++
			}
		}
	} else {
		// Nothing pinged since the start: show the newest saved step.
		b := ps.Latency[len(ps.Latency)-1]
		v.At = time.Unix(b.T, 0).Add(latencyStep)
		if v.At.After(now) {
			v.At = now
		}
		rtts, sent, lost = b.RTTs, b.Sent, b.Lost
		if len(rtts) == 0 && b.Sent > b.Lost {
			rtts = []float64{b.Min, b.Med, b.Max}
		}
	}
	if len(rtts) > 0 {
		var med float64
		v.Min, med, v.Max = spread(rtts)
		v.MS = &med
	}
	if sent > 0 {
		v.Loss = lost * 100 / sent
	}
	for _, b := range s.latencySeries(ps, now, 12) {
		if b.Sent > b.Lost {
			m := b.Med
			v.Spark = append(v.Spark, &m)
		} else {
			v.Spark = append(v.Spark, nil)
		}
	}
	return v
}

// latencySeries returns n steps ending with the current one; steps without
// pings have Sent 0. s.mu must be held.
func (s *Stats) latencySeries(ps *peerStats, now time.Time, n int) []latBucket {
	last := now.Truncate(latencyStep)
	out := make([]latBucket, n)
	idx := map[int64]int{}
	for i := range out {
		t := last.Add(-time.Duration(n-1-i) * latencyStep).Unix()
		out[i].T = t
		idx[t] = i
	}
	for _, b := range ps.Latency {
		if i, ok := idx[b.T]; ok {
			out[i] = b.summary()
		}
	}
	return out
}

// LatencyHistory returns the latency of the last 24 hours, one point per step.
func (s *Stats) LatencyHistory(id string) []latBucket {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := s.data.Peers[id]
	if ps == nil {
		ps = &peerStats{}
	}
	return s.latencySeries(ps, time.Now(), int(latencyKeep/latencyStep))
}

// pruneLatency drops history older than latencyKeep. s.mu must be held.
func pruneLatency(ps *peerStats, now time.Time) bool {
	cut := now.Add(-latencyKeep).Truncate(latencyStep).Unix()
	i := 0
	for i < len(ps.Latency) && ps.Latency[i].T < cut {
		i++
	}
	if i == 0 {
		return false
	}
	ps.Latency = append([]latBucket(nil), ps.Latency[i:]...)
	return true
}
