package main

import (
	"log/slog"
	"sync"
	"time"
)

// Speeds keeps the last few minutes of each peer's speed in memory for the
// Live page. It reads the kernel counters every speedStep, apart from the
// traffic history in Stats, and never writes to disk.

const (
	speedStep   = 2 * time.Second
	speedPoints = 60 // 2 minutes
)

// SpeedPoint is one step: per peer ID, download and upload in bits per
// second, from the peer's point of view.
type SpeedPoint struct {
	T     int64               `json:"t"`
	Peers map[string][2]int64 `json:"peers"`
}

type Speeds struct {
	store  *Store
	kernel Kernel

	mu     sync.Mutex
	last   map[string][2]int64 // raw rx, tx by public key
	lastAt time.Time
	points []SpeedPoint
	subs   map[chan SpeedPoint]struct{}
	done   chan struct{} // closed when Run returns
}

func newSpeeds(store *Store, kernel Kernel) *Speeds {
	return &Speeds{store: store, kernel: kernel, last: map[string][2]int64{}, subs: map[chan SpeedPoint]struct{}{}, done: make(chan struct{})}
}

// Subscribe returns the current points and a channel that receives each new
// one; cancel ends the subscription. A subscriber that falls behind misses
// points rather than holding up the sampler.
func (s *Speeds) Subscribe() (points []SpeedPoint, ch <-chan SpeedPoint, cancel func()) {
	c := make(chan SpeedPoint, 4)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs[c] = struct{}{}
	return append([]SpeedPoint{}, s.points...), c, func() {
		s.mu.Lock()
		delete(s.subs, c)
		s.mu.Unlock()
	}
}

// Done is closed when the sampler stops, so streams can end.
func (s *Speeds) Done() <-chan struct{} { return s.done }

func (s *Speeds) sample(now time.Time) {
	cfg := s.store.Get()
	samples, err := s.kernel.Sample(cfg.Server.Interface)
	if err != nil {
		slog.Debug("speed sample failed", "err", err)
		return
	}
	idByKey := map[string]string{}
	for _, p := range cfg.Peers {
		if p.hasKey() {
			idByKey[p.PublicKey] = p.ID
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	secs := now.Sub(s.lastAt).Seconds()
	first := s.lastAt.IsZero()
	cur := map[string][2]int64{}
	pt := SpeedPoint{T: now.Unix(), Peers: map[string][2]int64{}}
	for _, smp := range samples {
		cur[smp.PublicKey] = [2]int64{smp.RxBytes, smp.TxBytes}
		id := idByKey[smp.PublicKey]
		prev, ok := s.last[smp.PublicKey]
		if id == "" || !ok || first {
			continue
		}
		dRx, dTx := smp.RxBytes-prev[0], smp.TxBytes-prev[1]
		if dRx < 0 || dTx < 0 { // counters were reset
			continue
		}
		// Tx is what the server sent: the peer's download.
		pt.Peers[id] = [2]int64{int64(float64(dTx*8) / secs), int64(float64(dRx*8) / secs)}
	}
	s.last, s.lastAt = cur, now
	if first {
		return
	}
	s.points = append(s.points, pt)
	if len(s.points) > speedPoints {
		s.points = s.points[len(s.points)-speedPoints:]
	}
	for c := range s.subs {
		select {
		case c <- pt:
		default:
		}
	}
}

// Since returns the points newer than the unix time t, oldest first.
func (s *Speeds) Since(t int64) []SpeedPoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []SpeedPoint{}
	for _, p := range s.points {
		if p.T > t {
			out = append(out, p)
		}
	}
	return out
}

func (s *Speeds) Run(stop <-chan struct{}) {
	defer close(s.done)
	s.sample(time.Now())
	t := time.NewTicker(speedStep)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			s.sample(now)
		}
	}
}
