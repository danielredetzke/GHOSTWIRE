package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// GHOSTWIRE's own name lookups (update check, geo databases, Let's Encrypt,
// public IP detection, the dashboard's endpoint check) all go through Go's
// resolver, which reads /etc/hosts and /etc/resolv.conf. With DNS servers set
// under Settings, every query it sends goes to those servers instead, in
// order; the server from /etc/resolv.conf is asked only as the fallback.
// Every query is logged with "dns":true. Peers use the DNS of their client
// configs, which this does not touch.

// DNSConfig sets the DNS servers this service uses for its own lookups.
type DNSConfig struct {
	Servers  []string `json:"servers"`            // empty: the system resolver
	Fallback *bool    `json:"fallback,omitempty"` // ask the system resolver when none answers; default on
}

func (c DNSConfig) fallback() bool { return c.Fallback == nil || *c.Fallback }

const maxDNSServers = 8

var (
	dnsPort       = "53" // tests use another port
	dnsTryTimeout = 2 * time.Second
)

// dnsRouter sends the resolver's queries to the configured servers.
type dnsRouter struct {
	cfg atomic.Pointer[DNSConfig]
	// answered is told which server answered, as "address:port".
	answered func(server string)
}

func newDNSRouter(c DNSConfig) *dnsRouter {
	d := &dnsRouter{}
	d.Set(c)
	return d
}

// Set applies new settings; lookups that start afterwards use them.
func (d *dnsRouter) Set(c DNSConfig) {
	c.Servers = slices.Clone(c.Servers)
	d.cfg.Store(&c)
}

func (d *dnsRouter) resolver() *net.Resolver {
	return &net.Resolver{PreferGo: true, Dial: d.dial}
}

// dial is the resolver's Dial: address is the system's DNS server, the only
// target without servers of our own and the fallback with them.
func (d *dnsRouter) dial(ctx context.Context, network, address string) (net.Conn, error) {
	c := d.cfg.Load()
	sys, _, _ := net.SplitHostPort(address)
	targets, labels := []string{address}, []string{sys + " (system resolver)"}
	if c != nil && len(c.Servers) > 0 {
		targets, labels = targets[:0], labels[:0]
		for _, s := range c.Servers {
			targets, labels = append(targets, net.JoinHostPort(s, dnsPort)), append(labels, s)
		}
		if c.fallback() {
			targets, labels = append(targets, address), append(labels, sys+" (system resolver, fallback)")
		}
	}
	if strings.HasPrefix(network, "tcp") {
		// A truncated answer is asked again over TCP: the first server that
		// takes the connection gets it.
		var err error
		for _, t := range targets {
			tctx, cancel := context.WithTimeout(ctx, dnsTryTimeout)
			var nd net.Dialer
			var conn net.Conn
			conn, err = nd.DialContext(tctx, network, t)
			cancel()
			if err == nil {
				d.tell(t)
				return &dnsTCPConn{Conn: conn, router: d, server: labels[slices.Index(targets, t)]}, nil
			}
		}
		d.logQuery(nil, nil, "", 0, err)
		return nil, err
	}
	return &dnsUDPConn{network: network, targets: targets, labels: labels, router: d}, nil
}

func (d *dnsRouter) tell(server string) {
	if d.answered != nil {
		d.answered(server)
	}
}

// logQuery writes one query and its outcome to the log.
func (d *dnsRouter) logQuery(query, reply []byte, server string, took time.Duration, err error) {
	args := []any{"dns", true}
	var p dnsmessage.Parser
	if _, perr := p.Start(query); perr == nil {
		if q, perr := p.Question(); perr == nil {
			args = append(args, "name", strings.TrimSuffix(q.Name.String(), "."), "type", strings.TrimPrefix(q.Type.String(), "Type"))
		}
	}
	if err != nil {
		slog.Warn("dns query failed", append(args, "err", err.Error())...)
		return
	}
	args = append(args, "server", server)
	if h, perr := p.Start(reply); perr == nil {
		_ = p.SkipAllQuestions()
		var answers []string
		for {
			a, perr := p.Answer()
			if perr != nil {
				break
			}
			switch r := a.Body.(type) {
			case *dnsmessage.AResource:
				answers = append(answers, netip.AddrFrom4(r.A).String())
			case *dnsmessage.AAAAResource:
				answers = append(answers, netip.AddrFrom16(r.AAAA).String())
			case *dnsmessage.CNAMEResource:
				answers = append(answers, strings.TrimSuffix(r.CNAME.String(), "."))
			}
		}
		if h.RCode != dnsmessage.RCodeSuccess {
			args = append(args, "rcode", strings.TrimPrefix(h.RCode.String(), "RCode"))
		}
		if len(answers) > 0 {
			args = append(args, "answer", strings.Join(answers, ", "))
		} else if h.RCode == dnsmessage.RCodeSuccess {
			args = append(args, "answer", "none")
		}
	}
	slog.Info("dns query", append(args, "ms", took.Milliseconds())...)
}

// dnsUDPConn is handed to the resolver for one query over UDP. The query it
// writes is sent to the targets in turn until one answers.
type dnsUDPConn struct {
	network  string
	targets  []string
	router   *dnsRouter
	deadline time.Time
	query    []byte
	done     bool
	labels   []string // the targets as the log names them
}

var errNoDNSAnswer = errors.New("no DNS server answered")

func (c *dnsUDPConn) Write(b []byte) (int, error) {
	c.query, c.done = slices.Clone(b), false
	return len(b), nil
}

