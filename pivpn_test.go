package main

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// pivpnFixture writes a pivpn WireGuard layout under a temp root, in the
// format pivpn v4 writes (checked against a real install, Ubuntu 24.04).
type pivpnClient struct {
	name, v6 string
	ipv4     string
	psk      bool
	disabled bool
}

func pivpnFixture(t *testing.T, ipv6 bool, clients []pivpnClient) (root string, serverKey wgtypes.Key, pubs map[string]string) {
	t.Helper()
	root = t.TempDir()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"etc/pivpn/wireguard", "etc/wireguard/configs", "etc/wireguard/keys"} {
		must(os.MkdirAll(filepath.Join(root, d), 0o755))
	}
	v6 := "0"
	if ipv6 {
		v6 = "1"
	}
	vars := `USING_UFW=0
IPv4dev=eth0
VPN=wireguard
pivpnPORT=51820
pivpnDNS1=9.9.9.9
pivpnDNS2=149.112.112.112
pivpnHOST=vpn.example.net
pivpnPROTO=udp
pivpnMTU=1420
pivpnPERSISTENTKEEPALIVE=25
pivpnDEV=wg0
pivpnNET=10.6.0.0
subnetClass=24
pivpnenableipv6=` + v6 + `
pivpnNETv6="fd11:5ee:bad:c0de::"
subnetClassv6=64
ALLOWED_IPS="0.0.0.0/0, ::0/0"
INSTALLED_PACKAGES=(wireguard-tools qrencode)
`
	must(os.WriteFile(filepath.Join(root, pivpnSetupVars), []byte(vars), 0o644))
	serverKey, _ = wgtypes.GeneratePrivateKey()
	var conf, txt strings.Builder
	addr := "10.6.0.1/24"
	if ipv6 {
		addr += ",fd11:5ee:bad:c0de::a06:1/64"
	}
	fmt.Fprintf(&conf, "[Interface]\nPrivateKey = %s\nAddress = %s\nMTU = 1420\nListenPort = 51820\n", serverKey, addr)
	pubs = map[string]string{}
	for _, c := range clients {
		k, _ := wgtypes.GeneratePrivateKey()
		pubs[c.name] = k.PublicKey().String()
		var b strings.Builder
		fmt.Fprintf(&b, "### begin %s ###\n[Peer]\nPublicKey = %s\n", c.name, k.PublicKey())
		if c.psk {
			psk, _ := wgtypes.GenerateKey()
			fmt.Fprintf(&b, "PresharedKey = %s\n", psk)
		}
		fmt.Fprintf(&b, "AllowedIPs = %s/32", c.ipv4)
		if ipv6 {
			fmt.Fprintf(&b, ",%s/128", c.v6)
		}
		fmt.Fprintf(&b, "\n### end %s ###\n", c.name)
		block := b.String()
		if c.disabled {
			block = "#[disabled] " + strings.ReplaceAll(strings.TrimSuffix(block, "\n"), "\n", "\n#[disabled] ") + "\n"
		}
		conf.WriteString(block)
		fmt.Fprintf(&txt, "%s %s 1700000000 167116802\n", c.name, k.PublicKey())
	}
	must(os.WriteFile(filepath.Join(root, "etc/wireguard/wg0.conf"), []byte(conf.String()), 0o644))
	must(os.WriteFile(filepath.Join(root, "etc/wireguard/configs/clients.txt"), []byte(txt.String()), 0o644))
	return root, serverKey, pubs
}

func importedConfig(t *testing.T, s *pivpnSetup) *Config {
	t.Helper()
	c := &Config{}
	c.applyDefaults()
	s.apply(c)
	if err := c.validate(); err != nil {
		t.Fatalf("imported config does not validate: %v", err)
	}
	return c
}

