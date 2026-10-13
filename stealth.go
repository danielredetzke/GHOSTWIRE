package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
)

// Stealth hides the web interface and the API from everyone but the
// management sources: the server itself (loopback), the tunnel addresses of
// enabled peers with management access, and the trusted networks. WireGuard
// authenticates every packet that comes through the tunnel, so a tunnel
// source address is a verified device. Everyone else gets a closed
// connection: during the TLS handshake, before a certificate is sent, or
// without an HTTP response. Let's Encrypt validations and live setup links
// still get through, so the certificate renews and new devices can be set up.
//
// A request from the server's own public address is not a management source:
// on a home network that is every device behind the router.

// manages reports whether the peer is a management device right now.
func (p *Peer) manages() bool { return p.Enabled && p.Manage && p.hasKey() }

func validateTrusted(list []string) error {
	if err := validateHostList(list, "trusted networks", true); err != nil {
		return err
	}
	for _, s := range list {
		p := netip.MustParsePrefix(s)
		if p.Bits() == 0 {
			return fmt.Errorf("trusted networks: %s would let the whole internet in", s)
		}
		if p.Masked() != p {
			return fmt.Errorf("trusted networks: use the network address, e.g. %s", p.Masked())
		}
	}
	return nil
}

// stealthState is what the gate needs from one config, prepared once.
type stealthState struct {
	on      bool
	manage  map[netip.Addr]bool // tunnel addresses of management peers
	trusted []netip.Prefix
	setups  map[string]time.Time // live setup link tokens and their expiry
}

func newStealthState(c *Config) *stealthState {
	s := &stealthState{on: c.Stealth.Enabled, manage: map[netip.Addr]bool{}, setups: map[string]time.Time{}}
	for i := range c.Peers {
		p := &c.Peers[i]
		if p.manages() {
			for _, a := range peerAddresses(c, p) {
				s.manage[a.Addr()] = true
			}
		}
		if p.Setup != nil {
			s.setups[p.Setup.Token] = p.Setup.Expires
		}
	}
	// Invalid entries are skipped: keepAccess sees a config before
	// validation refuses them.
	for _, t := range c.Stealth.Trusted {
		if p, err := netip.ParsePrefix(t); err == nil {
			s.trusted = append(s.trusted, p)
		}
	}
	return s
}

// allows reports whether ip is a management source.
func (s *stealthState) allows(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return false
	}
	return ip.IsLoopback() || s.manage[ip] || slices.ContainsFunc(s.trusted, func(p netip.Prefix) bool { return p.Contains(ip) })
}

func (s *stealthState) setupLive(token string, now time.Time) bool {
	exp, ok := s.setups[token]
	return ok && token != "" && now.Before(exp)
}

func (s *stealthState) anySetupLive(now time.Time) bool {
	for _, exp := range s.setups {
		if now.Before(exp) {
			return true
		}
	}
	return false
}

// stealthGate keeps the state of the current config and counts the
// connections it closed, per hour for the last 24 hours. Nothing is logged
// per connection: scanners would fill the log.
type stealthGate struct {
	mu     sync.Mutex
	cfg    *Config // the config st was made from
	st     *stealthState
	closed [24]struct{ hour, n int64 }
}

func (g *stealthGate) state(store *Store) *stealthState {
	c := store.current()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cfg != c {
		g.cfg, g.st = c, newStealthState(c)
	}
	return g.st
}

func (g *stealthGate) count() {
	h := time.Now().Unix() / 3600
	g.mu.Lock()
	defer g.mu.Unlock()
	b := &g.closed[h%24]
	if b.hour != h {
		b.hour, b.n = h, 0
	}
	b.n++
}

func (g *stealthGate) closed24h() int64 {
	h := time.Now().Unix() / 3600
	g.mu.Lock()
	defer g.mu.Unlock()
	var n int64
	for _, b := range g.closed {
		if b.hour > h-24 {
			n += b.n
		}
	}
	return n
}

var errStealth = errors.New("closed by stealth")

// stealthTLS hooks the gate into the TLS handshake. It runs on the
// ClientHello, before the server sends anything, so a refused client gets
// no certificate and learns no host name. The connection is closed without
// a TLS alert. Loopback gets the Let's Encrypt certificate for any server
// name, so an SSH tunnel to https://localhost works after one warning.
func (a *App) stealthTLS(t *webTLS, domain string) {
	if t.Config == nil {
		return
	}
	loopback := t.Config.Clone()
	if base := t.Config.GetCertificate; t.ACME != nil && base != nil {
		loopback.GetCertificate = func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
			named := *h
			named.ServerName = domain
			return base(&named)
		}
	}
	t.Config.GetConfigForClient = func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		st := a.stealth.state(a.store)
		ip := addrIP(h.Conn.RemoteAddr())
		switch {
		case ip.IsLoopback() && t.ACME != nil && h.ServerName != domain:
			return loopback, nil
		case !st.on || st.allows(ip):
			return nil, nil
		// A Let's Encrypt validation: autocert answers it with the
		// challenge certificate, and no HTTP is possible on it. Without a
		// pending challenge the connection is closed like any other.
		case t.ACME != nil && slices.Contains(h.SupportedProtos, acme.ALPNProto):
			challenge := t.Config.Clone()
			challenge.GetConfigForClient = nil
			challenge.GetCertificate = func(hh *tls.ClientHelloInfo) (*tls.Certificate, error) {
				c, err := t.Config.GetCertificate(hh)
				if err != nil {
					a.stealth.count()
					h.Conn.Close()
				}
				return c, err
			}
			return challenge, nil
		// A new device has no tunnel yet; the HTTP gate lets only its
		// setup link through.
		case st.anySetupLive(time.Now()):
			return nil, nil
		}
		a.stealth.count()
		h.Conn.Close()
		return nil, errStealth
	}
}

