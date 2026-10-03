package main

import (
	"log/slog"
	"sync"
	"time"
)

// PeerSample is one reading of a peer's kernel counters.
type PeerSample struct {
	PublicKey     string
	RxBytes       int64 // received by the server = uploaded by the peer
	TxBytes       int64 // sent by the server = downloaded by the peer
	LastHandshake time.Time
	Endpoint      string
}

// Check is one line of the health report.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Kernel applies the desired state to the system. The Linux implementation
// uses netlink, wgctrl and nftables; other platforms get a simulator so the
// web UI can be developed without a Linux box.
type Kernel interface {
	Apply(c *Config) error
	Sample(iface string) ([]PeerSample, error)
	Checks(c *Config) []Check
	Uplink(c *Config, v6 bool) string
	Down(c *Config) error
	Close() error
}

// Reconciler applies the config to the kernel whenever it is triggered and
// remembers the outcome for the health report.
type Reconciler struct {
	kernel  Kernel
	store   *Store
	trigger chan struct{}

	mu        sync.Mutex
	lastErr   error
	lastApply time.Time
}

func newReconciler(k Kernel, s *Store) *Reconciler {
	return &Reconciler{kernel: k, store: s, trigger: make(chan struct{}, 1)}
}

// Kick schedules an apply; several kicks in a row collapse into one.
func (r *Reconciler) Kick() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// ApplyNow applies synchronously and returns the result, so an API call can
// report kernel errors to the user.
func (r *Reconciler) ApplyNow() error {
	err := r.kernel.Apply(r.store.Get())
	r.mu.Lock()
	r.lastErr, r.lastApply = err, time.Now()
	r.mu.Unlock()
	if err != nil {
		slog.Error("apply failed", "err", err)
	} else {
		slog.Debug("config applied to kernel")
	}
	return err
}

// Run applies on every kick and re-applies every 5 minutes, which repairs
// drift such as a flushed nftables ruleset or a deleted interface.
func (r *Reconciler) Run(stop <-chan struct{}) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-r.trigger:
		case <-t.C:
		}
		_ = r.ApplyNow()
	}
}

func (r *Reconciler) Status() (time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastApply, r.lastErr
}
