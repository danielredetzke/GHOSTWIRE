package main

import (
	"log/slog"
	"net/netip"
	"os"
	"slices"
	"strings"
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
	// Ping sends one echo request to each address and returns the round-trip
	// times of the replies that came within timeout.
	Ping(dsts []netip.Addr, timeout time.Duration) (map[netip.Addr]time.Duration, error)
	Down(c *Config) error
	Close() error
}

// lanBlock picks, from the networks on the uplinks, the ones peers must not
// reach while LAN access is off: private IPv4 networks, and IPv6 networks
// except link-local, since a home LAN uses global IPv6 addresses. IPv6
// prefixes shorter than /48 are left out: they are no LAN.
func lanBlock(nets []netip.Prefix) []netip.Prefix {
	var out []netip.Prefix
	for _, p := range nets {
		a := p.Addr().Unmap()
		p = netip.PrefixFrom(a, min(p.Bits(), a.BitLen())).Masked()
		switch {
		case a.Is4() && !a.IsPrivate():
			continue
		case a.Is6() && (a.IsLinkLocalUnicast() || a.IsLoopback() || p.Bits() < 48):
			continue
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// readSysctl returns the trimmed content of a /proc/sys file, or "".
func readSysctl(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Reconciler applies the config to the kernel whenever it is triggered and
// remembers the outcome for the health report.
type Reconciler struct {
	kernel  Kernel
	store   *Store
	trigger chan struct{}

	// applyMu runs one apply at a time. Each reads the config once it holds
	// the lock, so the last apply always uses the newest config.
	applyMu sync.Mutex

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
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
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