// stealthListener closes connections from outside whose first byte does not
// start a TLS handshake. Go's TLS server would otherwise answer plain HTTP
// on the HTTPS port with its own "400 Bad Request".
func (a *App) stealthListener(ln net.Listener) net.Listener { return &stealthLn{ln, a} }

type stealthLn struct {
	net.Listener
	a *App
}

func (l *stealthLn) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &stealthConn{Conn: c, a: l.a}, nil
}

type stealthConn struct {
	net.Conn
	a       *App
	checked bool
}

func (c *stealthConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if c.checked || n == 0 {
		return n, err
	}
	c.checked = true
	const handshakeRecord = 0x16
	if b[0] != handshakeRecord {
		if st := c.a.stealth.state(c.a.store); st.on && !st.allows(addrIP(c.RemoteAddr())) {
			c.a.stealth.count()
			c.Conn.Close()
			return 0, io.EOF
		}
	}
	return n, err
}

// stealthHTTP is the gate for every request: a management source gets the
// routes, everyone else only a live setup link. Anything else ends the
// connection without a response. Behind a reverse proxy this is the only
// gate, and the source is the X-Forwarded-For address (see remoteIP).
func (a *App) stealthHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := a.stealth.state(a.store)
		if !st.on || st.allows(requestIP(r)) || st.setupLive(setupToken(r.URL.Path), time.Now()) {
			next.ServeHTTP(w, r)
			return
		}
		a.stealth.count()
		panic(http.ErrAbortHandler)
	})
}

// stealthPort80 is the gate for the plain HTTP listener: the public may only
// answer Let's Encrypt http-01 challenges for the domain. There is no
// redirect, so nothing echoes the host name back.
func (a *App) stealthPort80(full, acmeOnly http.Handler, domain string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := a.stealth.state(a.store)
		if !st.on || st.allows(requestIP(r)) {
			full.ServeHTTP(w, r)
			return
		}
		// Only a real challenge answer goes out: autocert's own errors
		// ("acme/autocert: certificate cache miss") would name it.
		if acmeOnly != nil && hostOnly(r.Host) == domain && strings.HasPrefix(r.URL.Path, "/.well-known/acme-challenge/") {
			b := &bufferedResponse{header: http.Header{}, code: http.StatusOK}
			acmeOnly.ServeHTTP(b, r)
			if b.code == http.StatusOK {
				maps.Copy(w.Header(), b.header)
				w.WriteHeader(b.code)
				_, _ = w.Write(b.body.Bytes())
				return
			}
		}
		a.stealth.count()
		panic(http.ErrAbortHandler)
	})
}

// bufferedResponse holds a response until it is clear it may be sent.
type bufferedResponse struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header         { return b.header }
func (b *bufferedResponse) WriteHeader(code int)        { b.code = code }
func (b *bufferedResponse) Write(p []byte) (int, error) { return b.body.Write(p) }

// setupToken returns the token of a setup link path, "" for other paths.
// Only the exact shapes count, so nothing like /setup/<token>/../../api
// reaches the router.
func setupToken(p string) string {
	if path.Clean(p) != p {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	switch {
	case parts[0] == "setup" && (len(parts) == 2 || len(parts) == 3):
		return parts[1]
	case len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "setup":
		return parts[3]
	}
	return ""
}

func addrIP(a net.Addr) netip.Addr {
	if t, ok := a.(*net.TCPAddr); ok {
		return t.AddrPort().Addr().Unmap()
	}
	ap, _ := netip.ParseAddrPort(a.String())
	return ap.Addr().Unmap()
}

func requestIP(r *http.Request) netip.Addr {
	ip, _ := netip.ParseAddr(remoteIP(r))
	return ip.Unmap()
}

// accessView tells the UI how the browser asking is connected, so it can
// show whether turning on Stealth would lock it out.
type accessView struct {
	Management bool     `json:"management"`
	Via        string   `json:"via"` // server | peer | trusted | internet
	Peer       *peerRef `json:"peer"`
}

func accessFor(c *Config, r *http.Request) accessView {
	ip := requestIP(r)
	v := accessView{Management: newStealthState(c).allows(ip), Via: "internet"}
	switch p := peerByTunnelAddr(c, ip); {
	case ip.IsLoopback():
		v.Via = "server"
	case p != nil:
		v.Via, v.Peer = "peer", &peerRef{ID: p.ID, Name: p.Name}
	case v.Management:
		v.Via = "trusted"
	}
	return v
}

// keepAccess refuses a change that would turn away the browser making it.
// Called with the changed config inside a store update.
func keepAccess(r *http.Request, old, next *Config) error {
	// An invalid config is refused by the store's own validation, with
	// its own message; only a valid one is judged here.
	checked := next.clone()
	checked.applyDefaults()
	if checked.validate() != nil {
		return nil
	}
	if !checked.Stealth.Enabled || newStealthState(checked).allows(requestIP(r)) {
		return nil
	}
	if !old.Stealth.Enabled {
		return badRequest("connect through a peer with management access first: turning on Stealth from here would lock you out")
	}
	return badRequest("Stealth is on, and this change would cut off the connection you're using")
}
