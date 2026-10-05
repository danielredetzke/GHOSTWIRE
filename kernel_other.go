//go:build !linux

package main

import (
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net/netip"
	"slices"
	"sync"
	"time"
)

// simKernel stands in for the Linux kernel on other platforms. It does not
// touch the system; it invents traffic for enabled peers so the web UI has
// data during development.
type simKernel struct {
	mu    sync.Mutex
	peers map[string]*PeerSample
	last  time.Time // previous Sample; traffic grows with the time since
}

func newKernel() (Kernel, error) {
	slog.Warn("not running on Linux: using the traffic simulator, no WireGuard interface is created")
	return &simKernel{peers: map[string]*PeerSample{}}, nil
}

func (k *simKernel) Close() error { return nil }

func (k *simKernel) Apply(c *Config) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	keep := map[string]bool{}
	for i, p := range c.Peers {
		if !p.Enabled || !p.hasKey() {
			continue
		}
		keep[p.PublicKey] = true
		if k.peers[p.PublicKey] == nil {
			k.peers[p.PublicKey] = &PeerSample{
				PublicKey: p.PublicKey,
				Endpoint:  fmt.Sprintf("198.51.100.%d:%d", 10+i, 40000+i*7),
			}
		}
	}
	for key := range k.peers {
		if !keep[key] {
			delete(k.peers, key)
		}
	}
	return nil
}

func (k *simKernel) Sample(string) ([]PeerSample, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	f := 1.0
	if !k.last.IsZero() {
		f = now.Sub(k.last).Seconds() / 30
	}
	k.last = now
	var out []PeerSample
	for i, key := range slices.Sorted(maps.Keys(k.peers)) {
		p := k.peers[key]
		// Every third peer stays idle; the others move some data, scaled to
		// the time since the previous sample.
		if i%3 != 2 {
			p.TxBytes += int64(float64(rand.Int64N(40<<20)) * f)
			p.RxBytes += int64(float64(rand.Int64N(6<<20)) * f)
			p.LastHandshake = now.Add(-time.Duration(rand.IntN(90)) * time.Second)
		}
		out = append(out, *p)
	}
	return out, nil
}

func (k *simKernel) Checks(c *Config) []Check {
	return []Check{
		// Same wording as on Linux, so screenshots look like a real server.
		{"WireGuard interface", true, c.Server.Interface + " is up"},
		{"IPv4 forwarding", true, "net.ipv4.ip_forward=1"},
		{"nftables rules", true, "table inet " + appName + " present"},
		{"IPv4 uplink", true, "via eth0"},
		{"Public IPv4", true, "203.0.113.10"},
	}
}

func (k *simKernel) Uplink(c *Config, v6 bool) string {
	if v6 && c.Server.UplinkV6 != "" {
		return c.Server.UplinkV6
	}
	if !v6 && c.Server.UplinkV4 != "" {
		return c.Server.UplinkV4
	}
	return "eth0"
}

func (k *simKernel) Down(*Config) error { return nil }

// Ping invents round trips from the address, so each peer keeps its own
// typical latency. Every seventh address never answers, like a Windows PC.
func (k *simKernel) Ping(dsts []netip.Addr, _ time.Duration) (map[netip.Addr]time.Duration, error) {
	out := map[netip.Addr]time.Duration{}
	for _, d := range dsts {
		last := int(d.As4()[3])
		if last%7 == 4 {
			continue
		}
		base := 8 + last*37%180
		out[d] = time.Duration(base*1000+rand.IntN(base*300+1)) * time.Microsecond
	}
	return out, nil
}
