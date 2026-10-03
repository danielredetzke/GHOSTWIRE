package main

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func newPrivateKey() (wgtypes.Key, error) { return wgtypes.GeneratePrivateKey() }

func newPresharedKey() (wgtypes.Key, error) { return wgtypes.GenerateKey() }

func serverPublicKey(c *Config) string {
	k, err := wgtypes.ParseKey(c.Server.PrivateKey)
	if err != nil {
		return ""
	}
	return k.PublicKey().String()
}

// effective returns the peer's value or the server default.
func peerDNS(c *Config, p *Peer) []string {
	if p.DNS != nil {
		return p.DNS
	}
	return c.Server.ClientDefaults.DNS
}

func peerAllowedIPs(c *Config, p *Peer) []string {
	if p.AllowedIPs != nil {
		return p.AllowedIPs
	}
	return c.Server.ClientDefaults.AllowedIPs
}

func peerKeepalive(c *Config, p *Peer) int {
	if p.Keepalive != nil {
		return *p.Keepalive
	}
	return c.Server.ClientDefaults.Keepalive
}

// peerAddresses returns the peer's tunnel addresses as host routes for the
// server side (/32, /128).
func peerAddresses(c *Config, p *Peer) []netip.Prefix {
	v4 := netip.MustParseAddr(p.IPv4)
	out := []netip.Prefix{netip.PrefixFrom(v4, 32)}
	if c.Server.IPv6Enabled {
		out = append(out, netip.PrefixFrom(mapIPv6(netip.MustParsePrefix(c.Server.IPv6), v4), 128))
	}
	return out
}

func endpointString(c *Config) string {
	port := c.Server.EndpointPort
	if port == 0 {
		port = c.Server.ListenPort
	}
	host := c.Server.Endpoint
	if host == "" {
		host = "SET-ENDPOINT-IN-SERVER-SETTINGS"
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// clientConfig renders the wg-quick file for a peer. privateKey may be empty
// when the client generated its own keys.
func clientConfig(c *Config, p *Peer, privateKey string) string {
	v4net := netip.MustParsePrefix(c.Server.IPv4)
	v4 := netip.MustParseAddr(p.IPv4)
	addr := fmt.Sprintf("%s/%d", v4, v4net.Bits())
	if c.Server.IPv6Enabled {
		v6net := netip.MustParsePrefix(c.Server.IPv6)
		addr += fmt.Sprintf(",%s/%d", mapIPv6(v6net, v4), v6net.Bits())
	}
	if privateKey == "" {
		privateKey = "<the private key of this device>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[Interface]\nPrivateKey = %s\nAddress = %s\n", privateKey, addr)
	if dns := peerDNS(c, p); len(dns) > 0 {
		fmt.Fprintf(&b, "DNS = %s\n", strings.Join(dns, ", "))
	}
	// No MTU line: the client picks one for the network it is on, as pivpn does.
	fmt.Fprintf(&b, "\n[Peer]\nPublicKey = %s\n", serverPublicKey(c))
	if p.PresharedKey != "" {
		fmt.Fprintf(&b, "PresharedKey = %s\n", p.PresharedKey)
	}
	fmt.Fprintf(&b, "Endpoint = %s\nAllowedIPs = %s\n", endpointString(c), strings.Join(peerAllowedIPs(c, p), ", "))
	if ka := peerKeepalive(c, p); ka > 0 {
		fmt.Fprintf(&b, "PersistentKeepalive = %d\n", ka)
	}
	return b.String()
}

// qrDataURL returns a PNG QR code as a data: URL for direct use in <img src>.
func qrDataURL(text string) (string, error) {
	png, err := qrcode.Encode(text, qrcode.Medium, 512)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}