func TestPivpnImport(t *testing.T) {
	// No pivpn: nothing to import, no error.
	if s, err := readPivpn(t.TempDir()); s != nil || err != nil {
		t.Fatalf("empty root: %v %v", s, err)
	}

	clients := []pivpnClient{
		{name: "iphone-alex", ipv4: "10.6.0.2", v6: "fd11:5ee:bad:c0de::a06:2", psk: true},
		{name: "nas-office", ipv4: "10.6.0.5", v6: "fd11:5ee:bad:c0de::a06:5", psk: false},
		{name: "phone-guest", ipv4: "10.6.0.6", v6: "fd11:5ee:bad:c0de::a06:6", psk: true, disabled: true},
		// Hand-edited: an IPv6 address that is not the mapped one.
		{name: "old-laptop", ipv4: "10.6.0.7", v6: "fd11:5ee:bad:c0de::7", psk: true},
	}
	root, key, pubs := pivpnFixture(t, true, clients)
	s, err := readPivpn(root)
	if err != nil || s == nil {
		t.Fatal(err)
	}
	c := importedConfig(t, s)
	srv := c.Server
	if srv.PrivateKey != key.String() || srv.ListenPort != 51820 || srv.MTU != 1420 || srv.Interface != "wg0" ||
		srv.IPv4 != "10.6.0.0/24" || srv.IPv6 != "fd11:5ee:bad:c0de::/64" || !srv.IPv6Enabled ||
		srv.Endpoint != "vpn.example.net" || !srv.NAT || !srv.PeerToPeer || !srv.OpenPort {
		t.Fatalf("server: %+v", srv)
	}
	cd := srv.ClientDefaults
	if strings.Join(cd.DNS, ",") != "9.9.9.9,149.112.112.112" || strings.Join(cd.AllowedIPs, ",") != "0.0.0.0/0,::/0" || cd.Keepalive != 25 {
		t.Fatalf("client defaults: %+v", cd)
	}
	if len(c.Peers) != 4 {
		t.Fatalf("want 4 peers, got %d", len(c.Peers))
	}
	by := map[string]*Peer{}
	for i := range c.Peers {
		by[c.Peers[i].Name] = &c.Peers[i]
	}
	ph := by["iphone-alex"]
	if ph.PublicKey != pubs["iphone-alex"] || ph.PresharedKey == "" || ph.IPv4 != "10.6.0.2" || ph.IPv6 != "" ||
		!ph.Enabled || ph.Note != pivpnNote || !ph.Created.Equal(time.Unix(1700000000, 0)) || ph.ConfigIssued == nil {
		t.Fatalf("iphone-alex: %+v", ph)
	}
	if by["nas-office"].PresharedKey != "" {
		t.Error("nas-office had no preshared key")
	}
	if by["phone-guest"].Enabled {
		t.Error("a #[disabled] client must be imported switched off")
	}
	if by["old-laptop"].IPv6 != "fd11:5ee:bad:c0de::7" {
		t.Errorf("a non-mapped IPv6 address must be kept, got %q", by["old-laptop"].IPv6)
	}

	// Each device's own pivpn config keeps working: the server accepts its
	// key, preshared key and both addresses.
	for _, cl := range clients {
		p := by[cl.name]
		got := []string{}
		for _, a := range peerAddresses(c, p) {
			got = append(got, a.String())
		}
		want := cl.ipv4 + "/32 " + cl.v6 + "/128"
		if strings.Join(got, " ") != want {
			t.Errorf("%s: server allows %v, the device uses %s", cl.name, got, want)
		}
	}
	// A config issued here gets the mapped address and drops the kept one.
	conf := clientConfig(c, by["old-laptop"], "")
	if !strings.Contains(conf, "fd11:5ee:bad:c0de::7/64") {
		t.Errorf("config before re-issue should keep pivpn's address:\n%s", conf)
	}

	// IPv4-only pivpn.
	root4, _, _ := pivpnFixture(t, false, clients[:1])
	s4, err := readPivpn(root4)
	if err != nil {
		t.Fatal(err)
	}
	c4 := importedConfig(t, s4)
	if c4.Server.IPv6Enabled || c4.Peers[0].IPv6 != "" {
		t.Fatalf("IPv4-only import: %+v %+v", c4.Server, c4.Peers[0])
	}

	// A wg0.conf without clients imports the server alone.
	root0, _, _ := pivpnFixture(t, true, nil)
	s0, err := readPivpn(root0)
	if err != nil || len(importedConfig(t, s0).Peers) != 0 {
		t.Fatalf("no clients: %v", err)
	}

	// Broken files are refused before anything changes.
	broken := func(edit func(string) string) error {
		r, _, _ := pivpnFixture(t, true, clients[:1])
		p := filepath.Join(r, "etc/wireguard/wg0.conf")
		b, _ := os.ReadFile(p)
		_ = os.WriteFile(p, []byte(edit(string(b))), 0o644)
		_, err := readPivpn(r)
		return err
	}
	if broken(func(s string) string { return strings.Replace(s, "PrivateKey = ", "PrivateKey = x", 1) }) == nil {
		t.Error("a bad server key must be refused")
	}
	if broken(func(s string) string { return strings.Replace(s, "\nPublicKey = ", "\nPublicKey = x", 1) }) == nil {
		t.Error("a bad client key must be refused")
	}
	if err := broken(func(s string) string { return strings.Replace(s, "AllowedIPs = 10.6.0.2/32,", "AllowedIPs = ", 1) }); err == nil {
		t.Error("a client without IPv4 must be refused")
	}
	r, _, _ := pivpnFixture(t, true, nil)
	_ = os.Remove(filepath.Join(r, "etc/wireguard/wg0.conf"))
	if _, err := readPivpn(r); err == nil || errors.Is(err, os.ErrNotExist) && !strings.Contains(err.Error(), "pivpn") {
		t.Errorf("a missing wg0.conf must be an error: %v", err)
	}
}

