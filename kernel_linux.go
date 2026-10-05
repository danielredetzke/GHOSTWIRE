//go:build linux

package main

import (
	"cmp"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

type linuxKernel struct {
	wg *wgctrl.Client
}

func newKernel() (Kernel, error) {
	c, err := wgctrl.New()
	if err != nil {
		return nil, fmt.Errorf("wgctrl: %w", err)
	}
	return &linuxKernel{wg: c}, nil
}

func (k *linuxKernel) Close() error { return k.wg.Close() }

// serverAddrs returns the addresses the interface carries: the first host of
// each tunnel network with the network's prefix length, which also installs
// the route to the peers.
func serverAddrs(c *Config) []netip.Prefix {
	v4 := netip.MustParsePrefix(c.Server.IPv4)
	out := []netip.Prefix{netip.PrefixFrom(serverIPv4(v4), v4.Bits())}
	if c.Server.IPv6Enabled {
		v6 := netip.MustParsePrefix(c.Server.IPv6)
		out = append(out, netip.PrefixFrom(mapIPv6(v6, serverIPv4(v4)), v6.Bits()))
	}
	return out
}

func toIPNet(p netip.Prefix) *net.IPNet {
	return &net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}
}

func (k *linuxKernel) ensureLink(c *Config) (netlink.Link, error) {
	s := c.Server
	link, err := netlink.LinkByName(s.Interface)
	var nf netlink.LinkNotFoundError
	if errors.As(err, &nf) {
		la := netlink.NewLinkAttrs()
		la.Name = s.Interface
		la.MTU = s.MTU
		if err := netlink.LinkAdd(&netlink.Wireguard{LinkAttrs: la}); err != nil {
			return nil, fmt.Errorf("create %s: %w (is the wireguard kernel module available?)", s.Interface, err)
		}
		link, err = netlink.LinkByName(s.Interface)
	}
	if err != nil {
		return nil, fmt.Errorf("find %s: %w", s.Interface, err)
	}
	if link.Type() != "wireguard" {
		return nil, fmt.Errorf("%s exists but is a %s interface, not wireguard", s.Interface, link.Type())
	}
	if link.Attrs().MTU != s.MTU {
		if err := netlink.LinkSetMTU(link, s.MTU); err != nil {
			return nil, fmt.Errorf("set MTU: %w", err)
		}
	}
	return link, nil
}

func (k *linuxKernel) syncAddrs(link netlink.Link, want []netip.Prefix) error {
	have, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("list addresses: %w", err)
	}
	var present []netip.Prefix
	for _, a := range have {
		if a.IP.IsLinkLocalUnicast() {
			continue
		}
		ones, _ := a.Mask.Size()
		ip, _ := netip.AddrFromSlice(a.IP)
		p := netip.PrefixFrom(ip.Unmap(), ones)
		if slices.Contains(want, p) {
			present = append(present, p)
			continue
		}
		if err := netlink.AddrDel(link, &a); err != nil {
			return fmt.Errorf("remove address %s: %w", p, err)
		}
	}
	for _, p := range want {
		if slices.Contains(present, p) {
			continue
		}
		if err := netlink.AddrAdd(link, &netlink.Addr{IPNet: toIPNet(p)}); err != nil {
			return fmt.Errorf("add address %s: %w", p, err)
		}
	}
	return nil
}

