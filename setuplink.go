package main

import (
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"slices"
	"time"
)

// A setup link hands a client config to someone who is not next to the
// admin. The keys are made only when the link is opened, so the private key
// is never stored: the link works once, then it is deleted.
//
// The token and PIN are kept in config.json (0600) so the admin can copy the
// link again. Whoever can read that file already holds the server key.
type SetupLink struct {
	Token   string    `json:"token"`
	PIN     string    `json:"pin,omitempty"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
	Fails   int       `json:"fails,omitempty"` // wrong PINs so far
}

const (
	setupMaxFails = 5 // wrong PINs before the link is revoked
	setupPINLen   = 4
)

// setupHours are the lifetimes the UI offers.
var setupHours = []int{1, 24, 168}

func (s *SetupLink) expired(now time.Time) bool { return !now.Before(s.Expires) }

func randomPIN() string {
	max := big.NewInt(1)
	for range setupPINLen {
		max.Mul(max, big.NewInt(10))
	}
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%0*d", setupPINLen, n)
}

// setupRequest is the part of a create or issue request that asks for a
// link instead of a config shown right away.
type setupRequest struct {
	Delivery  string `json:"delivery"`  // "" or "show": config now; "link": setup link
	LinkHours int    `json:"linkHours"` // 1, 24 or 168; default 24
	LinkPIN   *bool  `json:"linkPIN"`   // default true
}

func (in setupRequest) wantsLink() bool { return in.Delivery == "link" }

func (in setupRequest) newLink() (*SetupLink, error) {
	switch in.Delivery {
	case "", "show", "link":
	default:
		return nil, badRequest("delivery must be show or link")
	}
	hours := in.LinkHours
	if hours == 0 {
		hours = 24
	}
	if !slices.Contains(setupHours, hours) {
		return nil, badRequest("linkHours must be 1, 24 or 168")
	}
	now := time.Now().UTC()
	l := &SetupLink{Token: randomString(24), Created: now, Expires: now.Add(time.Duration(hours) * time.Hour)}
	if in.LinkPIN == nil || *in.LinkPIN {
		l.PIN = randomPIN()
	}
	return l, nil
}

// setupView is what peer lists show about a pending link: no secrets, so
// read-only tokens may see it.
type setupView struct {
	Created     time.Time `json:"created"`
	Expires     time.Time `json:"expires"`
	Expired     bool      `json:"expired"`
	PINRequired bool      `json:"pinRequired"`
	PINFails    int       `json:"pinFails"`
}

func viewSetup(s *SetupLink) *setupView {
	if s == nil {
		return nil
	}
	return &setupView{Created: s.Created, Expires: s.Expires, Expired: s.expired(time.Now()), PINRequired: s.PIN != "", PINFails: s.Fails}
}

// setupSecret is the link itself, for the admin who sends it.
type setupSecret struct {
	URL     string    `json:"url"`
	Path    string    `json:"path"`
	PIN     string    `json:"pin,omitempty"`
	Expires time.Time `json:"expires"`
	QR      string    `json:"qr"`
}

// setupBase is the scheme and host the admin reached this server with.
// Behind a local reverse proxy, X-Forwarded-Proto tells whether that was
// HTTPS.
func setupBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || (fromLoopback(r) && r.Header.Get("X-Forwarded-Proto") == "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func fromLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func secretFor(r *http.Request, s *SetupLink) (setupSecret, error) {
	path := "/setup/" + s.Token
	out := setupSecret{URL: setupBase(r) + path, Path: path, PIN: s.PIN, Expires: s.Expires}
	qr, err := qrDataURL(out.URL)
	if err != nil {
		return out, err
	}
	out.QR = qr
	return out, nil
}

// peerByToken finds the peer whose link matches token, in constant time per
// comparison.
func (c *Config) peerByToken(token string) *Peer {
	if token == "" {
		return nil
	}
	var found *Peer
	for i := range c.Peers {
		if s := c.Peers[i].Setup; s != nil && subtle.ConstantTimeCompare([]byte(s.Token), []byte(token)) == 1 {
			found = &c.Peers[i]
		}
	}
	return found
}

// --- admin endpoints ---

func (a *App) getSetup(w http.ResponseWriter, r *http.Request) {
	// The link sets up a device, so read-only tokens must not see it.
	if who(r).Scope == "ro" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this token is read-only"})
		return
	}
	cfg := a.store.Get()
	_, p := cfg.peerByID(r.PathValue("id"))
	if p == nil || p.Setup == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "this peer has no setup link"})
		return
	}
	out, err := secretFor(r, p.Setup)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) revokeSetup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var name string
	if err := a.store.Update(func(c *Config) error {
		_, p := c.peerByID(id)
		if p == nil {
			return badRequest("no such peer")
		}
		p.Setup, name = nil, p.Name
		return nil
	}); err != nil {
		writeErr(w, err)
		return
	}
	a.audit(r, "setup link revoked", "peer", name)
	cfg := a.store.Get()
	_, p := cfg.peerByID(id)
	writeJSON(w, http.StatusOK, map[string]any{"peer": a.peerView(cfg, p)})
}

// --- public endpoints, reached with the link alone ---

// errSetupInvalid is the one answer for unknown, used, expired and revoked
// links, so a visitor cannot tell whether a link ever existed.
const errSetupInvalid = "this link isn't valid"

func setupInvalid(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": errSetupInvalid})
}

func (a *App) setupInfo(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Get()
	p := cfg.peerByToken(r.PathValue("token"))
	if p == nil || p.Setup.expired(time.Now()) {
		setupInvalid(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": p.Name, "pinRequired": p.Setup.PIN != "", "expires": p.Setup.Expires})
}

// setupRedeem makes the keys, stores the public key and returns the config.
// The link is deleted in the same update, so it cannot be used twice.
func (a *App) setupRedeem(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PIN string `json:"pin"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	k, err := newPrivateKey()
	if err != nil {
		writeErr(w, err)
		return
	}
	psk, err := newPresharedKey()
	if err != nil {
		writeErr(w, err)
		return
	}
	ip := remoteIP(r)
	var (
		id, name   string
		hadKey     bool
		invalid    bool
		wrongPIN   bool
		triesLeft  int
		revokedNow bool
	)
	err = a.store.Update(func(c *Config) error {
		p := c.peerByToken(r.PathValue("token"))
		if p == nil || p.Setup.expired(time.Now()) {
			invalid = true
			return nil
		}
		name = p.Name
		if p.Setup.PIN != "" && subtle.ConstantTimeCompare([]byte(p.Setup.PIN), []byte(in.PIN)) != 1 {
			wrongPIN = true
			p.Setup.Fails++
			triesLeft = setupMaxFails - p.Setup.Fails
			if triesLeft <= 0 {
				p.Setup, revokedNow = nil, true
			}
			return nil
		}
		now := time.Now().UTC()
		id, hadKey = p.ID, p.hasKey()
		p.PublicKey, p.ConfigIssued, p.Setup = k.PublicKey().String(), &now, nil
		if p.PresharedKey != "" {
			p.PresharedKey = psk.String()
		}
		return nil
	})
	switch {
	case err != nil:
		writeErr(w, err)
		return
	case invalid:
		slog.Warn("setup link not valid", "remote", ip)
		setupInvalid(w)
		return
	case revokedNow:
		slog.Warn("setup link revoked after wrong PINs", "audit", true, "actor", "setup link", "peer", name, "remote", ip)
		setupInvalid(w)
		return
	case wrongPIN:
		slog.Warn("setup link: wrong PIN", "peer", name, "remote", ip)
		writeJSON(w, http.StatusForbidden, map[string]any{"error": fmt.Sprintf("Wrong PIN. %d tries left.", triesLeft), "triesLeft": triesLeft})
		return
	}
	if hadKey {
		a.stats.Forget(id)
	}
	slog.Info("peer config issued", "audit", true, "actor", "setup link", "peer", name, "remote", ip)
	out, err := a.issue(id, k.String())
	if err != nil {
		writeErr(w, err)
		return
	}
	if e := a.apply(); e != "" {
		slog.Error("apply after setup link failed", "err", e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": out.Peer.Name, "config": out.Config, "qr": out.QR})
}
