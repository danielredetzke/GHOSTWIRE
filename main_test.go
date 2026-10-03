package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
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
	} {
		cc := c.clone()
		mutate(cc)
		if err := cc.validate(); err == nil {
			t.Errorf("%s: expected an error", name)
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
	_ = store.Update(func(c *Config) error { c.Admin.PasswordHash = hash; c.Server.Endpoint = "vpn.example.net"; return nil })
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
	bearer := func(method, path string, want int) {
		req, _ := http.NewRequest(method, srv.URL+"/api/v1"+path, nil)
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
