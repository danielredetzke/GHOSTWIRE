package main

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// A new install can take over a WireGuard server set up by pivpn: the
// server key, the network and every client with its public key, preshared
// key and addresses, so the devices keep their configs. pivpn keeps the
// client private keys in /etc/wireguard/configs; they are not read.

const (
	pivpnSetupVars = "etc/pivpn/wireguard/setupVars.conf"
	pivpnNote      = "Imported from pivpn"
)

// pivpnSetup is what install takes over from pivpn.
type pivpnSetup struct {
	Dev        string // the interface, wg0
	Server     Server
	Peers      []Peer
	Renamed    [][2]string // pivpn name, name here
	ClientKeys string      // where pivpn keeps the client configs with private keys
}

// readPivpn reads pivpn's WireGuard setup under root ("/" on a server). It
// returns nil and no error when pivpn's WireGuard is not installed.
func readPivpn(root string) (*pivpnSetup, error) {
	vars, err := readSetupVars(filepath.Join(root, pivpnSetupVars))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s := &pivpnSetup{Dev: pivpnIfName(vars)}
	if checkIfName(s.Dev) != nil {
		return nil, fmt.Errorf("pivpn: interface name %q is not usable", s.Dev)
	}
	confPath := filepath.Join(root, "etc/wireguard", s.Dev+".conf")
	conf, err := parseWgConf(confPath)
	if err != nil {
		return nil, fmt.Errorf("pivpn: %w", err)
	}
	s.ClientKeys = "/etc/wireguard/configs"
	created := readClientsTxt(filepath.Join(root, "etc/wireguard/configs/clients.txt"))

	// Server
	srv := &s.Server
	srv.Interface = s.Dev
	if _, err := wgtypes.ParseKey(conf.privateKey); err != nil {
		return nil, fmt.Errorf("pivpn: %s: the server key is missing or invalid", confPath)
	}
	srv.PrivateKey = conf.privateKey
	srv.KeyCreated = fileTime(filepath.Join(root, "etc/wireguard/keys/server_priv"), confPath)
	if srv.ListenPort = conf.listenPort; srv.ListenPort == 0 {
		srv.ListenPort, _ = strconv.Atoi(vars["pivpnPORT"])
	}
	if srv.MTU = conf.mtu; srv.MTU == 0 {
		srv.MTU, _ = strconv.Atoi(vars["pivpnMTU"])
	}
	for _, a := range conf.address {
		if a.Addr().Is4() {
			srv.IPv4 = a.Masked().String()
		} else {
			srv.IPv6, srv.IPv6Enabled = a.Masked().String(), true
		}
	}
	if srv.IPv4 == "" {
		return nil, fmt.Errorf("pivpn: %s has no IPv4 Address line", confPath)
	}
	if h := vars["pivpnHOST"]; checkEndpoint(h) == nil {
		srv.Endpoint = h
	}
	srv.NAT, srv.PeerToPeer, srv.OpenPort = true, true, true
	for _, k := range []string{"pivpnDNS1", "pivpnDNS2"} {
		if a, err := netip.ParseAddr(vars[k]); err == nil {
			srv.ClientDefaults.DNS = append(srv.ClientDefaults.DNS, a.String())
		}
	}
	for _, v := range strings.Split(vars["ALLOWED_IPS"], ",") {
		if p, err := netip.ParsePrefix(strings.TrimSpace(v)); err == nil {
			srv.ClientDefaults.AllowedIPs = append(srv.ClientDefaults.AllowedIPs, p.Masked().String())
		}
	}
	// pivpn without IPv6 writes only 0.0.0.0/0, so clients would send IPv6
	// around the tunnel. ::/0 makes the server drop it instead.
	if fullTunnel(srv.ClientDefaults.AllowedIPs, false) && !fullTunnel(srv.ClientDefaults.AllowedIPs, true) {
		srv.ClientDefaults.AllowedIPs = append(srv.ClientDefaults.AllowedIPs, "::/0")
	}
	srv.ClientDefaults.Keepalive, _ = strconv.Atoi(vars["pivpnPERSISTENTKEEPALIVE"])

	// Clients
	v6net, _ := netip.ParsePrefix(srv.IPv6)
	taken := map[string]bool{}
	for _, cl := range conf.clients {
		pub, err := wgtypes.ParseKey(cl.publicKey)
		if err != nil {
			return nil, fmt.Errorf("pivpn: client %q has no valid public key", cl.name)
		}
		p := Peer{ID: newID(), Name: cl.name, Note: pivpnNote, Enabled: !cl.disabled, PublicKey: pub.String()}
		if cl.presharedKey != "" {
			psk, err := wgtypes.ParseKey(cl.presharedKey)
			if err != nil {
				return nil, fmt.Errorf("pivpn: client %q has an invalid preshared key", cl.name)
			}
			p.PresharedKey = psk.String()
		}
		for _, a := range cl.allowedIPs {
			switch {
			case a.Addr().Is4() && p.IPv4 == "":
				p.IPv4 = a.Addr().String()
			case a.Addr().Is6() && p.IPv6 == "" && v6net.IsValid() && v6net.Contains(a.Addr()):
				p.IPv6 = a.Addr().String()
			}
		}
		if p.IPv4 == "" {
			return nil, fmt.Errorf("pivpn: client %q has no IPv4 address", cl.name)
		}
		// Keep pivpn's IPv6 address only where it differs from the mapped one.
		if v4, err := netip.ParseAddr(p.IPv4); err == nil && v6net.IsValid() && p.IPv6 == mapIPv6(v6net, v4).String() {
			p.IPv6 = ""
		}
		t, ok := created[cl.name]
		if !ok {
			t = srv.KeyCreated
		}
		t = t.UTC()
		p.Created, p.ConfigIssued = t, &t
		if name := usableName(cl.name, taken); name != cl.name {
			s.Renamed = append(s.Renamed, [2]string{cl.name, name})
			p.Name = name
		}
		taken[strings.ToLower(p.Name)] = true
		s.Peers = append(s.Peers, p)
	}
	return s, nil
}

