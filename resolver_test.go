package main

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeDNS answers A queries with ip on addr ("127.0.0.1:0" picks a port).
// With ip invalid it reads queries and never answers.
func fakeDNS(t *testing.T, addr string, ip netip.Addr) *net.UDPConn {
	t.Helper()
	ua, _ := net.ResolveUDPAddr("udp", addr)
	c, err := net.ListenUDP("udp", ua)
	if err != nil {
		t.Skip("cannot listen on", addr, err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if !ip.IsValid() {
				continue
			}
			var p dnsmessage.Parser
			h, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, err := p.Question()
			if err != nil {
				continue
			}
			b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, Authoritative: true})
			_ = b.StartQuestions()
			_ = b.Question(q)
			_ = b.StartAnswers()
			if q.Type == dnsmessage.TypeA {
				_ = b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60}, dnsmessage.AResource{A: ip.As4()})
			}
			out, err := b.Finish()
			if err == nil {
				_, _ = c.WriteToUDP(out, from)
			}
		}
	}()
	return c
}

// ask sends one A query for name through the router's connection for the
// system server sys, and returns the answer's address.
func ask(t *testing.T, d *dnsRouter, sys string) (netip.Addr, error) {
	t.Helper()
	conn, err := d.dial(context.Background(), "udp", sys)
	if err != nil {
		return netip.Addr{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	name := dnsmessage.MustNewName("api.example.test.")
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 4242, RecursionDesired: true})
	_ = b.StartQuestions()
	_ = b.Question(dnsmessage.Question{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
	q, _ := b.Finish()
	if _, err := conn.Write(q); err != nil {
		return netip.Addr{}, err
	}
	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	if err != nil {
		return netip.Addr{}, err
	}
	var p dnsmessage.Parser
	if _, err := p.Start(buf[:n]); err != nil {
		return netip.Addr{}, err
	}
	_ = p.SkipAllQuestions()
	a, err := p.AnswerHeader()
	if err != nil {
		return netip.Addr{}, err
	}
	if a.Type != dnsmessage.TypeA {
		t.Fatalf("answer type %v", a.Type)
	}
	r, err := p.AResource()
	if err != nil {
		return netip.Addr{}, err
	}
	return netip.AddrFrom4(r.A), nil
}

func TestDNSRouter(t *testing.T) {
	good := fakeDNS(t, "127.0.0.1:0", netip.MustParseAddr("192.0.2.10"))
	port := good.LocalAddr().(*net.UDPAddr).Port
	fakeDNS(t, net.JoinHostPort("::1", strconv.Itoa(port)), netip.Addr{}) // silent
	sys := fakeDNS(t, "127.0.0.1:0", netip.MustParseAddr("192.0.2.99"))
	sysAddr := sys.LocalAddr().String()

	oldPort, oldTry := dnsPort, dnsTryTimeout
	dnsPort, dnsTryTimeout = strconv.Itoa(port), 300*time.Millisecond
	t.Cleanup(func() { dnsPort, dnsTryTimeout = oldPort, oldTry })

	on, off := true, false
	for _, tc := range []struct {
		name    string
		cfg     DNSConfig
		want    string // answer, "" for an error
		answers string // who answered
	}{
		{"no servers: the system resolver", DNSConfig{Servers: []string{}}, "192.0.2.99", sysAddr},
		{"first server answers", DNSConfig{Servers: []string{"127.0.0.1", "::1"}}, "192.0.2.10", "127.0.0.1:" + strconv.Itoa(port)},
		{"silent first server is skipped", DNSConfig{Servers: []string{"::1", "127.0.0.1"}}, "192.0.2.10", "127.0.0.1:" + strconv.Itoa(port)},
		{"none answers: fallback (default on)", DNSConfig{Servers: []string{"::1"}}, "192.0.2.99", sysAddr},
		{"none answers: fallback on", DNSConfig{Servers: []string{"::1"}, Fallback: &on}, "192.0.2.99", sysAddr},
		{"none answers, fallback off: error", DNSConfig{Servers: []string{"::1"}, Fallback: &off}, "", ""},
	} {
		d := newDNSRouter(tc.cfg)
		var who string
		d.answered = func(s string) { who = s }
		got, err := ask(t, d, sysAddr)
		switch {
		case tc.want == "" && err == nil:
			t.Errorf("%s: answer %v, want an error", tc.name, got)
		case tc.want != "" && (err != nil || got.String() != tc.want):
			t.Errorf("%s: %v %v, want %s", tc.name, got, err, tc.want)
		case tc.want != "" && who != tc.answers:
			t.Errorf("%s: answered by %s, want %s", tc.name, who, tc.answers)
		}
	}

	// Settings apply to the next lookup.
	d := newDNSRouter(DNSConfig{Servers: []string{"::1"}, Fallback: &off})
	if _, err := ask(t, d, sysAddr); err == nil {
		t.Fatal("silent server and no fallback answered")
	}
	d.Set(DNSConfig{Servers: []string{"127.0.0.1"}})
	if got, err := ask(t, d, sysAddr); err != nil || got.String() != "192.0.2.10" {
		t.Fatalf("after Set: %v %v", got, err)
	}

	// The whole Go resolver goes through the router: the system server
	// from resolv.conf is never asked while a configured server answers.
	d.Set(DNSConfig{Servers: []string{"127.0.0.1"}, Fallback: &off})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, err := d.resolver().LookupNetIP(ctx, "ip4", "api.example.test.")
	if err != nil || len(addrs) != 1 || addrs[0].String() != "192.0.2.10" {
		t.Fatalf("resolver lookup: %v %v", addrs, err)
	}

	// The Test button names the server that answered.
	res, err := testDNS(context.Background(), DNSConfig{Servers: []string{"::1", "127.0.0.1"}}, "api.example.test.")
	if err != nil || res.Answer != "192.0.2.10" || res.Server != "127.0.0.1" {
		t.Fatalf("test lookup: %+v %v", res, err)
	}
}

func TestValidateDNSServers(t *testing.T) {
	for _, ok := range [][]string{{}, {"9.9.9.9", "149.112.112.112"}, {"2620:fe::fe"}} {
		if err := validateDNSServers(ok); err != nil {
			t.Errorf("%v refused: %v", ok, err)
		}
	}
	for _, bad := range [][]string{{"dns.quad9.net"}, {"9.9.9.9:53"}, {"fe80::1%eth0"}, {""},
		{"1.1.1.1", "1.1.1.2", "1.1.1.3", "1.1.1.4", "1.1.1.5", "1.1.1.6", "1.1.1.7", "1.1.1.8", "1.1.1.9"}} {
		if err := validateDNSServers(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestDNSSettings(t *testing.T) {
	app, call := signedInApp(t)
	app.dns = newDNSRouter(app.store.Get().DNS)
	s := call("GET", "/settings", nil, 200)["dns"].(map[string]any)
	if len(s["servers"].([]any)) != 0 || s["fallback"] != true {
		t.Fatalf("default DNS settings: %v", s)
	}
	call("PATCH", "/settings", map[string]any{"dns": map[string]any{"servers": []string{"9.9.9.9", "149.112.112.112"}, "fallback": false}}, 200)
	c := app.store.Get().DNS
	if strings.Join(c.Servers, ",") != "9.9.9.9,149.112.112.112" || c.fallback() {
		t.Fatalf("saved: %+v", c)
	}
	if got := app.dns.cfg.Load(); strings.Join(got.Servers, ",") != "9.9.9.9,149.112.112.112" || got.fallback() {
		t.Fatalf("not applied to the router: %+v", got)
	}
	call("PATCH", "/settings", map[string]any{"dns": map[string]any{"servers": []string{"dns.quad9.net"}}}, 400)
	call("POST", "/dns/test", map[string]any{"servers": []string{"not-an-ip"}}, 400)
	call("PATCH", "/settings", map[string]any{"dns": map[string]any{"servers": []string{}, "fallback": true}}, 200)
	if c := app.store.Get().DNS; len(c.Servers) != 0 {
		t.Fatalf("back to the system resolver: %+v", c)
	}
}

func TestDNSQueryLog(t *testing.T) {
	good := fakeDNS(t, "127.0.0.1:0", netip.MustParseAddr("192.0.2.10"))
	port := good.LocalAddr().(*net.UDPAddr).Port
	sys := fakeDNS(t, "127.0.0.1:0", netip.MustParseAddr("192.0.2.99"))
	oldPort, oldTry := dnsPort, dnsTryTimeout
	dnsPort, dnsTryTimeout = strconv.Itoa(port), 300*time.Millisecond
	t.Cleanup(func() { dnsPort, dnsTryTimeout = oldPort, oldTry })

	path := filepath.Join(t.TempDir(), "log.jsonl")
	w, err := setupLogging(path, LogConfig{Level: "info", MaxSizeMB: 10, MaxFiles: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close(); slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil))) })
	slog.Info("unrelated line")

	off := false
	for _, cfg := range []DNSConfig{{Servers: []string{}}, {Servers: []string{"127.0.0.1"}}, {Servers: []string{"::1"}, Fallback: &off}} {
		_, _ = ask(t, newDNSRouter(cfg), sys.LocalAddr().String())
	}
	lines, err := readLogTail(path, 100, "", "dns")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("%d DNS lines, want 3: %s", len(lines), lines)
	}
	want := []string{ // newest first
		`"level":"WARN","msg":"dns query failed","dns":true,"name":"api.example.test","type":"A","err":"no DNS server answered (asked ::1)`,
		`"msg":"dns query","dns":true,"name":"api.example.test","type":"A","server":"127.0.0.1","answer":"192.0.2.10"`,
		`"msg":"dns query","dns":true,"name":"api.example.test","type":"A","server":"127.0.0.1 (system resolver)","answer":"192.0.2.99"`,
	}
	for i, l := range lines {
		if !strings.Contains(string(l), want[i]) {
			t.Errorf("line %d:\n%s\nwant it to contain\n%s", i, l, want[i])
		}
	}
	if all, _ := readLogTail(path, 100, "", ""); len(all) != 4 {
		t.Errorf("unfiltered: %d lines, want 4", len(all))
	}
}
