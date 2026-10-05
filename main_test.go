package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMapIPv6(t *testing.T) {
	got := mapIPv6(netip.MustParsePrefix("fd11:5ee:bad:c0de::/64"), netip.MustParseAddr("10.84.12.8"))
	if got.String() != "fd11:5ee:bad:c0de::a54:c08" {
		t.Fatalf("got %s", got)
	}
}

func testConfig(t *testing.T) *Config {
	t.Helper()
	c := &Config{}
	c.applyDefaults()
	c.Server.IPv4 = "10.84.12.0/24"
	if _, err := c.initServer(); err != nil {
		t.Fatal(err)
	}
	c.Server.Endpoint = "vpn.example.net"
	return c
}

func TestNextFreeIPv4(t *testing.T) {
	c := testConfig(t)
	ip, err := nextFreeIPv4(c)
	if err != nil || ip.String() != "10.84.12.2" {
		t.Fatalf("first free: %v %v", ip, err)
	}
	c.Peers = []Peer{{IPv4: "10.84.12.2"}, {IPv4: "10.84.12.4"}}
	ip, _ = nextFreeIPv4(c)
	if ip.String() != "10.84.12.3" {
		t.Fatalf("gap not reused: %v", ip)
	}
	c.Server.IPv4 = "10.84.12.0/30" // .1 server, .2 the only peer address
	c.Peers = []Peer{{IPv4: "10.84.12.2"}}
	if _, err := nextFreeIPv4(c); err == nil {
		t.Fatal("expected full network")
	}
}

