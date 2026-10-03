package main

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"math/big"
	"net"
	"net/netip"
)

// Subnets that commonly appear on home and office networks. A tunnel network
// overlapping one of them breaks routing for clients that sit on such a LAN.
// Source: pivpn, https://community.openvpn.net/openvpn/wiki/AvoidRoutingConflicts
var avoidedSubnets = mustPrefixes(
	"10.0.0.0/24", "10.0.1.0/24", "10.1.1.0/24", "10.1.10.0/24", "10.2.0.0/24",
	"10.8.0.0/24", "10.10.1.0/24", "10.90.90.0/24", "10.100.1.0/24",
	"10.255.255.0/24", "192.168.0.0/24", "192.168.1.0/24",
)

func mustPrefixes(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(s))
	for i, v := range s {
		out[i] = netip.MustParsePrefix(v)
	}
	return out
}

// hostNetworks returns the networks of all addresses configured on this host.
func hostNetworks() []netip.Prefix {
	var out []netip.Prefix
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if p, err := netip.ParsePrefix(n.String()); err == nil {
				out = append(out, p.Masked())
			}
		}
	}
	return out
}

func hasGlobalIPv6() bool {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() == nil && n.IP.IsGlobalUnicast() && !n.IP.IsPrivate() {
			return true
		}
	}
	return false
}

// randomSubnet picks a random, unused /bits network from 10/8, then
// 172.16/12, then 192.168/16, like pivpn does.
func randomSubnet(bits int) (netip.Prefix, error) {
	taken := append(append([]netip.Prefix{}, avoidedSubnets...), hostNetworks()...)
	for _, pool := range mustPrefixes("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16") {
		if bits < pool.Bits() {
			continue
		}
		count := int64(1) << (bits - pool.Bits())
		base := binary.BigEndian.Uint32(pool.Addr().AsSlice())
		size := uint32(1) << (32 - bits)
		for range 2000 {
			n, err := rand.Int(rand.Reader, big.NewInt(count))
			if err != nil {
				return netip.Prefix{}, err
			}
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], base+uint32(n.Int64())*size)
			cand := netip.PrefixFrom(netip.AddrFrom4(b), bits)
			if !overlapsAny(cand, taken) {
				return cand, nil
			}
		}
	}
	return netip.Prefix{}, errors.New("no free private IPv4 subnet found")
}

func overlapsAny(p netip.Prefix, list []netip.Prefix) bool {
	for _, q := range list {
		if p.Overlaps(q) {
			return true
		}
	}
	return false
}

func addrToU32(a netip.Addr) uint32 { return binary.BigEndian.Uint32(a.AsSlice()) }

func u32ToAddr(v uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}

// serverIPv4 is the first host address of the tunnel network.
func serverIPv4(n netip.Prefix) netip.Addr { return n.Addr().Next() }

func lastAddr(n netip.Prefix) netip.Addr {
	return u32ToAddr(addrToU32(n.Addr()) | (1<<(32-n.Bits()) - 1))
}

// nextFreeIPv4 returns the lowest unused peer address (server is .1).
func nextFreeIPv4(c *Config) (netip.Addr, error) {
	n := netip.MustParsePrefix(c.Server.IPv4)
	used := map[netip.Addr]bool{}
	for _, p := range c.Peers {
		if a, err := netip.ParseAddr(p.IPv4); err == nil {
			used[a] = true
		}
	}
	last := lastAddr(n)
	for a := serverIPv4(n).Next(); a.Less(last); a = a.Next() {
		if !used[a] {
			return a, nil
		}
	}
	return netip.Addr{}, badRequest("no free address left in %s", n)
}

// capacity is the number of peer addresses in the tunnel network.
func capacity(n netip.Prefix) int { return 1<<(32-n.Bits()) - 3 }

// mapIPv6 puts the 32 bits of an IPv4 address into the low bits of the IPv6
// network: 10.84.12.8 in fd11:5ee:bad:c0de::/64 becomes fd11:5ee:bad:c0de::a54:c08.
func mapIPv6(v6net netip.Prefix, v4 netip.Addr) netip.Addr {
	b := v6net.Addr().As16()
	copy(b[12:], v4.AsSlice())
	return netip.AddrFrom16(b)
}
