package main

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// visitorView tells the dashboard whether the browser asking is behind the
// VPN. Behind it, websites see the server's address, so the card shows the
// visitor's address next to the server's: the same means protected.
type visitorView struct {
	Protected bool     `json:"protected"`
	IP        string   `json:"ip"`       // what websites see; the server's address when protected
	ServerIP  string   `json:"serverIP"` // empty when the endpoint does not resolve
	Peer      *peerRef `json:"peer"`     // the peer whose tunnel the request came through
	Location  *GeoInfo `json:"location"` // of IP, when not protected
}

type peerRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// classifyVisitor decides on the address the server sees (remoteIP).
// Through the tunnel that is the peer's tunnel address; the peer is
// protected when its AllowedIPs send all traffic of that family through
// the server. A server behind NAT may instead see its own or its public
// address, when the request loops back through the router.
func classifyVisitor(c *Config, ip, server netip.Addr, own func(netip.Addr) bool) visitorView {
	v := visitorView{IP: ip.String()}
	if server.IsValid() {
		v.ServerIP = server.String()
	}
	if p := peerByTunnelAddr(c, ip); p != nil {
		v.Peer = &peerRef{ID: p.ID, Name: p.Name}
		v.Protected = fullTunnel(peerAllowedIPs(c, p), ip.Is6())
	} else if !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && (ip == server || own(ip)) {
		v.Protected = true
	}
	if v.Protected && server.IsValid() {
		v.IP = v.ServerIP
	}
	return v
}

func peerByTunnelAddr(c *Config, ip netip.Addr) *Peer {
	for i := range c.Peers {
		p := &c.Peers[i]
		if !p.Enabled {
			continue
		}
		for _, a := range peerAddresses(c, p) {
			if a.Addr() == ip {
				return p
			}
		}
	}
	return nil
}

// fullTunnel reports whether AllowedIPs route all IPv4 (or IPv6) traffic
// into the tunnel: 0.0.0.0/0, ::/0, or the two halves some clients use.
func fullTunnel(allowed []string, v6 bool) bool {
	halves := 0
	for _, s := range allowed {
		p, err := netip.ParsePrefix(s)
		if err != nil || p.Addr().Is6() != v6 {
			continue
		}
		switch p.Bits() {
		case 0:
			return true
		case 1:
			halves++
		}
	}
	return halves >= 2
}

// ownAddr reports whether ip is an address of this machine.
func ownAddr(ip netip.Addr) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if x, ok := netip.AddrFromSlice(n.IP); ok && x.Unmap() == ip {
				return true
			}
		}
	}
	return false
}

// endpointIPs resolves the WireGuard endpoint, the address websites see
// behind the VPN, and keeps the answer for a few minutes.
type endpointIPs struct {
	mu    sync.Mutex
	host  string
	addrs []netip.Addr
	at    time.Time
}

func (e *endpointIPs) lookup(host string, v6 bool) netip.Addr {
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if host != e.host || time.Since(e.at) > 10*time.Minute {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		addrs, _ := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		cancel()
		e.host, e.addrs, e.at = host, addrs, time.Now()
	}
	var other netip.Addr
	for _, a := range e.addrs {
		a = a.Unmap()
		if a.Is6() == v6 {
			return a
		}
		if !other.IsValid() {
			other = a
		}
	}
	return other
}

func (a *App) visitor(r *http.Request, c *Config) visitorView {
	ip, err := netip.ParseAddr(remoteIP(r))
	if err != nil {
		return visitorView{IP: remoteIP(r)}
	}
	ip = ip.Unmap()
	v := classifyVisitor(c, ip, a.endpoint.lookup(c.Server.Endpoint, ip.Is6()), ownAddr)
	if !v.Protected && v.Peer == nil {
		v.Location = a.geo.Lookup(v.IP)
	}
	return v
}