// syncPeers changes only peers that differ, so unchanged peers keep their
// sessions — the same effect as "wg syncconf".
func (k *linuxKernel) syncDevice(c *Config) error {
	s := c.Server
	priv, err := wgtypes.ParseKey(s.PrivateKey)
	if err != nil {
		return fmt.Errorf("server private key: %w", err)
	}
	dev, err := k.wg.Device(s.Interface)
	if err != nil {
		return fmt.Errorf("read %s: %w", s.Interface, err)
	}

	type want struct {
		psk wgtypes.Key
		ips []net.IPNet
	}
	desired := map[wgtypes.Key]want{}
	for i := range c.Peers {
		p := &c.Peers[i]
		if !p.Enabled || !p.hasKey() {
			continue
		}
		pub, err := wgtypes.ParseKey(p.PublicKey)
		if err != nil {
			return fmt.Errorf("peer %s: %w", p.Name, err)
		}
		var w want
		if p.PresharedKey != "" {
			if w.psk, err = wgtypes.ParseKey(p.PresharedKey); err != nil {
				return fmt.Errorf("peer %s preshared key: %w", p.Name, err)
			}
		}
		for _, a := range peerAddresses(c, p) {
			w.ips = append(w.ips, *toIPNet(a))
		}
		desired[pub] = w
	}

	var changes []wgtypes.PeerConfig
	existing := map[wgtypes.Key]wgtypes.Peer{}
	for _, p := range dev.Peers {
		existing[p.PublicKey] = p
		if _, ok := desired[p.PublicKey]; !ok {
			changes = append(changes, wgtypes.PeerConfig{PublicKey: p.PublicKey, Remove: true})
		}
	}
	for pub, w := range desired {
		if cur, ok := existing[pub]; ok && cur.PresharedKey == w.psk && sameIPNets(cur.AllowedIPs, w.ips) {
			continue
		}
		psk := w.psk
		changes = append(changes, wgtypes.PeerConfig{
			PublicKey:         pub,
			PresharedKey:      &psk,
			ReplaceAllowedIPs: true,
			AllowedIPs:        w.ips,
		})
	}

	cfg := wgtypes.Config{Peers: changes}
	if dev.PrivateKey != priv {
		cfg.PrivateKey = &priv
	}
	if dev.ListenPort != s.ListenPort {
		port := s.ListenPort
		cfg.ListenPort = &port
	}
	if cfg.PrivateKey == nil && cfg.ListenPort == nil && len(changes) == 0 {
		return nil
	}
	if err := k.wg.ConfigureDevice(s.Interface, cfg); err != nil {
		return fmt.Errorf("configure %s: %w", s.Interface, err)
	}
	return nil
}

func sameIPNets(a, b []net.IPNet) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(n net.IPNet) string { return n.String() }
	as, bs := make([]string, len(a)), make([]string, len(b))
	for i := range a {
		as[i], bs[i] = key(a[i]), key(b[i])
	}
	slices.Sort(as)
	slices.Sort(bs)
	return slices.Equal(as, bs)
}

func (k *linuxKernel) Apply(c *Config) error {
	link, err := k.ensureLink(c)
	if err != nil {
		return err
	}
	if err := k.syncAddrs(link, serverAddrs(c)); err != nil {
		return err
	}
	if err := k.syncDevice(c); err != nil {
		return err
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring %s up: %w", c.Server.Interface, err)
	}
	// Forwarding is normally set by /etc/sysctl.d at install time; this only
	// succeeds when running as root.
	_ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644)
	if c.Server.IPv6Enabled {
		_ = os.WriteFile("/proc/sys/net/ipv6/conf/all/forwarding", []byte("1"), 0o644)
	}
	up4, up6 := k.Uplink(c, false), k.Uplink(c, true)
	return applyFirewall(c, up4, up6, lanNetworks(up4, up6))
}

func (k *linuxKernel) Sample(iface string) ([]PeerSample, error) {
	dev, err := k.wg.Device(iface)
	if err != nil {
		return nil, err
	}
	out := make([]PeerSample, 0, len(dev.Peers))
	for _, p := range dev.Peers {
		s := PeerSample{
			PublicKey:     p.PublicKey.String(),
			RxBytes:       p.ReceiveBytes,
			TxBytes:       p.TransmitBytes,
			LastHandshake: p.LastHandshakeTime,
		}
		if p.Endpoint != nil {
			s.Endpoint = p.Endpoint.String()
		}
		out = append(out, s)
	}
	return out, nil
}

// Uplink returns the configured uplink or the interface of the default route.
func (k *linuxKernel) Uplink(c *Config, v6 bool) string {
	if !v6 && c.Server.UplinkV4 != "" {
		return c.Server.UplinkV4
	}
	if v6 && c.Server.UplinkV6 != "" {
		return c.Server.UplinkV6
	}
	dst := net.ParseIP("1.1.1.1")
	if v6 {
		dst = net.ParseIP("2606:4700:4700::1111")
	}
	routes, err := netlink.RouteGet(dst)
	if err != nil || len(routes) == 0 {
		return ""
	}
	l, err := netlink.LinkByIndex(routes[0].LinkIndex)
	if err != nil {
		return ""
	}
	return l.Attrs().Name
}

