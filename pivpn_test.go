package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
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
	v6, allowed := "0", "0.0.0.0/0"
	if ipv6 {
		v6, allowed = "1", "0.0.0.0/0, ::0/0"
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
ALLOWED_IPS="` + allowed + `"
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
	if c4.Server.IPv6Enabled || c4.Peers[0].IPv6 != "" || c4.Server.IPv6 != defaultIPv6 || strings.Join(c4.Server.ClientDefaults.AllowedIPs, ",") != "0.0.0.0/0,::/0" {
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
	c.Peers = []Peer{{ID: "a", Name: "a", IPv4: "10.84.12.2", PublicKey: "k1", IPv6: "fd00:b00b:5::2"}}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"10.84.12.9", "fd00::2", "fd00:b00b:5::", "not an address"} {
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

func TestPivpnWaitBack(t *testing.T) {
	pv := &pivpnSetup{Peers: []Peer{{Name: "a", PublicKey: "ka"}, {Name: "b", PublicKey: "kb"}}}
	since := time.Now()
	after := since.Add(time.Second)
	sample := func(back ...string) func() ([]PeerSample, error) {
		return func() ([]PeerSample, error) {
			var out []PeerSample
			for _, k := range back {
				out = append(out, PeerSample{PublicKey: k, LastHandshake: after})
			}
			// A handshake from before the switch does not count.
			return append(out, PeerSample{PublicKey: "kb", LastHandshake: since.Add(-time.Minute)}), nil
		}
	}
	// Everyone back: returns at once.
	start := time.Now()
	back, skipped := waitBack(pv, []string{"a", "b"}, sample("ka", "kb"), since, time.Minute, time.Millisecond, nil)
	if len(back) != 2 || skipped || time.Since(start) > time.Second {
		t.Fatalf("all back: %v %v", back, skipped)
	}
	// One missing: waits for the timeout.
	back, skipped = waitBack(pv, []string{"a", "b"}, sample("ka"), since, 50*time.Millisecond, 5*time.Millisecond, nil)
	if len(back) != 1 || back[0] != "a" || skipped {
		t.Fatalf("timeout: %v %v", back, skipped)
	}
	// Enter: returns at once, marked skipped.
	skip := make(chan struct{})
	close(skip)
	start = time.Now()
	back, skipped = waitBack(pv, []string{"a", "b"}, sample("ka"), time.Now(), time.Minute, time.Second, skip)
	if !skipped || time.Since(start) > time.Second {
		t.Fatalf("skip: %v %v", back, skipped)
	}
}

func TestCheckPivpn(t *testing.T) {
	pv := &pivpnSetup{Dev: "wg0"}
	for _, tc := range []struct {
		name                               string
		existing, importPivpn, interactive bool
		pv                                 *pivpnSetup
		unit                               string
		want                               string // part of the error, "" for none
	}{
		{"plain new install", false, false, false, nil, "", ""},
		{"takeover in a terminal", false, false, true, pv, "", ""},
		{"takeover with -import-pivpn", false, true, false, pv, "", ""},
		{"pivpn without terminal or flag", false, false, false, pv, "", "add -import-pivpn"},
		{"-import-pivpn without pivpn", false, true, false, nil, "", "not found"},
		{"-import-pivpn on an existing install", true, true, false, nil, "", "new install"},
		{"existing, pivpn switched off", true, false, false, nil, "", ""},
		{"existing, pivpn switched on again", true, false, true, nil, "wg-quick@wg0", "systemctl disable --now wg-quick@wg0"},
		{"existing, pivpn on, with -import-pivpn", true, true, false, nil, "wg-quick@wg0", "is switched on"},
	} {
		err := checkPivpn(tc.existing, tc.importPivpn, tc.interactive, tc.pv, tc.unit)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestStdinPassword(t *testing.T) {
	read := func(in string) (string, error) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.WriteString(in)
		w.Close()
		old := os.Stdin
		os.Stdin = r
		defer func() { os.Stdin = old; r.Close() }()
		return stdinPassword("admin")
	}
	for _, in := range []string{"a long test password\n", "a long test password"} {
		hash, err := read(in)
		if err != nil || !verifyPassword(hash, "a long test password") {
			t.Errorf("%q: %v", in, err)
		}
	}
	for _, in := range []string{"", "\n", "short\n"} {
		if _, err := read(in); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
			t.Errorf("%q should be refused before any change: %v", in, err)
		}
	}
}

func TestPivpnNATLines(t *testing.T) {
	rulesV4 := `*nat
:POSTROUTING ACCEPT [0:0]
-A POSTROUTING -s 10.6.0.0/24 -o eth0 -m comment --comment wireguard-nat-rule -j MASQUERADE
-A POSTROUTING -s 172.17.0.0/16 ! -o docker0 -j MASQUERADE
COMMIT
`
	off, lines := switchNATLines(rulesV4, true)
	if len(lines) != 1 || !strings.Contains(off, natOffMark+"-A POSTROUTING -s 10.6.0.0/24") || !strings.Contains(off, "\n-A POSTROUTING -s 172.17.0.0/16") {
		t.Fatalf("switching off:\n%s", off)
	}
	if again, lines := switchNATLines(off, true); again != off || len(lines) != 0 {
		t.Error("switching off twice should change nothing")
	}
	on, lines := switchNATLines(off, false)
	if on != rulesV4 || len(lines) != 1 {
		t.Fatalf("switching on should give the original back:\n%s", on)
	}
	want := []string{"POSTROUTING", "-s", "10.6.0.0/24", "-o", "eth0", "-m", "comment", "--comment", "wireguard-nat-rule", "-j", "MASQUERADE"}
	if r := pivpnNATRules(strings.Join(lines, "")); len(r) != 1 || !slices.Equal(r[0], want) {
		t.Errorf("rule: %q", r)
	}

	// ufw's before.rules (-I), and iptables -S output with other rules.
	ufw := "-I POSTROUTING -s 10.6.0.0/24 -o eth0 -j MASQUERADE -m comment --comment wireguard-nat-rule\n"
	if r := pivpnNATRules(ufw); len(r) != 1 || r[0][0] != "POSTROUTING" {
		t.Errorf("ufw rule: %q", r)
	}
	s := "-P POSTROUTING ACCEPT\n-A POSTROUTING -s 172.17.0.0/16 ! -o docker0 -j MASQUERADE\n" +
		"-A POSTROUTING -s fd11:5ee:bad:c0de::/64 -o eth0 -m comment --comment wireguard-nat-rule -j MASQUERADE\n" +
		`-A POSTROUTING -m comment --comment "wireguard-nat-rule copy" -j MASQUERADE` + "\n"
	if r := pivpnNATRules(s); len(r) != 1 || r[0][2] != "fd11:5ee:bad:c0de::/64" {
		t.Errorf("iptables -S: %q", r)
	}
}

func TestUnattendedEndpoint(t *testing.T) {
	detected := func(context.Context) (netip.Addr, error) { return netip.MustParseAddr("203.0.113.7"), nil }
	failed := func(context.Context) (netip.Addr, error) { return netip.Addr{}, errors.New("no network") }
	cur := testConfig(t)
	cur.Server.Endpoint = ""

	for _, tc := range []struct {
		name     string
		plan     installPlan
		endpoint string // current endpoint
		domain   string // current domain
		want     string
	}{
		{"new install without flags", installPlan{}, "", "", "203.0.113.7"},
		{"-endpoint given", installPlan{endpoint: "vpn.example.net"}, "", "", "vpn.example.net"},
		{"-domain given (apply uses it)", installPlan{domain: "vpn.example.net"}, "", "", ""},
		{"endpoint already set", installPlan{}, "198.51.100.1", "", ""},
		{"domain already set", installPlan{}, "", "vpn.example.net", "vpn.example.net"},
	} {
		c := cur.clone()
		c.Server.Endpoint, c.Web.TLS.Domain = tc.endpoint, tc.domain
		got, err := unattendedEndpoint(tc.plan, c, detected)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
	if got, err := unattendedEndpoint(installPlan{}, cur, failed); err == nil || got != "" {
		t.Errorf("a failed detection should be reported: %q, %v", got, err)
	}
}
