//go:build linux

package main

import (
	"errors"
	"math/rand/v2"
	"net"
	"net/netip"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// listenICMP opens an unprivileged ICMP socket, which the kernel allows for
// the groups in net.ipv4.ping_group_range (systemd opens it to all groups).
// Running as root, a raw socket works too.
func listenICMP() (c *icmp.PacketConn, raw bool, err error) {
	if c, err = icmp.ListenPacket("udp4", "0.0.0.0"); err == nil {
		return c, false, nil
	}
	if c, err2 := icmp.ListenPacket("ip4:icmp", "0.0.0.0"); err2 == nil {
		return c, true, nil
	}
	return nil, false, errors.New("cannot open an ICMP socket (" + err.Error() + "): add the service's group to the sysctl net.ipv4.ping_group_range")
}

func (k *linuxKernel) Ping(dsts []netip.Addr, timeout time.Duration) (map[netip.Addr]time.Duration, error) {
	conn, raw, err := listenICMP()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	// On an unprivileged socket the kernel sets the ID and delivers only the
	// socket's own replies; on a raw socket the ID tells them apart.
	id := rand.IntN(0xffff) + 1
	bySeq := map[int]netip.Addr{}
	sent := map[netip.Addr]time.Time{}
	for i, d := range dsts {
		seq := i + 1
		b, err := (&icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: id, Seq: seq, Data: []byte(appName)}}).Marshal(nil)
		if err != nil {
			return nil, err
		}
		var to net.Addr = &net.UDPAddr{IP: d.AsSlice()}
		if raw {
			to = &net.IPAddr{IP: d.AsSlice()}
		}
		sent[d] = time.Now()
		if _, err := conn.WriteTo(b, to); err != nil {
			delete(sent, d) // e.g. the peer has no endpoint: counts as lost
			continue
		}
		bySeq[seq] = d
	}
	out := map[netip.Addr]time.Duration{}
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 1500)
	for len(out) < len(sent) {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			break // deadline reached
		}
		now := time.Now()
		m, err := icmp.ParseMessage(1, buf[:n])
		if err != nil || m.Type != ipv4.ICMPTypeEchoReply {
			continue
		}
		e, ok := m.Body.(*icmp.Echo)
		if !ok || (raw && e.ID != id) {
			continue
		}
		var src netip.Addr
		switch a := from.(type) {
		case *net.UDPAddr:
			src, _ = netip.AddrFromSlice(a.IP)
		case *net.IPAddr:
			src, _ = netip.AddrFromSlice(a.IP)
		}
		src = src.Unmap()
		if d, ok := bySeq[e.Seq]; !ok || d != src {
			continue
		}
		if _, dup := out[src]; !dup {
			out[src] = now.Sub(sent[src])
		}
	}
	return out, nil
}