// lanNetworks returns the LAN networks on the IPv4 and IPv6 uplinks (see
// lanBlock), used to block peers from the server's LAN when LAN access is off.
func lanNetworks(uplinks ...string) []netip.Prefix {
	var nets []netip.Prefix
	for i, uplink := range uplinks {
		if uplink == "" || slices.Contains(uplinks[:i], uplink) {
			continue
		}
		l, err := netlink.LinkByName(uplink)
		if err != nil {
			continue
		}
		addrs, _ := netlink.AddrList(l, netlink.FAMILY_ALL)
		for _, a := range addrs {
			ones, _ := a.Mask.Size()
			if ip, ok := netip.AddrFromSlice(a.IP); ok {
				nets = append(nets, netip.PrefixFrom(ip.Unmap(), ones))
			}
		}
	}
	return lanBlock(nets)
}

// publicAddr reports the uplink's address for the health check: the first
// public one, or else the first private one, marked as behind NAT. It reads
// the interface rather than asking an outside service.
func publicAddr(uplink string, v6 bool) (bool, string) {
	l, err := netlink.LinkByName(uplink)
	if err != nil {
		return false, uplink + " not found"
	}
	family := netlink.FAMILY_V4
	if v6 {
		family = netlink.FAMILY_V6
	}
	addrs, _ := netlink.AddrList(l, family)
	var private string
	for _, a := range addrs {
		if !a.IP.IsGlobalUnicast() {
			continue
		}
		if !a.IP.IsPrivate() {
			return true, a.IP.String()
		}
		if private == "" {
			private = a.IP.String()
		}
	}
	if private != "" {
		return true, private + " (private, behind NAT)"
	}
	return false, "no address on " + uplink
}

func (k *linuxKernel) Checks(c *Config) []Check {
	var out []Check
	link, err := netlink.LinkByName(c.Server.Interface)
	if err != nil {
		out = append(out, Check{"WireGuard interface", false, c.Server.Interface + " does not exist"})
	} else {
		up := link.Attrs().Flags&net.FlagUp != 0
		out = append(out, Check{"WireGuard interface", up, c.Server.Interface + map[bool]string{true: " is up", false: " is down"}[up]})
	}
	fwd := readSysctl("/proc/sys/net/ipv4/ip_forward") == "1"
	out = append(out, Check{"IPv4 forwarding", fwd, "net.ipv4.ip_forward=" + readSysctl("/proc/sys/net/ipv4/ip_forward")})
	if c.Server.IPv6Enabled {
		v := readSysctl("/proc/sys/net/ipv6/conf/all/forwarding")
		out = append(out, Check{"IPv6 forwarding", v == "1", "net.ipv6.conf.all.forwarding=" + v})
	}
	// With IPv6 forwarding on, accept_ra 1 means router announcements are
	// ignored: an IPv6 route learned from them expires (see sysctlConf).
	if readSysctl("/proc/sys/net/ipv6/conf/all/forwarding") == "1" {
		up := cmp.Or(k.Uplink(c, true), k.Uplink(c, false))
		if ra := readSysctl("/proc/sys/net/ipv6/conf/" + up + "/accept_ra"); up != "" && ra != "" {
			ok := ra != "1"
			detail := "net.ipv6.conf." + up + ".accept_ra=" + ra
			if !ok {
				detail += ": IPv6 from router announcements stops working; run " + appName + " update"
			}
			out = append(out, Check{"IPv6 router announcements", ok, detail})
		}
	}
	ok, detail := firewallPresent()
	out = append(out, Check{"nftables rules", ok, detail})
	up4 := k.Uplink(c, false)
	out = append(out, Check{"IPv4 uplink", up4 != "", map[bool]string{true: "via " + up4, false: "no default route found"}[up4 != ""]})
	if up4 != "" {
		ok, detail := publicAddr(up4, false)
		out = append(out, Check{"Public IPv4", ok, detail})
	}
	if c.Server.IPv6Enabled {
		up6 := k.Uplink(c, true)
		out = append(out, Check{"IPv6 uplink", up6 != "", map[bool]string{true: "via " + up6, false: "no default route found"}[up6 != ""]})
		if up6 != "" {
			ok, detail := publicAddr(up6, true)
			out = append(out, Check{"Public IPv6", ok, detail})
		}
	}
	return out
}

func (k *linuxKernel) Down(c *Config) error {
	var errs []error
	if link, err := netlink.LinkByName(c.Server.Interface); err == nil {
		errs = append(errs, netlink.LinkDel(link))
	}
	errs = append(errs, removeFirewall())
	return errors.Join(errs...)
}