func TestPivpnNames(t *testing.T) {
	taken := map[string]bool{"phone": true}
	for in, want := range map[string]string{
		"iphone-alex": "iphone-alex",
		"phone":       "phone-1",
		"server":      "peer-server",
		"12345":       "peer-12345",
		"-dash":       "dash",
		"a-very-long-client-name-from-pivpn-2025": "a-very-long-client-name-from-piv",
		"Ümlaut": "mlaut",
	} {
		if got := usableName(in, taken); got != want || validatePeerName(got) != nil {
			t.Errorf("usableName(%q) = %q, want %q", in, got, want)
		}
	}

	// Two pivpn names that become the same here are both kept, renamed.
	root, _, _ := pivpnFixture(t, true, []pivpnClient{
		{name: "Phone", ipv4: "10.6.0.2", v6: "fd11:5ee:bad:c0de::a06:2"},
		{name: "phone", ipv4: "10.6.0.3", v6: "fd11:5ee:bad:c0de::a06:3"},
	})
	s, err := readPivpn(root)
	if err != nil {
		t.Fatal(err)
	}
	c := importedConfig(t, s)
	if c.Peers[0].Name != "Phone" || c.Peers[1].Name != "phone-1" || len(s.Renamed) != 1 || s.Renamed[0] != [2]string{"phone", "phone-1"} {
		t.Fatalf("renames: %v %v", []string{c.Peers[0].Name, c.Peers[1].Name}, s.Renamed)
	}
}

func TestPivpnInstallQuestion(t *testing.T) {
	root, _, _ := pivpnFixture(t, true, []pivpnClient{
		{name: "iphone-alex", ipv4: "10.6.0.2", v6: "fd11:5ee:bad:c0de::a06:2", psk: true},
	})
	s, err := readPivpn(root)
	if err != nil {
		t.Fatal(err)
	}
	cur := importedConfig(t, s)
	hash, _ := hashPassword("a long test password")
	cur.Users[0].PasswordHash = hash

	// Yes, then Enter keeps pivpn's endpoint and port: no device needs a new config.
	p, err := askInstall(strings.NewReader("y\n\n\n\ny\n"), cur, false, map[string]bool{}, installPlan{pivpn: s})
	if err != nil || p.endpoint != "" || p.port != 0 || p.reissueCount(cur, false) != 0 {
		t.Fatalf("Enter should keep pivpn's settings: %+v %v", p, err)
	}
	// A new port means the device needs a new config.
	p, err = askInstall(strings.NewReader("y\n\n\n51900\ny\n"), cur, false, map[string]bool{}, installPlan{pivpn: s})
	if err != nil || p.reissueCount(cur, false) != 1 {
		t.Fatalf("a new port should need a new config: %+v %v", p, err)
	}
	// No: nothing changes, and install explains why.
	if _, err := askInstall(strings.NewReader("n\n"), cur, false, map[string]bool{}, installPlan{pivpn: s}); !errors.Is(err, errPivpnDeclined) {
		t.Fatalf("want errPivpnDeclined, got %v", err)
	}
	// -import-pivpn answers the question.
	if _, err := askInstall(strings.NewReader("\n\n\ny\n"), cur, false, map[string]bool{"import-pivpn": true}, installPlan{pivpn: s}); err != nil {
		t.Fatal(err)
	}
}

func TestPeerIPv6Kept(t *testing.T) {
	c := testConfig(t)
	c.Server.IPv6Enabled = true
	c.Peers = []Peer{{ID: "a", Name: "a", IPv4: "10.84.12.2", PublicKey: "k1", IPv6: "fd11:5ee:bad:c0de::2"}}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"10.84.12.9", "fd00::2", "fd11:5ee:bad:c0de::", "not an address"} {
		c.Peers[0].IPv6 = bad
		if c.validate() == nil {
			t.Errorf("IPv6 %q should be refused", bad)
		}
	}
	// Two peers on the same IPv6 address.
	c.Peers[0].IPv6 = mapIPv6(netip.MustParsePrefix(c.Server.IPv6), netip.MustParseAddr("10.84.12.3")).String()
	c.Peers = append(c.Peers, Peer{ID: "b", Name: "b", IPv4: "10.84.12.3", PublicKey: "k2"})
	if c.validate() == nil {
		t.Error("an IPv6 address used twice should be refused")
	}
}