func (c *dnsUDPConn) Read(b []byte) (int, error) {
	if c.done || len(c.query) < 2 {
		// The resolver reads again only after an answer it did not accept.
		return 0, os.ErrDeadlineExceeded
	}
	c.done = true
	start := time.Now()
	err := errNoDNSAnswer
	for i, t := range c.targets {
		// Each target but the last gets a fair share of the time left, at
		// most dnsTryTimeout; the last gets the rest.
		try := dnsTryTimeout
		if !c.deadline.IsZero() {
			left := time.Until(c.deadline)
			if left <= 0 {
				err = os.ErrDeadlineExceeded
				break
			}
			try = left
			if rest := len(c.targets) - i; rest > 1 {
				try = min(dnsTryTimeout, left/time.Duration(rest))
			}
		}
		var n int
		if n, err = udpExchange(c.network, t, c.query, b, try); err == nil {
			c.router.tell(t)
			c.router.logQuery(c.query, b[:n], c.labels[i], time.Since(start), nil)
			return n, nil
		}
	}
	c.router.logQuery(c.query, nil, "", 0, fmt.Errorf("%w (asked %s): %v", errNoDNSAnswer, strings.Join(c.labels, ", "), err))
	return 0, err
}

// udpExchange sends one query and returns the first reply with its ID.
func udpExchange(network, server string, query, buf []byte, timeout time.Duration) (int, error) {
	conn, err := net.DialTimeout(network, server, timeout)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(query); err != nil {
		return 0, err
	}
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return 0, err
		}
		if n >= 2 && buf[0] == query[0] && buf[1] == query[1] {
			return n, nil
		}
	}
}

func (c *dnsUDPConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, err := c.Read(b)
	return n, c.RemoteAddr(), err
}
func (c *dnsUDPConn) WriteTo(b []byte, _ net.Addr) (int, error) { return c.Write(b) }
func (c *dnsUDPConn) Close() error                              { return nil }
func (c *dnsUDPConn) LocalAddr() net.Addr                       { return &net.UDPAddr{} }
func (c *dnsUDPConn) RemoteAddr() net.Addr                      { return &net.UDPAddr{} }
func (c *dnsUDPConn) SetDeadline(t time.Time) error             { c.deadline = t; return nil }
func (c *dnsUDPConn) SetReadDeadline(t time.Time) error         { c.deadline = t; return nil }
func (c *dnsUDPConn) SetWriteDeadline(time.Time) error          { return nil }

// dnsTCPConn logs the query and answer of a TCP connection to a DNS
// server, which the resolver uses after a truncated UDP answer. Both are
// prefixed with their length.
type dnsTCPConn struct {
	net.Conn
	router *dnsRouter
	server string
	start  time.Time
	query  []byte
	reply  []byte
	logged bool
}

func (c *dnsTCPConn) Write(b []byte) (int, error) {
	if len(b) > 2 && c.query == nil {
		c.query, c.start = slices.Clone(b[2:]), time.Now()
	}
	return c.Conn.Write(b)
}

func (c *dnsTCPConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if !c.logged && n > 0 {
		c.reply = append(c.reply, b[:n]...)
		if len(c.reply) >= 2 {
			if size := int(c.reply[0])<<8 | int(c.reply[1]); len(c.reply) >= 2+size {
				c.logged = true
				c.router.logQuery(c.query, c.reply[2:2+size], c.server+" over TCP", time.Since(c.start), nil)
			}
		}
	}
	return n, err
}

// systemDNSServers lists the nameservers in resolv.conf, for the settings.
func systemDNSServers(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return []string{}
	}
	defer f.Close()
	out := []string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if fs := strings.Fields(sc.Text()); len(fs) >= 2 && fs[0] == "nameserver" {
			out = append(out, fs[1])
		}
	}
	return out
}

// dnsTestResult is what the Test button shows.
type dnsTestResult struct {
	Name   string `json:"name"`
	Answer string `json:"answer"`
	MS     int64  `json:"ms"`
	Server string `json:"server"`
}

// testDNS looks up name with settings c, without saving them.
func testDNS(ctx context.Context, c DNSConfig, name string) (dnsTestResult, error) {
	d := newDNSRouter(c)
	var server atomic.Value
	d.answered = func(s string) { server.CompareAndSwap(nil, s) }
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	start := time.Now()
	addrs, err := d.resolver().LookupNetIP(ctx, "ip", name)
	out := dnsTestResult{Name: name, MS: time.Since(start).Milliseconds()}
	if err != nil {
		return out, badRequest("looking up %s failed: %v", name, err)
	}
	if len(addrs) > 0 {
		out.Answer = addrs[0].Unmap().String()
	}
	if s, ok := server.Load().(string); ok {
		out.Server = s
		if host, port, err := net.SplitHostPort(s); err == nil && port == dnsPort && slices.Contains(c.Servers, host) {
			out.Server = host
		} else if len(c.Servers) > 0 {
			out.Server = "the system resolver (" + host + "), as fallback"
		} else {
			out.Server = "the system resolver (" + host + ")"
		}
	} else {
		out.Server = "the hosts file"
	}
	return out, nil
}

// validateDNSServers checks the servers of the DNS setting.
func validateDNSServers(list []string) error {
	if len(list) > maxDNSServers {
		return errors.New("DNS servers: at most 8")
	}
	for _, s := range list {
		if a, err := netip.ParseAddr(s); err != nil || a.Zone() != "" {
			return errors.New("DNS servers: \"" + s + "\" is not an IP address")
		}
	}
	return nil
}