func TestValidate(t *testing.T) {
	c := testConfig(t)
	key, _ := newPrivateKey()
	ok := Peer{ID: "a", Name: "phone", IPv4: "10.84.12.2", PublicKey: key.PublicKey().String()}
	c.Peers = []Peer{ok}
	if err := c.validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for name, mutate := range map[string]func(c *Config){
		"duplicate name": func(c *Config) {
			p := ok
			p.ID, p.IPv4, p.PublicKey = "b", "10.84.12.3", "x"
			c.Peers = append(c.Peers, p)
		},
		"duplicate ip":    func(c *Config) { p := ok; p.ID, p.Name, p.PublicKey = "b", "other", "x"; c.Peers = append(c.Peers, p) },
		"server address":  func(c *Config) { c.Peers[0].IPv4 = "10.84.12.1" },
		"outside network": func(c *Config) { c.Peers[0].IPv4 = "10.84.13.2" },
		"bad name":        func(c *Config) { c.Peers[0].Name = "has space" },
		"digits only":     func(c *Config) { c.Peers[0].Name = "1234" },
		"bad dns":         func(c *Config) { c.Peers[0].DNS = []string{"dns.example"} },
		"bad port":        func(c *Config) { c.Server.ListenPort = 70000 },
		"unmasked net":    func(c *Config) { c.Server.IPv4 = "10.84.12.5/24" },
		"update source":   func(c *Config) { c.Updates.Source = "sourceforge" },
		// The endpoint goes into client configs: no extra lines.
		"endpoint newline": func(c *Config) { c.Server.Endpoint = "vpn.example.net\n[Interface]\nPreUp=id;#" },
		"endpoint tab":     func(c *Config) { c.Server.Endpoint = "vpn.example.net\tx" },
		"endpoint port":    func(c *Config) { c.Server.Endpoint = "vpn.example.net:51820" },
		"listen":           func(c *Config) { c.Web.Listen = "not-an-address" },
		"listen port":      func(c *Config) { c.Web.Listen = ":70000" },
		"http listen":      func(c *Config) { c.Web.HTTPListen = "80" },
		"session hours":    func(c *Config) { c.Web.SessionHours = -1 },
		"session too long": func(c *Config) { c.Web.SessionHours = 100000 },
	} {
		cc := c.clone()
		mutate(cc)
		if err := cc.validate(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	for _, ep := range []string{"vpn.example.net", "203.0.113.7", "2001:db8::1"} {
		cc := c.clone()
		cc.Server.Endpoint = ep
		if err := cc.validate(); err != nil {
			t.Errorf("endpoint %q rejected: %v", ep, err)
		}
	}
	for _, l := range []string{":443", "0.0.0.0:8443", "[::]:443", "localhost:8080"} {
		cc := c.clone()
		cc.Web.Listen = l
		if err := cc.validate(); err != nil {
			t.Errorf("listen %q rejected: %v", l, err)
		}
	}
}

func TestClientConfig(t *testing.T) {
	c := testConfig(t)
	c.Server.IPv6Enabled = true
	c.Server.ClientDefaults.Keepalive = 25
	p := Peer{Name: "phone", IPv4: "10.84.12.8", PresharedKey: "psk="}
	out := clientConfig(c, &p, "priv=")
	for _, want := range []string{
		"PrivateKey = priv=",
		"Address = 10.84.12.8/24,fd11:5ee:bad:c0de::a54:c08/64",
		"DNS = 9.9.9.9, 149.112.112.112",
		"PresharedKey = psk=",
		"Endpoint = vpn.example.net:51820",
		"AllowedIPs = 0.0.0.0/0, ::/0",
		"PersistentKeepalive = 25",
		"PublicKey = " + serverPublicKey(c),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("config lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "MTU") {
		t.Error("client config must not set an MTU")
	}
	zero := 0
	p.Keepalive = &zero
	if strings.Contains(clientConfig(c, &p, ""), "PersistentKeepalive") {
		t.Error("keepalive override 0 should remove the line")
	}
}

func TestPassword(t *testing.T) {
	h, err := hashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !verifyPassword(h, "correct horse battery") || verifyPassword(h, "wrong password!") {
		t.Fatal("password verification is wrong")
	}
}

func TestRenumber(t *testing.T) {
	c := testConfig(t)
	c.Peers = []Peer{{IPv4: "10.84.12.7"}}
	c.Server.IPv4 = "172.20.5.0/24"
	if err := renumberPeers(c, "10.84.12.0/24"); err != nil || c.Peers[0].IPv4 != "172.20.5.7" {
		t.Fatalf("renumber: %v %v", c.Peers[0].IPv4, err)
	}
	c.Server.IPv4 = "172.20.5.0/30"
	if err := renumberPeers(c, "172.20.5.0/24"); err == nil {
		t.Fatal("expected too-small error")
	}
}

func TestRandomSubnet(t *testing.T) {
	for range 50 {
		n, err := randomSubnet(24)
		if err != nil {
			t.Fatal(err)
		}
		if !n.Addr().IsPrivate() || overlapsAny(n, avoidedSubnets) {
			t.Fatalf("bad subnet %s", n)
		}
	}
}

// fakeKernel records applies and returns scripted counters.
type fakeKernel struct {
	applied int
	samples []PeerSample
}

func (k *fakeKernel) Apply(*Config) error                 { k.applied++; return nil }
func (k *fakeKernel) Sample(string) ([]PeerSample, error) { return k.samples, nil }
func (k *fakeKernel) Checks(*Config) []Check              { return []Check{{"fake", true, ""}} }
func (k *fakeKernel) Uplink(*Config, bool) string         { return "eth0" }
func (k *fakeKernel) Down(*Config) error                  { return nil }
func (k *fakeKernel) Close() error                        { return nil }
func (k *fakeKernel) Ping(dsts []netip.Addr, _ time.Duration) (map[netip.Addr]time.Duration, error) {
	out := map[netip.Addr]time.Duration{}
	for _, d := range dsts {
		out[d] = 20 * time.Millisecond
	}
	return out, nil
}

func TestStatsDeltas(t *testing.T) {
	dir := t.TempDir()
	store, err := openStore(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	key, _ := newPrivateKey()
	pub := key.PublicKey().String()
	if err := store.Update(func(c *Config) error {
		c.Peers = append(c.Peers, Peer{ID: "p1", Name: "phone", IPv4: serverIPv4(netip.MustParsePrefix(c.Server.IPv4)).Next().String(), PublicKey: pub, Enabled: true})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	k := &fakeKernel{}
	st, _ := openStats(filepath.Join(dir, "stats.json"), store, k)

	hs := time.Now()
	step := func(rx, tx int64) {
		k.samples = []PeerSample{{PublicKey: pub, RxBytes: rx, TxBytes: tx, LastHandshake: hs}}
		st.sample()
	}
	step(100, 1000)
	step(150, 1500) // +50 / +500
	step(20, 30)    // counter reset: counts 20 / 30
	s := st.Summary("p1")
	if s.UpTotal != 170 || s.DownTotal != 1530 {
		t.Fatalf("totals: up %d down %d", s.UpTotal, s.DownTotal)
	}
	if s.Down24h != 1530 || !s.Online {
		t.Fatalf("24h %d online %v", s.Down24h, s.Online)
	}
	st.save()
	st2, _ := openStats(filepath.Join(dir, "stats.json"), store, k)
	if st2.Summary("p1").DownTotal != 1530 {
		t.Fatal("stats not persisted")
	}
}

// TestAPI runs a full flow over HTTP: login, create, list, patch, issue,
// disable, delete, tokens.
func TestAPI(t *testing.T) {
	dir := t.TempDir()
	store, err := openStore(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := hashPassword("a long test password")
	_ = store.Update(func(c *Config) error {
		c.Users[0].PasswordHash = hash
		c.Server.Endpoint = "vpn.example.net"
		return nil
	})
	k := &fakeKernel{}
	st, _ := openStats(filepath.Join(dir, "stats.json"), store, k)
	app := &App{store: store, kernel: k, recon: newReconciler(k, store), stats: st, auth: newAuth(store),
		tls: &webTLS{}, logPath: filepath.Join(dir, "log.jsonl"), started: time.Now(), shutdown: func() {}}
	srv := httptest.NewServer(app.routes())
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	cl := &http.Client{Jar: jar}

	call := func(method, path string, body any, want int) map[string]any {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, srv.URL+"/api/v1"+path, rd)
		req.Header.Set("Content-Type", "application/json")
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != want {
			t.Fatalf("%s %s: status %d, want %d: %v", method, path, resp.StatusCode, want, out)
		}
		return out
	}

	call("GET", "/peers", nil, 401)
	call("POST", "/auth/login", map[string]string{"username": "admin", "password": "wrong"}, 401)
	call("POST", "/auth/login", map[string]string{"username": "admin", "password": "a long test password"}, 200)

	created := call("POST", "/peers", map[string]any{"name": "phone-alex", "note": "iPhone"}, 201)
	peer := created["peer"].(map[string]any)
	id := peer["id"].(string)
	if peer["ipv4"] != serverIPv4(netip.MustParsePrefix(store.Get().Server.IPv4)).Next().String() {
		t.Fatalf("unexpected address %v", peer["ipv4"])
	}
	conf := created["config"].(string)
	if !strings.Contains(conf, "PrivateKey = ") || strings.Contains(conf, "<the private key") || created["qr"] == "" {
		t.Fatal("issued config lacks the private key or QR")
	}
	// The private key must not be stored anywhere in config.json.
	priv := strings.TrimSpace(strings.SplitN(strings.SplitN(conf, "PrivateKey = ", 2)[1], "\n", 2)[0])
	raw, _ := json.Marshal(store.Get())
	if bytes.Contains(raw, []byte(priv)) {
		t.Fatal("client private key was stored")
	}

	call("POST", "/peers", map[string]any{"name": "phone-alex"}, 400) // duplicate name
	call("POST", "/peers", map[string]any{"name": "bad name"}, 400)

	call("PATCH", "/peers/"+id, map[string]any{"name": "phone-a", "keepalive": 0, "dns": []string{"1.1.1.1"}}, 200)
	got := call("GET", "/peers/"+id, nil, 200)
	if got["name"] != "phone-a" || got["effectiveKeepalive"].(float64) != 0 {
		t.Fatalf("patch not applied: %v", got)
	}
	call("PATCH", "/peers/"+id, map[string]any{"dns": nil}, 200)
	if call("GET", "/peers/"+id, nil, 200)["dns"] != nil {
		t.Fatal("dns override not cleared")
	}

	oldKey := got["publicKey"]
	re := call("POST", "/peers/"+id+"/issue-config", nil, 200)
	if re["peer"].(map[string]any)["publicKey"] == oldKey {
		t.Fatal("issue-config did not change the key")
	}

	call("POST", "/peers/"+id+"/disable", nil, 200)
	if call("GET", "/peers/"+id, nil, 200)["enabled"] != false {
		t.Fatal("not disabled")
	}
	call("GET", "/peers/"+id+"/stats?range=7d", nil, 200)
	call("GET", "/status", nil, 200)

	call("PATCH", "/server", map[string]any{"listenPort": 51821, "clientDefaults": map[string]any{"dns": []string{"1.1.1.1"}, "allowedIPs": []string{"0.0.0.0/0"}, "keepalive": 25}}, 200)
	if store.Get().Server.ListenPort != 51821 {
		t.Fatal("server patch not saved")
	}
	call("PATCH", "/server", map[string]any{"mtu": 100}, 400)

	tok := call("POST", "/tokens", map[string]string{"name": "iPhone app", "scope": "ro"}, 201)
	secret := tok["token"].(string)

	// Read-only token: GET works, changes are refused, admin endpoints too.
	bearer := func(method, path string, want int, body ...any) {
		var rd io.Reader
		if len(body) > 0 {
			b, _ := json.Marshal(body[0])
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, srv.URL+"/api/v1"+path, rd)
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("token %s %s: %d, want %d", method, path, resp.StatusCode, want)
		}
	}
	bearer("GET", "/peers", 200)
	bearer("DELETE", "/peers/"+id, 403)
	bearer("GET", "/tokens", 403)
	bearer("GET", "/peers/"+id+"/setup", 403) // the link would set up a device

	// A full-access token changes settings, but users, passwords, tokens,
	// the sign-in rules and backups need a signed-in user.
	secret = call("POST", "/tokens", map[string]string{"name": "full", "scope": "rw"}, 201)["token"].(string)
	uid := call("GET", "/auth/me", nil, 200)["id"].(string)
	bearer("PATCH", "/settings", 200, map[string]any{"log": store.Get().Log})
	bearer("PATCH", "/settings", 403, map[string]any{"signin": map[string]bool{"requireMfa": false}})
	bearer("GET", "/users", 403)
	bearer("POST", "/users", 403, map[string]any{"username": "eve", "password": "correct horse battery"})
	bearer("POST", "/users/"+uid+"/reset-password", 403, map[string]any{"password": "correct horse battery"})
	bearer("POST", "/users/"+uid+"/reset-mfa", 403)
	bearer("POST", "/auth/password", 403, map[string]string{"current": "x", "new": "y"})
	bearer("GET", "/tokens", 403)
	bearer("POST", "/tokens", 403, map[string]string{"name": "more", "scope": "rw"})
	bearer("DELETE", "/tokens/"+tok["id"].(string), 403)
	bearer("GET", "/backup", 403)
	bearer("GET", "/update-backups", 403)
	bearer("DELETE", "/update-backups", 403)

	// Config copies made by update: listed newest first, removed one by
	// one or all at once; nothing else in the folder can be removed.
	for i, v := range []string{"v0.3.2", "v0.4.0"} {
		f := filepath.Join(dir, "config.json.bak-"+v)
		_ = os.WriteFile(f, []byte("{}"), 0o600)
		_ = os.Chtimes(f, time.Now(), time.Now().Add(time.Duration(i-2)*time.Hour))
	}
	list := call("GET", "/update-backups", nil, 200)["backups"].([]any)
	if len(list) != 2 || list[0].(map[string]any)["version"] != "v0.4.0" {
		t.Fatalf("update backups: %v", list)
	}
	call("DELETE", "/update-backups/config.json", nil, 400)
	call("DELETE", "/update-backups/stats.json", nil, 400)
	call("DELETE", "/update-backups/config.json.bak-v9.9.9", nil, 400)
	call("DELETE", "/update-backups/config.json.bak-v0.3.2", nil, 200)
	if r := call("DELETE", "/update-backups", nil, 200); r["removed"] != float64(1) {
		t.Fatalf("remove all: %v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatal("config.json is gone:", err)
	}

	call("DELETE", "/peers/"+id, nil, 200)
	if len(store.Get().Peers) != 0 {
		t.Fatal("peer not deleted")
	}
	if k.applied == 0 {
		t.Fatal("kernel never applied")
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
		ok   bool
	}{
		{"0.2.0", "0.1.0", 1, true},
		{"v0.1.0", "0.1.0", 0, true},
		{"0.1.0-4-gabc123", "0.1.1", -1, true},
		{"1.10.0", "1.9.3", 1, true},
		{"abc123", "0.1.0", 0, false},
	} {
		got, ok := compareVersions(c.a, c.b)
		if got != c.want || ok != c.ok {
			t.Errorf("compare(%q, %q) = %d %v, want %d %v", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
}

func TestUnitFile(t *testing.T) {
	u := unitFile()
	for _, want := range []string{
		"User=ghostwire",
		"ExecStart=/opt/ghostwire/GHOSTWIRE -config /opt/ghostwire/config.json",
		"ReadWritePaths=/opt/ghostwire",
		"AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit lacks %q", want)
		}
	}
}

func TestSysctlConf(t *testing.T) {
	dir := t.TempDir()
	conf, sys := filepath.Join(dir, "conf"), filepath.Join(dir, "net")
	for name, ra := range map[string]string{"eth0": "1", "wlan0": "2", "eth1": "0", "br0": "1", "veth1": "1", "lo": "1"} {
		_ = os.MkdirAll(filepath.Join(conf, name), 0o755)
		_ = os.WriteFile(filepath.Join(conf, name, "accept_ra"), []byte(ra+"\n"), 0o644)
	}
	for _, name := range []string{"eth0", "wlan0", "eth1"} { // network cards
		_ = os.MkdirAll(filepath.Join(sys, name, "device"), 0o755)
	}
	_ = os.MkdirAll(filepath.Join(sys, "veth1"), 0o755)
	// br0 carries the default route; the lo line is the kernel's unreachable route.
	routes := filepath.Join(dir, "ipv6_route")
	_ = os.WriteFile(routes, []byte(
		"00000000000000000000000000000000 00 00000000000000000000000000000000 00 fe800000000000000000000000000001 00000400 00000001 00000000 00000003     br0\n"+
			"00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 ffffffff 00000001 00000000 00200200       lo\n"), 0o644)

	got := raInterfaces(conf, sys, routes)
	if want := []string{"br0", "eth0", "wlan0"}; !slices.Equal(got, want) {
		t.Fatalf("raInterfaces = %v, want %v", got, want)
	}
	c := sysctlConf(got)
	for _, want := range []string{"net.ipv6.conf.all.forwarding=1\n", "net.ipv6.conf.default.accept_ra=2\n", "net.ipv6.conf.eth0.accept_ra=2\n", "net.ipv6.conf.br0.accept_ra=2\n"} {
		if !strings.Contains(c, want) {
			t.Errorf("sysctl conf lacks %q:\n%s", want, c)
		}
	}
	if strings.Contains(c, "eth1") || strings.Contains(c, "veth1") {
		t.Errorf("sysctl conf names eth1 (accept_ra 0) or veth1 (virtual):\n%s", c)
	}
}

func TestLoginLockout(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := hashPassword("a long test password")
	_ = store.Update(func(c *Config) error { c.Users[0].PasswordHash = hash; return nil })
	a := newAuth(store)
	const right, wrong = "a long test password", "a wrong password"

	// Ten wrong attempts at once from one /64: five are checked, the others
	// are locked out before any password check.
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[string]int{}
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := a.Login("admin", wrong, fmt.Sprintf("2001:db8::%x", i+1))
			mu.Lock()
			got[err.Error()]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if got["wrong username or password"] != 5 || got[errLocked.Error()] != 5 {
		t.Fatalf("parallel attempts: %v", got)
	}
	if _, _, err := a.Login("admin", right, "2001:db8::ffff"); !errors.Is(err, errLocked) {
		t.Fatalf("same /64: %v, want locked", err)
	}
	if _, _, err := a.Login("admin", right, "2001:db8:0:1::1"); err != nil {
		t.Fatalf("other /64: %v", err)
	}

	// A right password takes its own attempt back. With two-step sign-in
	// the earlier failures stay, so wrong codes still lead to the lockout.
	_ = store.Update(func(c *Config) error { c.Users[0].MFA = &UserMFA{TOTPSecret: newTOTPSecret()}; return nil })
	ip := "192.0.2.7"
	for range maxFailures - 1 {
		_, _, _ = a.Login("admin", wrong, ip)
	}
	if _, tk, err := a.Login("admin", right, ip); err != nil || tk == "" {
		t.Fatalf("5th attempt, right password: ticket %q, %v", tk, err)
	}
	if _, _, err := a.Login("admin", wrong, ip); err == nil || errors.Is(err, errLocked) {
		t.Fatalf("6th attempt: %v, want wrong password", err)
	}
	if _, _, err := a.Login("admin", right, ip); !errors.Is(err, errLocked) {
		t.Fatalf("7th attempt: %v, want locked", err)
	}
}

func TestWriteIfChanged(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.conf")
	if ch, err := writeIfChanged(p, "a\n", 0o644); !ch || err != nil {
		t.Fatal("first write should change", err)
	}
	if ch, _ := writeIfChanged(p, "a\n", 0o644); ch {
		t.Fatal("same content should not change")
	}
	if ch, _ := writeIfChanged(p, "b\n", 0o644); !ch {
		t.Fatal("new content should change")
	}
}

func TestStatsRetention(t *testing.T) {
	s := &Stats{data: statsFile{Peers: map[string]*peerStats{}}}
	now := time.Date(2026, 10, 3, 15, 30, 0, 0, time.Local)
	ps := &peerStats{}
	for h := 0; h < 72; h++ {
		ps.Hourly = append(ps.Hourly, bucket{T: hourStart(now.Add(-time.Duration(71-h) * time.Hour)), Rx: 1})
	}
	for d := 0; d < 30; d++ {
		ps.Daily = append(ps.Daily, bucket{T: dayStart(now.AddDate(0, 0, d-29)), Rx: 1})
	}
	s.data.Peers["p"] = ps
	s.prune(StatsConfig{HourlyHours: 24, DailyDays: 7}, now)
	if len(ps.Hourly) != 24 || ps.Hourly[23].T != hourStart(now) {
		t.Fatalf("hourly kept %d", len(ps.Hourly))
	}
	if len(ps.Daily) != 7 || ps.Daily[6].T != dayStart(now) {
		t.Fatalf("daily kept %d", len(ps.Daily))
	}
	if !s.dirty {
		t.Fatal("pruning should mark stats dirty")
	}
}

func TestLogSetLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.jsonl")
	w, err := newRotatingWriter(path, 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for i := 1; i <= 5; i++ {
		if err := os.WriteFile(fmt.Sprintf("%s.%d", path, i), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w.SetLimits(2, 2)
	for i := 1; i <= 5; i++ {
		_, err := os.Stat(fmt.Sprintf("%s.%d", path, i))
		if exists := err == nil; exists != (i <= 2) {
			t.Errorf("file .%d exists=%v", i, exists)
		}
	}
}

func TestRetentionValidation(t *testing.T) {
	c := testConfig(t)
	for name, mutate := range map[string]func(c *Config){
		"log size":  func(c *Config) { c.Log.MaxSizeMB = 0 },
		"log files": func(c *Config) { c.Log.MaxFiles = 101 },
		"log level": func(c *Config) { c.Log.Level = "loud" },
		"hourly":    func(c *Config) { c.Stats.HourlyHours = 12 },
		"daily":     func(c *Config) { c.Stats.DailyDays = 5000 },
	} {
		cc := c.clone()
		mutate(cc)
		if cc.validate() == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestSessions(t *testing.T) {
	s := &Stats{data: statsFile{Peers: map[string]*peerStats{}}}
	ps := &peerStats{}
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	at := func(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }
	smp := func(min int, ep string) PeerSample { return PeerSample{Endpoint: ep, LastHandshake: at(min)} }

	s.track(ps, smp(0, "192.168.1.20:5000"), 10, 100, at(0)) // online from home
	s.track(ps, smp(2, "192.168.1.20:5001"), 10, 100, at(2)) // same network, new port
	if len(ps.Sessions) != 1 || ps.Sessions[0].Rx != 20 || ps.Sessions[0].Endpoint != "192.168.1.20:5001" {
		t.Fatalf("same network should continue the session: %+v", ps.Sessions)
	}
	if g := ps.Sessions[0].Geo; g == nil || g.Network != "Local network" {
		t.Fatalf("private address should be the local network: %+v", g)
	}
	s.track(ps, smp(4, "198.51.100.7:6000"), 5, 50, at(4)) // roamed to mobile
	if len(ps.Sessions) != 2 || ps.Sessions[0].Open || !ps.Sessions[1].Open {
		t.Fatalf("roaming should start a new session: %+v", ps.Sessions)
	}
	s.track(ps, smp(4, "198.51.100.7:6000"), 0, 0, at(10)) // no handshake for 6 min
	if ps.openSession() != nil {
		t.Fatal("session should end when the peer goes quiet")
	}
	if ps.Sessions[1].End != at(4) {
		t.Fatalf("end should be the last time seen online, got %v", ps.Sessions[1].End)
	}

	// A peer missing from the kernel (disabled) gets its session closed, and
	// sessions older than the daily retention are pruned.
	ps.Sessions = append(ps.Sessions, connSession{Start: at(20), End: at(20), Open: true, Endpoint: "198.51.100.7:6000"})
	s.data.Peers["p"] = ps
	s.prune(StatsConfig{HourlyHours: 24, DailyDays: 7}, t0.AddDate(0, 0, 30))
	if len(ps.Sessions) != 1 || !ps.Sessions[0].Open {
		t.Fatalf("prune should keep only the open session: %+v", ps.Sessions)
	}
	if v := s.Sessions("p", 10); len(v) != 1 || v[0].IP != "198.51.100.7" {
		t.Fatalf("sessions view: %+v", v)
	}
}

func TestGeoLookupWithoutDatabase(t *testing.T) {
	var g *Geo // no databases: private addresses still resolve
	if info := g.Lookup("10.1.2.3:51820"); info == nil || info.Network != "Local network" {
		t.Fatalf("private: %+v", info)
	}
	if info := g.Lookup("[2001:db8::1]:51820"); info != nil {
		t.Fatalf("public without database should be unknown: %+v", info)
	}
	g2 := newGeo(t.TempDir(), false)
	if info := g2.Lookup("203.0.113.9:1"); info != nil {
		t.Fatalf("disabled: %+v", info)
	}
}

// TestGeoDatabase runs only with GHOSTWIRE_GEO_DIR pointing at a folder with
// downloaded geo-country.mmdb and geo-asn.mmdb.
func TestGeoDatabase(t *testing.T) {
	dir := os.Getenv("GHOSTWIRE_GEO_DIR")
	if dir == "" {
		t.Skip("GHOSTWIRE_GEO_DIR not set")
	}
	g := newGeo(dir, true)
	defer g.Close()
	for _, ep := range []string{"9.9.9.9:53", "1.1.1.1:53", "[2620:fe::fe]:53"} {
		info := g.Lookup(ep)
		if info == nil || info.Country == "" || info.Network == "" {
			t.Errorf("%s: %+v", ep, info)
		}
		t.Logf("%s → %+v", ep, info)
	}
}

// TestSetupLink creates a peer with a link, checks PIN handling and that the
// link works exactly once without storing the private key.
func TestSetupLink(t *testing.T) {
	dir := t.TempDir()
	store, err := openStore(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := hashPassword("a long test password")
	_ = store.Update(func(c *Config) error {
		c.Users[0].PasswordHash = hash
		c.Server.Endpoint = "vpn.example.net"
		return nil
	})
	k := &fakeKernel{}
	st, _ := openStats(filepath.Join(dir, "stats.json"), store, k)
	app := &App{store: store, kernel: k, recon: newReconciler(k, store), stats: st, auth: newAuth(store),
		tls: &webTLS{}, logPath: filepath.Join(dir, "log.jsonl"), started: time.Now(), shutdown: func() {}}
	srv := httptest.NewServer(app.routes())
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	admin := &http.Client{Jar: jar}

	call := func(cl *http.Client, method, path string, body any, want int) map[string]any {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, srv.URL+path, rd)
		req.Header.Set("Content-Type", "application/json")
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != want {
			t.Fatalf("%s %s: status %d, want %d: %v", method, path, resp.StatusCode, want, out)
		}
		return out
	}
	call(admin, "POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "a long test password"}, 200)

	// Two peers waiting for setup have no key yet; that must not clash.
	created := call(admin, "POST", "/api/v1/peers", map[string]any{"name": "phone-anna", "delivery": "link", "linkHours": 24}, 201)
	call(admin, "POST", "/api/v1/peers", map[string]any{"name": "laptop-ben", "delivery": "link", "linkPIN": false}, 201)
	call(admin, "POST", "/api/v1/peers", map[string]any{"name": "x", "delivery": "link", "linkHours": 5}, 400)
	peer := created["peer"].(map[string]any)
	id := peer["id"].(string)
	if peer["publicKey"] != "" || created["config"] != nil {
		t.Fatalf("link peer got keys or a config: %v", created)
	}
	setup := created["setup"].(map[string]any)
	path, pin := setup["path"].(string), setup["pin"].(string)
	if len(pin) != setupPINLen || !strings.HasPrefix(setup["url"].(string), srv.URL+"/setup/") || setup["qr"] == "" {
		t.Fatalf("bad setup answer: %v", setup)
	}
	if s := call(admin, "GET", "/api/v1/peers/"+id+"/setup", nil, 200); s["pin"] != pin {
		t.Fatal("admin cannot read the link again")
	}

	public := &http.Client{}
	api := strings.Replace(path, "/setup/", "/api/v1/setup/", 1)
	if info := call(public, "GET", api, nil, 200); info["name"] != "phone-anna" || info["pinRequired"] != true {
		t.Fatalf("setup info: %v", info)
	}
	call(public, "GET", "/api/v1/setup/nonsense", nil, 404)
	call(public, "GET", path, nil, 200) // the page itself

	wrong := "0000"
	if wrong == pin {
		wrong = "1111"
	}
	if r := call(public, "POST", api, map[string]string{"pin": wrong}, 403); r["triesLeft"].(float64) != setupMaxFails-1 {
		t.Fatalf("wrong PIN: %v", r)
	}
	got := call(public, "POST", api, map[string]string{"pin": pin}, 200)
	conf := got["config"].(string)
	if !strings.Contains(conf, "PrivateKey = ") || got["qr"] == "" {
		t.Fatal("redeemed config lacks the private key or QR")
	}
	priv := strings.TrimSpace(strings.SplitN(strings.SplitN(conf, "PrivateKey = ", 2)[1], "\n", 2)[0])
	raw, _ := json.Marshal(store.Get())
	if bytes.Contains(raw, []byte(priv)) {
		t.Fatal("client private key was stored")
	}
	call(public, "POST", api, map[string]string{"pin": pin}, 404) // works once
	call(public, "GET", api, nil, 404)
	p := call(admin, "GET", "/api/v1/peers/"+id, nil, 200)
	if p["publicKey"] == "" || p["setup"] != nil || p["configIssued"] == nil {
		t.Fatalf("peer not set up: %v", p)
	}

	// Re-issue by link: the old key stays until the link is used.
	oldKey := p["publicKey"]
	re := call(admin, "POST", "/api/v1/peers/"+id+"/issue-config", map[string]any{"delivery": "link"}, 200)
	if re["peer"].(map[string]any)["publicKey"] != oldKey {
		t.Fatal("issuing a link replaced the key early")
	}
	api = strings.Replace(re["setup"].(map[string]any)["path"].(string), "/setup/", "/api/v1/setup/", 1)
	for i := 0; i < setupMaxFails; i++ {
		want := 403
		if i == setupMaxFails-1 {
			want = 404 // revoked by the last wrong PIN
		}
		call(public, "POST", api, map[string]string{"pin": "x"}, want)
	}
	if p := call(admin, "GET", "/api/v1/peers/"+id, nil, 200); p["setup"] != nil || p["publicKey"] != oldKey {
		t.Fatalf("link not revoked after wrong PINs: %v", p)
	}

	// Revoking by hand.
	call(admin, "POST", "/api/v1/peers/"+id+"/issue-config", map[string]any{"delivery": "link"}, 200)
	call(admin, "DELETE", "/api/v1/peers/"+id+"/setup", nil, 200)
	call(admin, "GET", "/api/v1/peers/"+id+"/setup", nil, 404)
}

// TestInstallQuestions answers the interactive install's questions.
func TestInstallQuestions(t *testing.T) {
	hash, _ := hashPassword("a long test password")
	fresh := func() *Config {
		c := &Config{}
		c.applyDefaults()
		c.Users[0].PasswordHash = hash // skips the password question
		return c
	}

	// New install: domain, email, endpoint from the domain, own port. A bad
	// port is asked again.
	in := "vpn.example.net\nyou@example.net\n\nabc\n70000\n51900\ny\n"
	p, err := askInstall(strings.NewReader(in), fresh(), false, map[string]bool{}, installPlan{ipv4: "10.9.8.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	c := fresh()
	p.apply(c)
	if c.Web.TLS.Mode != "acme" || c.Web.TLS.Domain != "vpn.example.net" || c.Web.TLS.Email != "you@example.net" ||
		c.Server.Endpoint != "vpn.example.net" || c.Server.ListenPort != 51900 {
		t.Fatalf("answers not applied: %+v %+v", c.Web.TLS, c.Server)
	}

	// Re-run with a device: Enter keeps everything, so no change.
	cur := fresh()
	p.apply(cur)
	cur.Peers = []Peer{{ID: "a", Name: "phone", IPv4: "10.9.8.2", PublicKey: "k"}}
	p2, err := askInstall(strings.NewReader("\n\n\n\ny\n"), cur, true, map[string]bool{}, installPlan{})
	if err != nil || p2.changes() {
		t.Fatalf("Enter should keep the settings: %+v %v", p2, err)
	}

	// Changing the port warns about the device; answering n cancels.
	p3, err := askInstall(strings.NewReader("\n\n\n51820\nn\n"), cur, true, map[string]bool{}, installPlan{})
	if !errors.Is(err, errCancelled) {
		t.Fatalf("want cancel, got %v", err)
	}
	if p3.reissueCount(cur, true) != 1 {
		t.Fatal("port change should need a new config for the device")
	}

	// "none" turns the domain off; given flags are not asked.
	p4, err := askInstall(strings.NewReader("none\n\ny\n"), cur, true, map[string]bool{"port": true}, installPlan{})
	if err != nil || !p4.noDomain {
		t.Fatalf("none should remove the domain: %+v %v", p4, err)
	}
	c = cur.clone()
	p4.apply(c)
	if c.Web.TLS.Mode != "selfsigned" || c.Web.TLS.Domain != "" || c.Server.Endpoint != "vpn.example.net" {
		t.Fatalf("domain not removed: %+v", c.Web.TLS)
	}

	// Flags are checked before anything changes.
	for _, bad := range []installPlan{{port: 70000}, {domain: "not a domain"}, {email: "nope"}, {endpoint: "host:51820"}} {
		if bad.check() == nil {
			t.Errorf("%+v should be rejected", bad)
		}
	}
}

func TestLatency(t *testing.T) {
	dir := t.TempDir()
	store, err := openStore(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	key, _ := newPrivateKey()
	pub := key.PublicKey().String()
	ip := serverIPv4(netip.MustParsePrefix(store.Get().Server.IPv4)).Next()
	if err := store.Update(func(c *Config) error {
		c.Peers = append(c.Peers, Peer{ID: "p1", Name: "phone", IPv4: ip.String(), PublicKey: pub, Enabled: true, LatencyCheck: latencyActive})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) error { c.Peers[0].LatencyCheck = "sometimes"; return nil }); err == nil {
		t.Fatal("invalid latency check accepted")
	}
	k := &fakeKernel{}
	st, _ := openStats(filepath.Join(dir, "stats.json"), store, k)
	now := time.Now()
	k.samples = []PeerSample{{PublicKey: pub, RxBytes: 100, LastHandshake: now}}
	st.sample()
	k.samples[0].RxBytes += 200 // below activeRxBytes: idle, e.g. keepalives
	st.sample()
	if got := st.pingTargets(store.Get(), now); len(got) != 0 {
		t.Fatalf("idle peer pinged: %v", got)
	}
	k.samples[0].RxBytes += 50_000
	st.sample()
	if got := st.pingTargets(store.Get(), time.Now()); got[ip] != "p1" {
		t.Fatalf("active peer not pinged: %v", got)
	}

	st.mu.Lock()
	for _, ms := range []int{30, 10, 20} {
		st.record("p1", now, time.Duration(ms)*time.Millisecond, true)
	}
	st.record("p1", now, 0, false)
	st.mu.Unlock()
	l := st.Summary("p1").Latency
	if l == nil || l.MS == nil || *l.MS != 20 || l.Min != 10 || l.Max != 30 || l.Loss != 25 {
		t.Fatalf("latency %+v", l)
	}
	h := st.LatencyHistory("p1")
	if last := h[len(h)-1]; last.Sent != 4 || last.Lost != 1 || last.Med != 20 || last.RTTs != nil {
		t.Fatalf("history %+v", last)
	}
}

// TestConfigMigration turns a version 1 config with one admin into users.
func TestConfigMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	hash, _ := hashPassword("a long test password")
	old := `{"version":1,"admin":{"username":"dan","passwordHash":"` + hash + `"},` +
		`"apiTokens":[{"id":"t1","name":"iPhone","hash":"sha256:x","scope":"rw"}]}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	c := store.Get()
	if c.Version != 2 || c.Admin != nil || len(c.Users) != 1 || c.Users[0].Username != "dan" || c.Users[0].PasswordHash != hash {
		t.Fatalf("not migrated: %+v", c.Users)
	}
	if c.APITokens[0].UserID != c.Users[0].ID {
		t.Fatal("token not given to the migrated user")
	}
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte(`"admin":`)) {
		t.Fatal("old admin block still saved")
	}
}

// TestUsers covers adding, the forced password change, resets, renames and
// deleting users over HTTP.
func TestUsers(t *testing.T) {
	dir := t.TempDir()
	store, err := openStore(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := hashPassword("a long test password")
	_ = store.Update(func(c *Config) error { c.Users[0].PasswordHash = hash; return nil })
	k := &fakeKernel{}
	st, _ := openStats(filepath.Join(dir, "stats.json"), store, k)
	app := &App{store: store, kernel: k, recon: newReconciler(k, store), stats: st, auth: newAuth(store),
		tls: &webTLS{}, logPath: filepath.Join(dir, "log.jsonl"), started: time.Now(), shutdown: func() {}}
	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	// client returns a call function with its own cookie jar.
	client := func() func(method, path string, body any, want int) map[string]any {
		jar, _ := cookiejar.New(nil)
		cl := &http.Client{Jar: jar}
		return func(method, path string, body any, want int) map[string]any {
			t.Helper()
			var rd io.Reader
			if body != nil {
				b, _ := json.Marshal(body)
				rd = bytes.NewReader(b)
			}
			req, _ := http.NewRequest(method, srv.URL+"/api/v1"+path, rd)
			req.Header.Set("Content-Type", "application/json")
			resp, err := cl.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var out map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&out)
			if resp.StatusCode != want {
				t.Fatalf("%s %s: status %d, want %d: %v", method, path, resp.StatusCode, want, out)
			}
			return out
		}
	}
	admin := client()
	admin("POST", "/auth/login", map[string]string{"username": "ADMIN", "password": "a long test password"}, 200) // any letter case
	me := admin("GET", "/auth/me", nil, 200)
	myID := me["id"].(string)
	if sess, _ := me["session"].(map[string]any); sess == nil || sess["ip"] != "127.0.0.1" || me["username"] != "admin" || me["created"] == nil {
		t.Fatalf("me lacks the session or profile: %v", me)
	}

	// A new user with a temporary password can only change it.
	eve := admin("POST", "/users", map[string]any{"username": "eve", "password": "temporary password 1"}, 201)["user"].(map[string]any)
	eveID := eve["id"].(string)
	if eve["mustChangePassword"] != true {
		t.Fatal("mustChangePassword should default to true")
	}
	admin("POST", "/users", map[string]any{"username": "Eve", "password": "temporary password 1"}, 400) // taken, any case
	admin("POST", "/users", map[string]any{"username": "short", "password": "short"}, 400)

	e := client()
	e("POST", "/auth/login", map[string]string{"username": "eve", "password": "temporary password 1"}, 200)
	if e("GET", "/auth/me", nil, 200)["mustChangePassword"] != true {
		t.Fatal("me should report the forced change")
	}
	e("GET", "/peers", nil, 403)
	e("POST", "/auth/password", map[string]string{"current": "temporary password 1", "new": "temporary password 1"}, 400)
	e("POST", "/auth/password", map[string]string{"current": "temporary password 1", "new": "eve's own password"}, 200)
	e("GET", "/peers", nil, 200) // the session continues after the change
	if sess, _ := e("GET", "/auth/me", nil, 200)["session"].(map[string]any); sess == nil || sess["started"] == nil {
		t.Fatal("session start lost after the password change")
	}

	// A user created without the flag can work at once.
	sam := admin("POST", "/users", map[string]any{"username": "sam", "password": "sam's password 123", "mustChangePassword": false}, 201)["user"].(map[string]any)
	s := client()
	s("POST", "/auth/login", map[string]string{"username": "sam", "password": "sam's password 123"}, 200)
	s("GET", "/peers", nil, 200)

	// A reset ends the user's sessions; the flag can be cleared later.
	admin("POST", "/users/"+eveID+"/reset-password", map[string]any{"password": "another temp pw 1"}, 200)
	e("GET", "/peers", nil, 401)
	admin("PATCH", "/users/"+eveID, map[string]any{"mustChangePassword": false, "username": "eve2"}, 200)
	e("POST", "/auth/login", map[string]string{"username": "eve2", "password": "another temp pw 1"}, 200)
	e("GET", "/peers", nil, 200)
	admin("POST", "/users/"+myID+"/reset-password", map[string]any{"password": "whatever password"}, 400) // own: use /auth/password

	// Deleting a user removes their tokens and ends their sessions.
	tok := s("POST", "/tokens", map[string]string{"name": "sam's phone", "scope": "rw"}, 201)
	if l := admin("GET", "/tokens", nil, 200)["tokens"].([]any); l[0].(map[string]any)["owner"] != "sam" {
		t.Fatalf("token owner: %v", l)
	}
	admin("DELETE", "/users/"+myID, nil, 400)
	admin("DELETE", "/users/"+sam["id"].(string), nil, 200)
	s("GET", "/peers", nil, 401)
	if len(store.Get().APITokens) != 0 {
		t.Fatalf("token of deleted user kept: %v", tok["name"])
	}
	if n := len(admin("GET", "/users", nil, 200)["users"].([]any)); n != 2 {
		t.Fatalf("users: %d, want 2", n)
	}
}

// TestDecoy checks that the decoy hides the web interface but leaves the API
// and live setup links alone.
func TestDecoy(t *testing.T) {
	dir := t.TempDir()
	store, err := openStore(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if store.Get().Decoy.Page != "nginx" {
		t.Fatalf("default decoy page %q", store.Get().Decoy.Page)
	}
	k := &fakeKernel{}
	st, _ := openStats(filepath.Join(dir, "stats.json"), store, k)
	app := &App{store: store, kernel: k, recon: newReconciler(k, store), stats: st, auth: newAuth(store),
		tls: &webTLS{}, logPath: filepath.Join(dir, "log.jsonl"), started: time.Now(), shutdown: func() {}}
	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	get := func(path string, want int) (string, http.Header) {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("GET %s: status %d, want %d", path, resp.StatusCode, want)
		}
		return string(b), resp.Header
	}
	set := func(fn func(c *Config)) {
		if err := store.Update(func(c *Config) error { fn(c); return nil }); err != nil {
			t.Fatal(err)
		}
	}

	b, _ := get("/", 200)
	if !strings.Contains(b, `"/app.js?v=`+assetHash["app.js"]+`"`) || !strings.Contains(b, `"/app.css?v=`+assetHash["app.css"]+`"`) {
		t.Fatalf("web interface not served with fingerprinted files: %q", b)
	}
	if _, h := get("/app.js?v="+assetHash["app.js"], 200); !strings.Contains(h.Get("Cache-Control"), "immutable") {
		t.Fatalf("fingerprinted app.js: %v", h)
	}
	if _, h := get("/app.js?v=old", 200); h.Get("Cache-Control") != "no-cache" {
		t.Fatalf("stale app.js cached: %v", h)
	}
	set(func(c *Config) {
		v4 := netip.MustParsePrefix(c.Server.IPv4)
		c.Peers = append(c.Peers, Peer{ID: "p1", Name: "phone", IPv4: v4.Addr().Next().Next().Next().String(), Setup: &SetupLink{Token: "live-token", Expires: time.Now().Add(time.Hour)}})
		c.Decoy.Enabled = true
	})

	b, h := get("/", 200)
	if !strings.Contains(b, "Welcome to nginx!") || h.Get("Server") != nginxServer || h.Get("Content-Security-Policy") != "" {
		t.Fatalf("nginx decoy: %q %v", b, h)
	}
	for _, p := range []string{"/app.js", "/app.css", "/favicon.svg", "/ShipporiMinchoB1-ExtraBold.woff2", "/setup/wrong", "/setup/wrong/app.css", "/setup/live-token/app.js"} {
		if b, _ := get(p, 404); strings.Contains(b, "GHOSTWIRE") || !strings.Contains(b, "404 Not Found") {
			t.Fatalf("%s leaks: %q", p, b)
		}
	}
	if b, _ := get("/setup/live-token", 200); !strings.Contains(b, `src="/setup/live-token/setup.js?v=`+assetHash["setup.js"]+`"`) {
		t.Fatalf("setup page files not under the link: %q", b)
	}
	get("/setup/live-token/app.css", 200)
	get("/api/v1/setup/live-token", 200)
	get("/api/v1/status", 401)

	set(func(c *Config) { c.Decoy.Page = "apache" })
	if b, _ := get("/nope", 404); !strings.Contains(b, "Apache/2.4.58 (Ubuntu) Server at 127.0.0.1 Port") {
		t.Fatalf("apache 404: %q", b)
	}
	set(func(c *Config) { c.Decoy.Page = "soon" })
	if b, h := get("/", 200); !strings.Contains(b, "<p class=\"host\">127.0.0.1</p>") || h.Get("Server") != "" {
		t.Fatalf("soon decoy: %q", b)
	}
	set(func(c *Config) { c.Decoy.Page = "blank" })
	if b, _ := get("/", 200); b != "" {
		t.Fatalf("blank decoy: %q", b)
	}
	if b, _ := get("/app.js", 404); b != "" {
		t.Fatalf("blank 404: %q", b)
	}
	set(func(c *Config) { c.Decoy.Page = "forbidden" })
	if b, _ := get("/", 403); !strings.Contains(b, "Forbidden") {
		t.Fatalf("forbidden decoy: %q", b)
	}
	set(func(c *Config) { c.Decoy.Page = "private" })
	if b, _ := get("/", 200); !strings.Contains(b, "Private server") {
		t.Fatalf("private decoy: %q", b)
	}
	if err := store.Update(func(c *Config) error { c.Decoy.Page = "iis"; return nil }); err == nil {
		t.Fatal("unknown decoy page accepted")
	}
}

func TestTOTPCode(t *testing.T) {
	// RFC 6238, appendix B (SHA-1), cut to 6 digits.
	key := []byte("12345678901234567890")
	for _, c := range []struct {
		unix int64
		want string
	}{{59, "287082"}, {1111111109, "081804"}, {1234567890, "005924"}, {2000000000, "279037"}} {
		if got := totpCode(key, uint64(c.unix/30)); got != c.want {
			t.Errorf("time %d: %s, want %s", c.unix, got, c.want)
		}
	}
	secret := b32.EncodeToString(key)
	now := time.Unix(1111111109, 0)
	if _, ok := totpMatch(secret, "081 804", now); !ok {
		t.Error("code with a space refused")
	}
	if _, ok := totpMatch(secret, "081804", now.Add(90*time.Second)); ok {
		t.Error("code three steps late accepted")
	}
}

// TestMFA signs in with an authenticator code and a recovery code, and
// checks the "require" switch and the admin reset.
func TestMFA(t *testing.T) {
	dir := t.TempDir()
	store, err := openStore(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := hashPassword("a long test password")
	_ = store.Update(func(c *Config) error { c.Users[0].PasswordHash = hash; return nil })
	k := &fakeKernel{}
	st, _ := openStats(filepath.Join(dir, "stats.json"), store, k)
	app := &App{store: store, kernel: k, recon: newReconciler(k, store), stats: st, auth: newAuth(store),
		tls: &webTLS{}, logPath: filepath.Join(dir, "log.jsonl"), started: time.Now(), shutdown: func() {}}
	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	client := func() func(method, path string, body any, want int) map[string]any {
		jar, _ := cookiejar.New(nil)
		cl := &http.Client{Jar: jar}
		return func(method, path string, body any, want int) map[string]any {
			t.Helper()
			var rd io.Reader
			if body != nil {
				b, _ := json.Marshal(body)
				rd = bytes.NewReader(b)
			}
			req, _ := http.NewRequest(method, srv.URL+"/api/v1"+path, rd)
			req.Header.Set("Content-Type", "application/json")
			resp, err := cl.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var out map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&out)
			if resp.StatusCode != want {
				t.Fatalf("%s %s: status %d, want %d: %v", method, path, resp.StatusCode, want, out)
			}
			return out
		}
	}
	login := map[string]string{"username": "admin", "password": "a long test password"}
	adm := client()
	adm("POST", "/auth/login", login, 200)
	if o := adm("GET", "/auth/options", nil, 200); o["passkeys"] != false {
		t.Fatalf("passkeys offered on an IP address: %v", o)
	}
	adm("POST", "/auth/mfa/keys/begin", nil, 400)

	// Turn on the authenticator app; the first method brings recovery codes.
	setup := adm("POST", "/auth/mfa/totp/setup", nil, 200)
	secret := setup["secret"].(string)
	if !strings.HasPrefix(setup["uri"].(string), "otpauth://totp/") || setup["qr"] == "" {
		t.Fatalf("setup: %v", setup)
	}
	adm("POST", "/auth/mfa/totp/confirm", map[string]string{"code": "000000"}, 400)
	key, _ := b32.DecodeString(secret)
	code := func(offset int) string { return totpCode(key, uint64(time.Now().Unix()/30)+uint64(offset)) }
	conf := adm("POST", "/auth/mfa/totp/confirm", map[string]string{"code": code(0)}, 200)
	codes := conf["recoveryCodes"].([]any)
	if len(codes) != recoveryCount {
		t.Fatalf("recovery codes: %v", conf)
	}
	if s := adm("GET", "/auth/mfa", nil, 200); s["totp"] != true || s["recoveryLeft"] != float64(recoveryCount) {
		t.Fatalf("status: %v", s)
	}

	// A password alone now gives a ticket, not a session.
	c := client()
	r := c("POST", "/auth/login", login, 200)
	ticket, _ := r["ticket"].(string)
	if r["mfa"] != true || ticket == "" {
		t.Fatalf("login without second step: %v", r)
	}
	c("GET", "/peers", nil, 401)
	c("POST", "/auth/login/totp", map[string]string{"ticket": ticket, "code": "123456"}, 401)
	c("POST", "/auth/login/totp", map[string]string{"ticket": ticket, "code": code(0)}, 401) // used during setup
	c("POST", "/auth/login/totp", map[string]string{"ticket": ticket, "code": code(1)}, 200)
	c("GET", "/peers", nil, 200)

	// A recovery code works once.
	c2 := client()
	ticket = c2("POST", "/auth/login", login, 200)["ticket"].(string)
	c2("POST", "/auth/login/recovery", map[string]string{"ticket": ticket, "code": strings.ToLower(codes[0].(string))}, 200)
	c3 := client()
	ticket = c3("POST", "/auth/login", login, 200)["ticket"].(string)
	c3("POST", "/auth/login/recovery", map[string]string{"ticket": ticket, "code": codes[0].(string)}, 401)
	c3("POST", "/auth/login/recovery", map[string]string{"ticket": ticket, "code": codes[1].(string)}, 200)

	// Session length needs no restart; the listen address does.
	web := store.Get().Web
	web.SessionHours = 24
	if r := adm("PATCH", "/settings", map[string]any{"web": web}, 200); r["restartRequired"] != false || store.Get().Web.SessionHours != 24 {
		t.Fatalf("session length: %v", r)
	}
	web.Listen = "127.0.0.1:9443"
	if r := adm("PATCH", "/settings", map[string]any{"web": web}, 200); r["restartRequired"] != true {
		t.Fatalf("listen address: %v", r)
	}

	// Required for everyone: a user without it can only set it up.
	adm("PATCH", "/settings", map[string]any{"signin": map[string]bool{"requireMfa": true}}, 200)
	u := adm("POST", "/users", map[string]any{"username": "eve", "password": "eve's password 1", "mustChangePassword": false}, 201)["user"].(map[string]any)
	e := client()
	e("POST", "/auth/login", map[string]string{"username": "eve", "password": "eve's password 1"}, 200)
	if me := e("GET", "/auth/me", nil, 200); me["mfaSetupRequired"] != true {
		t.Fatalf("me: %v", me)
	}
	e("GET", "/peers", nil, 403)
	e("GET", "/auth/mfa", nil, 200)
	// The last method cannot be removed while it is required.
	adm("DELETE", "/auth/mfa/totp", nil, 400)

	// An admin resets another user's two-step sign-in, not their own.
	_ = store.Update(func(c *Config) error {
		_, eu := c.userByID(u["id"].(string))
		eu.MFA = &UserMFA{TOTPSecret: newTOTPSecret(), RecoveryCodes: []string{"x"}}
		return nil
	})
	if l := adm("GET", "/users", nil, 200)["users"].([]any); l[1].(map[string]any)["mfa"].(map[string]any)["totp"] != true {
		t.Fatalf("users list: %v", l)
	}
	me := adm("GET", "/auth/me", nil, 200)
	adm("POST", "/users/"+me["id"].(string)+"/reset-mfa", nil, 400)
	adm("POST", "/users/"+u["id"].(string)+"/reset-mfa", nil, 200)
	if _, eu := store.Get().userByID(u["id"].(string)); eu.hasMFA() || len(eu.MFA.RecoveryCodes) != 0 {
		t.Fatal("reset left methods behind")
	}
}

// TestDropSecurityKeys checks that security keys from v0.3.0 are deleted on
// load, and recovery codes with them when nothing else is left.
func TestDropSecurityKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := `{"users": [
		{"id": "a", "username": "a", "passwordHash": "x", "mfa": {"keys": [{"id": "k", "name": "YubiKey", "passkey": false}], "recoveryCodes": ["h"]}},
		{"id": "b", "username": "b", "passwordHash": "x", "mfa": {"keys": [{"id": "k1", "name": "YubiKey", "passkey": false}, {"id": "k2", "name": "Mac", "passkey": true}], "recoveryCodes": ["h"]}}
	]}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	c := store.Get()
	if a := c.Users[0].MFA; len(a.Keys) != 0 || len(a.RecoveryCodes) != 0 {
		t.Fatalf("user a kept %v", a)
	}
	if b := c.Users[1].MFA; len(b.Keys) != 1 || b.Keys[0].Name != "Mac" || len(b.RecoveryCodes) != 1 {
		t.Fatalf("user b: %v", b)
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "YubiKey") {
		t.Fatal("security key still in config.json")
	}
}

func TestSpeeds(t *testing.T) {
	dir := t.TempDir()
	store, err := openStore(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	key, _ := newPrivateKey()
	pub := key.PublicKey().String()
	if err := store.Update(func(c *Config) error {
		c.Peers = append(c.Peers, Peer{ID: "p1", Name: "phone", IPv4: serverIPv4(netip.MustParsePrefix(c.Server.IPv4)).Next().String(), PublicKey: pub, Enabled: true})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	k := &fakeKernel{}
	sp := newSpeeds(store, k)
	t0 := time.Unix(1_800_000_000, 0)
	step := func(sec int, rx, tx int64) {
		k.samples = []PeerSample{{PublicKey: pub, RxBytes: rx, TxBytes: tx}}
		sp.sample(t0.Add(time.Duration(sec) * time.Second))
	}
	_, ch, cancel := sp.Subscribe()
	defer cancel()
	step(0, 1000, 1000)
	if n := len(sp.Since(0)); n != 0 {
		t.Fatalf("first sample made %d points, want 0", n)
	}
	step(2, 1250, 3000) // +250 up, +2000 down in 2 s
	step(4, 10, 20)     // counter reset: no speed for this step
	pts := sp.Since(0)
	if len(pts) != 2 {
		t.Fatalf("got %d points, want 2", len(pts))
	}
	if got, want := pts[0].Peers["p1"], [2]int64{8000, 1000}; got != want {
		t.Fatalf("speed = %v, want %v (down, up in bit/s)", got, want)
	}
	if got := <-ch; got.T != pts[0].T {
		t.Fatalf("subscriber got step %d, want %d", got.T, pts[0].T)
	}
	if _, ok := pts[1].Peers["p1"]; ok {
		t.Fatal("a counter reset reported a speed")
	}
	if got := sp.Since(pts[0].T); len(got) != 1 || got[0].T != pts[1].T {
		t.Fatalf("Since returned %v", got)
	}
	for i := 0; i < speedPoints+5; i++ {
		step(6+2*i, 0, 0)
	}
	if n := len(sp.Since(0)); n != speedPoints {
		t.Fatalf("kept %d points, want %d", n, speedPoints)
	}
}

// signedInApp starts the API with a signed-in admin and returns the app
// and a call function.
func signedInApp(t *testing.T) (*App, func(method, path string, body any, want int) map[string]any) {
	t.Helper()
	dir := t.TempDir()
	store, err := openStore(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := hashPassword("a long test password")
	_ = store.Update(func(c *Config) error { c.Users[0].PasswordHash = hash; return nil })
	k := &fakeKernel{}
	st, _ := openStats(filepath.Join(dir, "stats.json"), store, k)
	app := &App{store: store, kernel: k, recon: newReconciler(k, store), stats: st, auth: newAuth(store),
		tls: &webTLS{}, logPath: filepath.Join(dir, "log.jsonl"), started: time.Now(), shutdown: func() {}}
	srv := httptest.NewServer(app.routes())
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	cl := &http.Client{Jar: jar}
	call := func(method, path string, body any, want int) map[string]any {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, srv.URL+"/api/v1"+path, rd)
		req.Header.Set("Content-Type", "application/json")
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != want {
			t.Fatalf("%s %s: status %d, want %d: %v", method, path, resp.StatusCode, want, out)
		}
		return out
	}
	call("POST", "/auth/login", map[string]string{"username": "admin", "password": "a long test password"}, 200)
	return app, call
}

// Web settings the service could not start with are refused before they
// are saved: a restart would otherwise take the web interface and the API
// down for good.
func TestWebSettingsCheck(t *testing.T) {
	app, call := signedInApp(t)
	web := func(change func(w *WebConfig)) map[string]any {
		w := app.store.Get().Web
		w.HTTPListen = ""
		change(&w)
		return map[string]any{"web": w}
	}
	call("PATCH", "/settings", web(func(w *WebConfig) { w.Listen = "not-an-address" }), 400)
	call("PATCH", "/settings", web(func(w *WebConfig) { w.SessionHours = -1 }), 400)
	call("PATCH", "/settings", web(func(w *WebConfig) {
		w.TLS = TLSConfig{Mode: "files", CertFile: "/nonexistent/cert.pem", KeyFile: "/nonexistent/key.pem"}
	}), 400)

	// A port another program holds is refused; a free one is saved.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	call("PATCH", "/settings", web(func(w *WebConfig) { w.Listen = busy.Addr().String() }), 400)
	free, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := free.Addr().String()
	free.Close()
	call("PATCH", "/settings", web(func(w *WebConfig) { w.Listen = addr; w.TLS = TLSConfig{Mode: "off"} }), 200)
	if app.store.Get().Web.Listen != addr {
		t.Fatal("valid listen address not saved")
	}

	// The address the service listens on now is in use by itself: fine.
	app.webAddrs = []string{busy.Addr().String()}
	call("PATCH", "/settings", web(func(w *WebConfig) { w.Listen = busy.Addr().String() }), 200)

	// Restore runs the same check.
	backup := app.store.Get()
	backup.Web.Listen = "not-an-address"
	call("POST", "/restore", backup, 400)
}

func TestRemoteIP(t *testing.T) {
	for _, c := range []struct {
		remote string
		xff    []string
		want   string
	}{
		{"203.0.113.5:1234", nil, "203.0.113.5"},
		{"203.0.113.5:1234", []string{"198.51.100.1"}, "203.0.113.5"}, // not from a local proxy
		{"127.0.0.1:1234", []string{"198.51.100.1"}, "198.51.100.1"},
		// The client sent its own header; the proxy appended the real address.
		{"127.0.0.1:1234", []string{"1.2.3.4, 198.51.100.1"}, "198.51.100.1"},
		{"127.0.0.1:1234", []string{"1.2.3.4", "198.51.100.1"}, "198.51.100.1"},
		{"127.0.0.1:1234", []string{"garbage"}, "127.0.0.1"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		for _, v := range c.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := remoteIP(r); got != c.want {
			t.Errorf("%s %v: got %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
}

// Anyone can start a passkey sign-in, so pending ones are capped per
// address and in total.
func TestPasskeyLoginCap(t *testing.T) {
	a := newAuth(nil)
	start := func(id, ip string, expires time.Time) bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.addPasskeyLoginLocked(id, &ceremony{ip: lockKey(ip), expires: expires})
	}
	later := time.Now().Add(ticketTTL)
	for i := range maxPasskeyLoginsPerIP {
		if !start(fmt.Sprint("a", i), "198.51.100.1", later) {
			t.Fatalf("sign-in %d refused", i)
		}
	}
	if start("a-more", "198.51.100.1", later) {
		t.Fatal("too many sign-ins from one address accepted")
	}
	if !start("b0", "198.51.100.2", later) {
		t.Fatal("another address refused")
	}
	// Expired ones make room again.
	a.mfa.logins = map[string]*ceremony{}
	start("old", "198.51.100.3", time.Now().Add(-time.Second))
	if !start("new", "198.51.100.3", later) || len(a.mfa.logins) != 1 {
		t.Fatalf("expired sign-in not dropped: %d pending", len(a.mfa.logins))
	}
	// In total, the oldest makes room.
	a.mfa.logins = map[string]*ceremony{}
	for i := range maxPasskeyLogins {
		start(fmt.Sprint("c", i), fmt.Sprintf("10.0.%d.%d", i/250, i%250), later.Add(time.Duration(i)*time.Millisecond))
	}
	start("last", "192.0.2.1", later.Add(time.Hour))
	if _, ok := a.mfa.logins["c0"]; ok || len(a.mfa.logins) != maxPasskeyLogins {
		t.Fatalf("cap not kept: %d pending, oldest kept %v", len(a.mfa.logins), ok)
	}
}

func TestLanBlock(t *testing.T) {
	got := lanBlock([]netip.Prefix{
		netip.MustParsePrefix("192.168.1.20/24"),
		netip.MustParsePrefix("203.0.113.9/24"), // public IPv4: not a LAN
		netip.MustParsePrefix("2001:db8:1:2::20/64"),
		netip.MustParsePrefix("fd00:1:2:3::20/64"),
		netip.MustParsePrefix("fe80::1/64"),
		netip.MustParsePrefix("2001:db8::1/32"),  // no LAN
		netip.MustParsePrefix("192.168.1.30/24"), // same network twice
	})
	want := []netip.Prefix{
		netip.MustParsePrefix("192.168.1.0/24"),
		netip.MustParsePrefix("2001:db8:1:2::/64"),
		netip.MustParsePrefix("fd00:1:2:3::/64"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// A change that would leave no user with a password is refused: restoring
// a backup without users, or the last users deleting each other.
func TestNoUserLeftWithPassword(t *testing.T) {
	app, call := signedInApp(t)
	if err := app.store.Update(func(c *Config) error { c.Users = nil; return nil }); err == nil {
		t.Fatal("removing every user was accepted")
	}
	if !app.store.Get().passwordSet() {
		t.Fatal("password lost")
	}

	backup := app.store.Get()
	backup.Users, backup.APITokens = nil, nil
	call("POST", "/restore", backup, 400)
	backup = app.store.Get()
	backup.Version = configVersion + 1
	call("POST", "/restore", backup, 400)
	call("POST", "/restore", app.store.Get(), 200)
	if !app.store.Get().passwordSet() {
		t.Fatal("password lost")
	}
}

// slowKernel records the configs it applied; the first apply takes a while.
type slowKernel struct {
	fakeKernel
	mu      sync.Mutex
	calls   int
	applied []string // peer names, per apply
}

func (k *slowKernel) Apply(c *Config) error {
	k.mu.Lock()
	k.calls++
	first := k.calls == 1
	k.mu.Unlock()
	if first {
		time.Sleep(200 * time.Millisecond)
	}
	var names []string
	for _, p := range c.Peers {
		names = append(names, p.Name)
	}
	k.mu.Lock()
	k.applied = append(k.applied, strings.Join(names, ","))
	k.mu.Unlock()
	return nil
}

// Applies run one at a time, so a slow apply of an older config cannot
// finish after the newest one and undo it in the kernel.
func TestApplyOrder(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	k := &slowKernel{}
	r := newReconciler(k, store)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = r.ApplyNow() }() // the old config, slowly
	time.Sleep(50 * time.Millisecond)
	if err := store.Update(func(c *Config) error {
		c.Peers = append(c.Peers, Peer{ID: newID(), Name: "phone", IPv4: serverIPv4(netip.MustParsePrefix(c.Server.IPv4)).Next().String()})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = r.ApplyNow()
	wg.Wait()
	if last := k.applied[len(k.applied)-1]; last != "phone" {
		t.Fatalf("the kernel ended with %q, not the newest config; applies: %q", last, k.applied)
	}
}
