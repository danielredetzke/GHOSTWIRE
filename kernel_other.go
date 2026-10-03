//go:build !linux

package main

import (
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"
)

// simKernel stands in for the Linux kernel on other platforms. It does not
// touch the system; it invents traffic for enabled peers so the web UI has
// data during development.
type simKernel struct {
	mu    sync.Mutex
	peers map[string]*PeerSample
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
	var out []PeerSample
	i := 0
	for _, p := range k.peers {
		// Every third peer stays idle; the others move some data.
		if i%3 != 2 {
			p.TxBytes += rand.Int64N(40 << 20)
			p.RxBytes += rand.Int64N(6 << 20)
			p.LastHandshake = time.Now().Add(-time.Duration(rand.IntN(90)) * time.Second)
		}
		out = append(out, *p)
		i++
	}
	return out, nil
}

func (k *simKernel) Checks(c *Config) []Check {
	return []Check{
		// Same wording as on Linux, so screenshots look like a real server.
		{"WireGuard interface", true, c.Server.Interface + " is up"},
		{"IPv4 forwarding", true, "net.ipv4.ip_forward=1"},
		{"nftables rules", true, "table inet " + appName + " present"},
		{"Uplink", true, "IPv4 via eth0"},
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