// apply puts the pivpn setup into a fresh config.
func (s *pivpnSetup) apply(c *Config) {
	cd := c.Server.ClientDefaults
	c.Server = s.Server
	// Settings pivpn left empty keep GHOSTWIRE's defaults.
	if c.Server.ClientDefaults.DNS == nil {
		c.Server.ClientDefaults.DNS = cd.DNS
	}
	if c.Server.ClientDefaults.AllowedIPs == nil {
		c.Server.ClientDefaults.AllowedIPs = cd.AllowedIPs
	}
	if c.Server.IPv6 == "" {
		// IPv4-only pivpn: IPv6 stays off, with the network GHOSTWIRE
		// would pick, ready for when it is switched on.
		c.Server.IPv6 = "fd11:5ee:bad:c0de::/64"
	}
	c.Peers = slices.Clone(s.Peers)
	c.applyDefaults()
}

// names lists the clients for the takeover question.
func (s *pivpnSetup) names() string {
	var out []string
	for _, p := range s.Peers {
		n := p.Name
		if !p.Enabled {
			n += " (off)"
		}
		out = append(out, n)
	}
	return strings.Join(out, ", ")
}

func checkIfName(n string) error {
	if n == "" || len(n) > 15 || strings.ContainsAny(n, "/ \t") {
		return errors.New("bad interface name")
	}
	return nil
}

// usableName turns a pivpn client name into one GHOSTWIRE accepts and that
// is not taken yet.
func usableName(name string, taken map[string]bool) string {
	b := []rune{}
	for _, r := range name {
		if r < 128 && (r == '.' || r == '@' || r == '_' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			b = append(b, r)
		} else {
			b = append(b, '-')
		}
	}
	n := strings.TrimLeft(string(b), "-.")
	if n == "" || strings.Trim(n, "0123456789") == "" || n == "server" {
		n = "peer-" + n
	}
	n = strings.TrimRight(n, "-")
	if len(n) > 32 {
		n = n[:32]
	}
	base := n
	for i := 1; taken[strings.ToLower(n)] || validatePeerName(n) != nil; i++ {
		suffix := "-" + strconv.Itoa(i)
		n = base
		if len(n)+len(suffix) > 32 {
			n = n[:32-len(suffix)]
		}
		n += suffix
		if i > 1000 {
			break
		}
	}
	return n
}

// readSetupVars reads pivpn's KEY=VALUE file; values may be quoted.
func readSetupVars(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out, nil
}

// pivpnIfName is the WireGuard interface in pivpn's setupVars.
func pivpnIfName(vars map[string]string) string { return cmp.Or(vars["pivpnDEV"], "wg0") }

// pivpnDev returns pivpn's WireGuard interface under root, or "" when pivpn's
// WireGuard is not installed.
func pivpnDev(root string) string {
	vars, err := readSetupVars(filepath.Join(root, pivpnSetupVars))
	if err != nil {
		return ""
	}
	return pivpnIfName(vars)
}

type wgClient struct {
	name, publicKey, presharedKey string
	allowedIPs                    []netip.Prefix
	disabled                      bool
}

type wgConf struct {
	privateKey      string
	listenPort, mtu int
	address         []netip.Prefix
	clients         []wgClient
}

// parseWgConf reads pivpn's wg0.conf: the [Interface] section, then one
// "### begin NAME ###" … "### end NAME ###" block per client. pivpn turns a
// client off by prefixing each line of its block with "#[disabled] ".
func parseWgConf(path string) (*wgConf, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	c := &wgConf{}
	var cur *wgClient
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		disabled := false
		if rest, ok := strings.CutPrefix(line, "#[disabled]"); ok {
			line, disabled = strings.TrimSpace(rest), true
		}
		if name, ok := strings.CutPrefix(line, "### begin "); ok {
			c.clients = append(c.clients, wgClient{name: strings.TrimSpace(strings.TrimSuffix(name, "###"))})
			cur = &c.clients[len(c.clients)-1]
			continue
		}
		if strings.HasPrefix(line, "### end ") {
			cur = nil
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		if cur != nil {
			cur.disabled = cur.disabled || disabled
			switch k {
			case "publickey":
				cur.publicKey = v
			case "presharedkey":
				cur.presharedKey = v
			case "allowedips":
				cur.allowedIPs = parsePrefixes(v)
			}
			continue
		}
		switch k {
		case "privatekey":
			c.privateKey = v
		case "listenport":
			c.listenPort, _ = strconv.Atoi(v)
		case "mtu":
			c.mtu, _ = strconv.Atoi(v)
		case "address":
			c.address = parsePrefixes(v)
		}
	}
	return c, sc.Err()
}

func parsePrefixes(v string) []netip.Prefix {
	var out []netip.Prefix
	for _, s := range strings.Split(v, ",") {
		if p, err := netip.ParsePrefix(strings.TrimSpace(s)); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// readClientsTxt returns when each client was created: clients.txt has
// "NAME PUBLICKEY UNIXTIME" per line.
func readClientsTxt(path string) map[string]time.Time {
	out := map[string]time.Time{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		if n, err := strconv.ParseInt(f[2], 10, 64); err == nil && n > 0 {
			out[f[0]] = time.Unix(n, 0)
		}
	}
	return out
}

// fileTime is the modification time of the first file that exists.
func fileTime(paths ...string) time.Time {
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil {
			return st.ModTime().UTC()
		}
	}
	return time.Now().UTC()
}
