//go:build linux

package main

import (
	"encoding/binary"
	"fmt"
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// The service owns one nftables table and rewrites it completely on every
// apply, in a single atomic transaction. Rules of other tables are untouched.
// Note: an accept here cannot override a drop in another table (for example
// ufw or firewalld); those firewalls must allow the port themselves.

func fwTable() *nftables.Table {
	return &nftables.Table{Family: nftables.TableFamilyINet, Name: appName}
}

func ifname(n string) []byte {
	b := make([]byte, 16)
	copy(b, n+"\x00")
	return b
}

func metaEq(key expr.MetaKey, data []byte) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: key, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: data},
	}
}

func iif(n string) []expr.Any { return metaEq(expr.MetaKeyIIFNAME, ifname(n)) }
func oif(n string) []expr.Any { return metaEq(expr.MetaKeyOIFNAME, ifname(n)) }

// addrMatch matches the source (src=true) or destination address against a
// prefix, including the protocol check an inet table needs.
func addrMatch(p netip.Prefix, src bool) []expr.Any {
	proto, offset, size := byte(unix.NFPROTO_IPV4), uint32(16), uint32(4)
	if src {
		offset = 12
	}
	if p.Addr().Is6() {
		proto, offset, size = unix.NFPROTO_IPV6, 24, 16
		if src {
			offset = 8
		}
	}
	mask := make([]byte, size)
	for i := 0; i < p.Bits(); i++ {
		mask[i/8] |= 0x80 >> (i % 8)
	}
	return append(metaEq(expr.MetaKeyNFPROTO, []byte{proto}),
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: offset, Len: size},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: size, Mask: mask, Xor: make([]byte, size)},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: p.Masked().Addr().AsSlice()},
	)
}

func udpDport(port int) []expr.Any {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, uint16(port))
	return append(metaEq(expr.MetaKeyL4PROTO, []byte{unix.IPPROTO_UDP}),
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: b},
	)
}

func ctEstablished() []expr.Any {
	mask := make([]byte, 4)
	binary.NativeEndian.PutUint32(mask, expr.CtStateBitESTABLISHED|expr.CtStateBitRELATED)
	return []expr.Any{
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: mask, Xor: make([]byte, 4)},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: make([]byte, 4)},
	}
}

func rule(parts ...[]expr.Any) []expr.Any {
	var out []expr.Any
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

var (
	accept = []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}}
	drop   = []expr.Any{&expr.Verdict{Kind: expr.VerdictDrop}}
	masq   = []expr.Any{&expr.Masq{}}
)

func applyFirewall(c *Config, up4, up6 string, lan []netip.Prefix) error {
	conn, err := nftables.New()
	if err != nil {
		return fmt.Errorf("nftables: %w", err)
	}
	s := c.Server
	wg := s.Interface
	t := conn.AddTable(fwTable())
	conn.FlushTable(t)

	policy := nftables.ChainPolicyAccept
	input := conn.AddChain(&nftables.Chain{Name: "input", Table: t, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookInput, Priority: nftables.ChainPriorityFilter, Policy: &policy})
	forward := conn.AddChain(&nftables.Chain{Name: "forward", Table: t, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookForward, Priority: nftables.ChainPriorityFilter, Policy: &policy})
	post := conn.AddChain(&nftables.Chain{Name: "postrouting", Table: t, Type: nftables.ChainTypeNAT,
		Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityNATSource, Policy: &policy})

	add := func(ch *nftables.Chain, e []expr.Any) { conn.AddRule(&nftables.Rule{Table: t, Chain: ch, Exprs: e}) }

	if s.OpenPort {
		add(input, rule(udpDport(s.ListenPort), accept))
	}

	if s.PeerToPeer {
		add(forward, rule(iif(wg), oif(wg), accept))
	} else {
		add(forward, rule(iif(wg), oif(wg), drop))
	}
	if !s.LANAccess {
		for _, n := range lan {
			add(forward, rule(iif(wg), addrMatch(n, false), drop))
		}
	}
	add(forward, rule(iif(wg), accept))
	add(forward, rule(oif(wg), ctEstablished(), accept))

	if s.NAT {
		if up4 != "" {
			add(post, rule(addrMatch(netip.MustParsePrefix(s.IPv4), true), oif(up4), masq))
		}
		if s.IPv6Enabled && up6 != "" {
			add(post, rule(addrMatch(netip.MustParsePrefix(s.IPv6), true), oif(up6), masq))
		}
	}

	if err := conn.Flush(); err != nil {
		return fmt.Errorf("nftables: %w", err)
	}
	return nil
}

func firewallPresent() (bool, string) {
	conn, err := nftables.New()
	if err != nil {
		return false, err.Error()
	}
	tables, err := conn.ListTablesOfFamily(nftables.TableFamilyINet)
	if err != nil {
		return false, err.Error()
	}
	for _, t := range tables {
		if t.Name == appName {
			return true, "table inet " + appName + " present"
		}
	}
	return false, "table inet " + appName + " missing"
}

func removeFirewall() error {
	if ok, _ := firewallPresent(); !ok {
		return nil
	}
	conn, err := nftables.New()
	if err != nil {
		return err
	}
	conn.DelTable(fwTable())
	return conn.Flush()
}
