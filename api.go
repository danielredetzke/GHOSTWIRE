package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// App wires the parts together and serves the HTTP API.
type App struct {
	store    *Store
	kernel   Kernel
	recon    *Reconciler
	stats    *Stats
	auth     *Auth
	tls      *webTLS
	logPath  string
	started  time.Time
	shutdown func() // graceful stop; systemd restarts the service
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var ue *userError
	switch {
	case errors.As(err, &ue):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": ue.msg})
	default:
		slog.Error("request failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid JSON: %v", err)
	}
	return nil
}

type ctxKey struct{}

func who(r *http.Request) *principal { return r.Context().Value(ctxKey{}).(*principal) }

func (a *App) audit(r *http.Request, msg string, args ...any) {
	p := who(r)
	slog.Info(msg, append([]any{"audit", true, "actor", p.Name, "remote", p.RemoteIP}, args...)...)
}

// guard requires authentication. adminOnly endpoints refuse API tokens;
// read-only tokens may only use GET.
func (a *App) guard(adminOnly bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.auth.Authenticate(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
			return
		}
		if adminOnly && !p.IsAdmin {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "only the admin account can do this"})
			return
		}
		if p.Scope == "ro" && r.Method != http.MethodGet {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "this token is read-only"})
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	}
}

// applyResult saves-then-applies: the config is already stored, so a kernel
// error is reported but does not undo the change.
func (a *App) apply() string {
	if err := a.recon.ApplyNow(); err != nil {
		return err.Error()
	}
	return ""
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	g := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, a.guard(false, h)) }
	adm := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, a.guard(true, h)) }

	mux.HandleFunc("POST /api/v1/auth/login", a.login)
	mux.HandleFunc("POST /api/v1/auth/logout", a.logout)
	g("GET /api/v1/auth/me", a.me)
	adm("POST /api/v1/auth/password", a.changePassword)

	g("GET /api/v1/status", a.status)
	g("GET /api/v1/stats", a.allStats)

	g("GET /api/v1/server", a.getServer)
	g("PATCH /api/v1/server", a.patchServer)
	g("POST /api/v1/server/rotate-key", a.rotateServerKey)
	g("GET /api/v1/server/detect-ip", a.detectIP)

	g("GET /api/v1/peers", a.listPeers)
	g("POST /api/v1/peers", a.createPeer)
	g("GET /api/v1/peers/{id}", a.getPeer)
	g("PATCH /api/v1/peers/{id}", a.patchPeer)
	g("DELETE /api/v1/peers/{id}", a.deletePeer)
	g("POST /api/v1/peers/{id}/enable", a.setEnabled(true))
	g("POST /api/v1/peers/{id}/disable", a.setEnabled(false))
	g("POST /api/v1/peers/{id}/issue-config", a.issueConfig)
	g("GET /api/v1/peers/{id}/stats", a.peerStats)

	adm("GET /api/v1/settings", a.getSettings)
	adm("PATCH /api/v1/settings", a.patchSettings)
	adm("POST /api/v1/restart", a.restart)
	adm("GET /api/v1/tokens", a.listTokens)
	adm("POST /api/v1/tokens", a.createToken)
	adm("DELETE /api/v1/tokens/{id}", a.deleteToken)
	adm("GET /api/v1/logs", a.logs)
	adm("GET /api/v1/logs/download", a.downloadLog)
	adm("GET /api/v1/backup", a.backup)
	adm("POST /api/v1/restore", a.restore)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such endpoint"})
	})
	mux.Handle("/", webHandler())

	csrf := http.NewCrossOriginProtection()
	return securityHeaders(csrf.Handler(mux))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// --- auth ---

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	var in struct{ Username, Password string }
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	ip := remoteIP(r)
	id, err := a.auth.Login(in.Username, in.Password, ip)
	if err != nil {
		slog.Warn("login failed", "user", in.Username, "remote", ip, "reason", err.Error())
		code := http.StatusUnauthorized
		if errors.Is(err, errLocked) {
			code = http.StatusTooManyRequests
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	cfg := a.store.Get()
	http.SetCookie(w, &http.Cookie{
		Name: cookieName(), Value: id, Path: "/", HttpOnly: true, Secure: r.TLS != nil,
		SameSite: http.SameSiteStrictMode, MaxAge: cfg.Web.SessionHours * 3600,
	})
	slog.Info("login", "audit", true, "actor", in.Username, "remote", ip)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName()); err == nil {
		a.auth.Logout(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName(), Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: r.TLS != nil})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) me(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	writeJSON(w, http.StatusOK, map[string]any{"name": p.Name, "isAdmin": p.IsAdmin, "scope": p.Scope, "version": version})
}

func (a *App) changePassword(w http.ResponseWriter, r *http.Request) {
	var in struct{ Current, New string }
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if !verifyPassword(a.store.Get().Admin.PasswordHash, in.Current) {
		writeErr(w, badRequest("current password is wrong"))
		return
	}
	if err := validatePassword(in.New); err != nil {
		writeErr(w, err)
		return
	}
	hash, err := hashPassword(in.New)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := a.store.Update(func(c *Config) error { c.Admin.PasswordHash = hash; return nil }); err != nil {
		writeErr(w, err)
		return
	}
	a.audit(r, "password changed")
	a.auth.DropSessions()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- status & stats ---

func (a *App) status(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Get()
	var online, enabled, never int
	var top string
	var topBytes int64
	for _, p := range cfg.Peers {
		s := a.stats.Summary(p.ID)
		if p.Enabled {
			enabled++
			if s.Online {
				online++
			}
		}
		if s.LastHandshake == nil {
			never++
		}
		if t := s.Down30d + s.Up30d; t > topBytes {
			topBytes, top = t, p.Name
		}
	}
	d24, u24 := sumPoints(a.stats.series(nil, "24h"))
	d30, u30 := sumPoints(a.stats.series(nil, "30d"))
	checks := a.kernel.Checks(cfg)
	last, applyErr := a.recon.Status()
	ac := Check{Name: "Last apply", OK: applyErr == nil, Detail: "applied " + last.Format(time.RFC3339)}
	if applyErr != nil {
		ac.Detail = applyErr.Error()
	}
	checks = append(checks, ac)
	healthy := true
	for _, c := range checks {
		healthy = healthy && c.OK
	}
	v4 := netip.MustParsePrefix(cfg.Server.IPv4)
	writeJSON(w, http.StatusOK, map[string]any{
		"version":     version,
		"interface":   cfg.Server.Interface,
		"listenPort":  cfg.Server.ListenPort,
		"endpoint":    endpointString(cfg),
		"ipv4":        cfg.Server.IPv4,
		"ipv6":        cfg.Server.IPv6,
		"ipv6Enabled": cfg.Server.IPv6Enabled,
		"capacity":    capacity(v4),
		"started":     a.started,
		"healthy":     healthy,
		"checks":      checks,
		"peers": map[string]int{
			"total": len(cfg.Peers), "enabled": enabled, "online": online,
			"disabled": len(cfg.Peers) - enabled, "never": never,
		},
		"traffic24h": map[string]int64{"down": d24, "up": u24},
		"traffic30d": map[string]int64{"down": d30, "up": u30},
		"topPeer30d": top,
	})
}

func validRange(r *http.Request) string {
	rng := r.URL.Query().Get("range")
	if !slices.Contains([]string{"24h", "7d", "30d", "90d"}, rng) {
		rng = "24h"
	}
	return rng
}

func (a *App) allStats(w http.ResponseWriter, r *http.Request) {
	rng := validRange(r)
	writeJSON(w, http.StatusOK, map[string]any{"range": rng, "points": a.stats.series(nil, rng)})
}

func (a *App) peerStats(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Get()
	if _, p := cfg.peerByID(r.PathValue("id")); p == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such peer"})
		return
	}
	rng := validRange(r)
	writeJSON(w, http.StatusOK, map[string]any{"range": rng, "points": a.stats.series([]string{r.PathValue("id")}, rng)})
}

// --- server ---

type serverView struct {
	Interface      string         `json:"interface"`
	PublicKey      string         `json:"publicKey"`
	KeyCreated     time.Time      `json:"keyCreated"`
	ListenPort     int            `json:"listenPort"`
	MTU            int            `json:"mtu"`
	IPv4           string         `json:"ipv4"`
	IPv6           string         `json:"ipv6"`
	IPv6Enabled    bool           `json:"ipv6Enabled"`
	Endpoint       string         `json:"endpoint"`
	EndpointPort   int            `json:"endpointPort"`
	UplinkV4       string         `json:"uplinkV4"`
	UplinkV6       string         `json:"uplinkV6"`
	DetectedV4     string         `json:"detectedUplinkV4"`
	DetectedV6     string         `json:"detectedUplinkV6"`
	NAT            bool           `json:"nat"`
	PeerToPeer     bool           `json:"peerToPeer"`
	LANAccess      bool           `json:"lanAccess"`
	OpenPort       bool           `json:"openPort"`
	ClientDefaults ClientDefaults `json:"clientDefaults"`
}

func (a *App) serverView(cfg *Config) serverView {
	s := cfg.Server
	return serverView{
		Interface: s.Interface, PublicKey: serverPublicKey(cfg), KeyCreated: s.KeyCreated,
		ListenPort: s.ListenPort, MTU: s.MTU, IPv4: s.IPv4, IPv6: s.IPv6, IPv6Enabled: s.IPv6Enabled,
		Endpoint: s.Endpoint, EndpointPort: s.EndpointPort, UplinkV4: s.UplinkV4, UplinkV6: s.UplinkV6,
		DetectedV4: a.kernel.Uplink(&Config{}, false), DetectedV6: a.kernel.Uplink(&Config{}, true),
		NAT: s.NAT, PeerToPeer: s.PeerToPeer, LANAccess: s.LANAccess, OpenPort: s.OpenPort,
		ClientDefaults: s.ClientDefaults,
	}
}

func (a *App) getServer(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.serverView(a.store.Get()))
}

// decodeFields reads a PATCH body as raw fields so absent and null differ.
func decodeFields(r *http.Request) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := readJSON(r, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func field[T any](m map[string]json.RawMessage, key string, dst *T) error {
	raw, ok := m[key]
	if !ok {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return badRequest("%s: %v", key, err)
	}
	return nil
}

func (a *App) patchServer(w http.ResponseWriter, r *http.Request) {
	m, err := decodeFields(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var changed []string
	var reissue bool
	err = a.store.Update(func(c *Config) error {
		s := &c.Server
		before := s.clientFacing()
		oldV4 := s.IPv4
		for _, f := range []struct {
			key string
			dst any
		}{
			{"listenPort", &s.ListenPort}, {"mtu", &s.MTU}, {"ipv4", &s.IPv4}, {"ipv6", &s.IPv6},
			{"ipv6Enabled", &s.IPv6Enabled}, {"endpoint", &s.Endpoint}, {"endpointPort", &s.EndpointPort},
			{"uplinkV4", &s.UplinkV4}, {"uplinkV6", &s.UplinkV6}, {"nat", &s.NAT}, {"peerToPeer", &s.PeerToPeer},
			{"lanAccess", &s.LANAccess}, {"openPort", &s.OpenPort}, {"clientDefaults", &s.ClientDefaults},
		} {
			if _, ok := m[f.key]; ok {
				if err := json.Unmarshal(m[f.key], f.dst); err != nil {
					return badRequest("%s: %v", f.key, err)
				}
				changed = append(changed, f.key)
			}
		}
		s.Endpoint = strings.TrimSpace(s.Endpoint)
		if s.IPv4 != oldV4 {
			if err := renumberPeers(c, oldV4); err != nil {
				return err
			}
		}
		reissue = before != s.clientFacing()
		return nil
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	a.audit(r, "server settings changed", "fields", changed)
	writeJSON(w, http.StatusOK, map[string]any{
		"server": a.serverView(a.store.Get()), "applyError": a.apply(), "reissueNeeded": reissue,
	})
}

// clientFacing captures the settings baked into issued client configs;
// changing any of them means existing devices need a new config.
func (s *Server) clientFacing() string {
	return fmt.Sprint(s.ListenPort, s.EndpointPort, s.Endpoint, s.IPv4, s.IPv6, s.IPv6Enabled)
}

// renumberPeers moves peers into a new IPv4 network, keeping each host part.
func renumberPeers(c *Config, oldNet string) error {
	oldP, err := netip.ParsePrefix(oldNet)
	if err != nil {
		return err
	}
	newP, err := netip.ParsePrefix(c.Server.IPv4)
	if err != nil || !newP.Addr().Is4() {
		return badRequest("IPv4 network must be an IPv4 CIDR")
	}
	newP = newP.Masked()
	c.Server.IPv4 = newP.String()
	for i := range c.Peers {
		old := netip.MustParseAddr(c.Peers[i].IPv4)
		host := addrToU32(old) - addrToU32(oldP.Addr())
		if host >= 1<<(32-newP.Bits())-1 {
			return badRequest("%s is too small for the existing peers", newP)
		}
		c.Peers[i].IPv4 = u32ToAddr(addrToU32(newP.Addr()) + host).String()
	}
	return nil
}

func (a *App) rotateServerKey(w http.ResponseWriter, r *http.Request) {
	key, err := newPrivateKey()
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := a.store.Update(func(c *Config) error {
		c.Server.PrivateKey = key.String()
		c.Server.KeyCreated = time.Now().UTC()
		return nil
	}); err != nil {
		writeErr(w, err)
		return
	}
	a.audit(r, "server key rotated")
	writeJSON(w, http.StatusOK, map[string]any{"server": a.serverView(a.store.Get()), "applyError": a.apply()})
}

func (a *App) detectIP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://checkip.amazonaws.com", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeErr(w, badRequest("could not detect the public IP: %v", err))
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 100))
	ip, err := netip.ParseAddr(strings.TrimSpace(string(b)))
	if err != nil {
		writeErr(w, badRequest("unexpected answer from the IP service"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ip": ip.String()})
}

// --- peers ---

type peerView struct {
	ID           string      `json:"id"`
	Name         string      `json:"name"`
	Note         string      `json:"note"`
	Enabled      bool        `json:"enabled"`
	PublicKey    string      `json:"publicKey"`
	HasPSK       bool        `json:"hasPresharedKey"`
	IPv4         string      `json:"ipv4"`
	IPv6         string      `json:"ipv6,omitempty"`
	DNS          []string    `json:"dns"`        // null = server default
	AllowedIPs   []string    `json:"allowedIPs"` // null = server default
	Keepalive    *int        `json:"keepalive"`  // null = server default
	EffDNS       []string    `json:"effectiveDNS"`
	EffAllowed   []string    `json:"effectiveAllowedIPs"`
	EffKeepalive int         `json:"effectiveKeepalive"`
	Created      time.Time   `json:"created"`
	ConfigIssued *time.Time  `json:"configIssued"`
	Stats        PeerSummary `json:"stats"`
}

func (a *App) peerView(c *Config, p *Peer) peerView {
	v := peerView{
		ID: p.ID, Name: p.Name, Note: p.Note, Enabled: p.Enabled, PublicKey: p.PublicKey,
		HasPSK: p.PresharedKey != "", IPv4: p.IPv4, DNS: p.DNS, AllowedIPs: p.AllowedIPs, Keepalive: p.Keepalive,
		EffDNS: peerDNS(c, p), EffAllowed: peerAllowedIPs(c, p), EffKeepalive: peerKeepalive(c, p),
		Created: p.Created, ConfigIssued: p.ConfigIssued, Stats: a.stats.Summary(p.ID),
	}
	if c.Server.IPv6Enabled {
		v.IPv6 = mapIPv6(netip.MustParsePrefix(c.Server.IPv6), netip.MustParseAddr(p.IPv4)).String()
	}
	return v
}

func (a *App) listPeers(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Get()
	out := make([]peerView, 0, len(cfg.Peers))
	for i := range cfg.Peers {
		out = append(out, a.peerView(cfg, &cfg.Peers[i]))
	}
	v4 := netip.MustParsePrefix(cfg.Server.IPv4)
	writeJSON(w, http.StatusOK, map[string]any{"peers": out, "capacity": capacity(v4), "network": cfg.Server.IPv4})
}

func (a *App) getPeer(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Get()
	_, p := cfg.peerByID(r.PathValue("id"))
	if p == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such peer"})
		return
	}
	writeJSON(w, http.StatusOK, a.peerView(cfg, p))
}

// issuedConfig is returned exactly once; the private key is not stored.
type issuedConfig struct {
	Peer       peerView `json:"peer"`
	Config     string   `json:"config"`
	QR         string   `json:"qr,omitempty"`
	HasPrivKey bool     `json:"includesPrivateKey"`
	ApplyError string   `json:"applyError"`
}

// newKeys returns a fresh key pair, or only the given public key when the
// client made its own keys.
func newKeys(clientPublic string) (priv, pub string, err error) {
	if clientPublic != "" {
		k, err := wgtypes.ParseKey(strings.TrimSpace(clientPublic))
		if err != nil {
			return "", "", badRequest("public key is not a valid WireGuard key")
		}
		return "", k.String(), nil
	}
	k, err := newPrivateKey()
	if err != nil {
		return "", "", err
	}
	return k.String(), k.PublicKey().String(), nil
}

func (a *App) issue(id, priv string) (issuedConfig, error) {
	cfg := a.store.Get()
	_, p := cfg.peerByID(id)
	out := issuedConfig{Peer: a.peerView(cfg, p), Config: clientConfig(cfg, p, priv), HasPrivKey: priv != ""}
	if priv != "" {
		qr, err := qrDataURL(out.Config)
		if err != nil {
			return out, err
		}
		out.QR = qr
	}
	return out, nil
}

func (a *App) createPeer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name         string   `json:"name"`
		Note         string   `json:"note"`
		IPv4         string   `json:"ipv4"`
		DNS          []string `json:"dns"`
		AllowedIPs   []string `json:"allowedIPs"`
		Keepalive    *int     `json:"keepalive"`
		PublicKey    string   `json:"publicKey"`
		PresharedKey *bool    `json:"presharedKey"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	priv, pub, err := newKeys(in.PublicKey)
	if err != nil {
		writeErr(w, err)
		return
	}
	p := Peer{
		ID: newID(), Name: in.Name, Note: strings.TrimSpace(in.Note), Enabled: true, PublicKey: pub,
		DNS: in.DNS, AllowedIPs: in.AllowedIPs, Keepalive: in.Keepalive, Created: time.Now().UTC(),
	}
	if in.PresharedKey == nil || *in.PresharedKey {
		psk, err := newPresharedKey()
		if err != nil {
			writeErr(w, err)
			return
		}
		p.PresharedKey = psk.String()
	}
	now := time.Now().UTC()
	p.ConfigIssued = &now
	err = a.store.Update(func(c *Config) error {
		if err := validatePeerName(p.Name); err != nil {
			return &userError{err.Error()}
		}
		if in.IPv4 == "" || in.IPv4 == "auto" {
			ip, err := nextFreeIPv4(c)
			if err != nil {
				return err
			}
			p.IPv4 = ip.String()
		} else {
			p.IPv4 = strings.TrimSpace(in.IPv4)
		}
		c.Peers = append(c.Peers, p)
		return nil
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	a.audit(r, "peer created", "peer", p.Name, "ip", p.IPv4)
	out, err := a.issue(p.ID, priv)
	if err != nil {
		writeErr(w, err)
		return
	}
	out.ApplyError = a.apply()
	writeJSON(w, http.StatusCreated, out)
}

func (a *App) patchPeer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m, err := decodeFields(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var name string
	var changed []string
	err = a.store.Update(func(c *Config) error {
		_, p := c.peerByID(id)
		if p == nil {
			return badRequest("no such peer")
		}
		for _, f := range []struct {
			key string
			dst any
		}{
			{"name", &p.Name}, {"note", &p.Note}, {"ipv4", &p.IPv4}, {"enabled", &p.Enabled},
		} {
			if raw, ok := m[f.key]; ok {
				if err := json.Unmarshal(raw, f.dst); err != nil {
					return badRequest("%s: %v", f.key, err)
				}
				changed = append(changed, f.key)
			}
		}
		// For the overrides, null means "use the server default".
		if raw, ok := m["dns"]; ok {
			p.DNS = nil
			if err := json.Unmarshal(raw, &p.DNS); err != nil {
				return badRequest("dns: %v", err)
			}
			changed = append(changed, "dns")
		}
		if raw, ok := m["allowedIPs"]; ok {
			p.AllowedIPs = nil
			if err := json.Unmarshal(raw, &p.AllowedIPs); err != nil {
				return badRequest("allowedIPs: %v", err)
			}
			changed = append(changed, "allowedIPs")
		}
		if raw, ok := m["keepalive"]; ok {
			p.Keepalive = nil
			if err := json.Unmarshal(raw, &p.Keepalive); err != nil {
				return badRequest("keepalive: %v", err)
			}
			changed = append(changed, "keepalive")
		}
		p.Name = strings.TrimSpace(p.Name)
		p.Note = strings.TrimSpace(p.Note)
		name = p.Name
		return nil
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	a.audit(r, "peer updated", "peer", name, "fields", changed)
	cfg := a.store.Get()
	_, p := cfg.peerByID(id)
	writeJSON(w, http.StatusOK, map[string]any{"peer": a.peerView(cfg, p), "applyError": a.apply()})
}

func (a *App) setEnabled(on bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var name string
		if err := a.store.Update(func(c *Config) error {
			_, p := c.peerByID(id)
			if p == nil {
				return badRequest("no such peer")
			}
			p.Enabled, name = on, p.Name
			return nil
		}); err != nil {
			writeErr(w, err)
			return
		}
		a.audit(r, map[bool]string{true: "peer enabled", false: "peer disabled"}[on], "peer", name)
		cfg := a.store.Get()
		_, p := cfg.peerByID(id)
		writeJSON(w, http.StatusOK, map[string]any{"peer": a.peerView(cfg, p), "applyError": a.apply()})
	}
}

func (a *App) deletePeer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var name string
	if err := a.store.Update(func(c *Config) error {
		i, p := c.peerByID(id)
		if p == nil {
			return badRequest("no such peer")
		}
		name = p.Name
		c.Peers = slices.Delete(c.Peers, i, i+1)
		return nil
	}); err != nil {
		writeErr(w, err)
		return
	}
	a.audit(r, "peer deleted", "peer", name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applyError": a.apply()})
}

// issueConfig replaces the peer's keys. The old device stops working.
func (a *App) issueConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in struct {
		PublicKey string `json:"publicKey"`
	}
	if r.ContentLength > 0 {
		if err := readJSON(r, &in); err != nil {
			writeErr(w, err)
			return
		}
	}
	priv, pub, err := newKeys(in.PublicKey)
	if err != nil {
		writeErr(w, err)
		return
	}
	psk, err := newPresharedKey()
	if err != nil {
		writeErr(w, err)
		return
	}
	var name string
	if err := a.store.Update(func(c *Config) error {
		_, p := c.peerByID(id)
		if p == nil {
			return badRequest("no such peer")
		}
		now := time.Now().UTC()
		p.PublicKey, p.ConfigIssued, name = pub, &now, p.Name
		if p.PresharedKey != "" {
			p.PresharedKey = psk.String()
		}
		return nil
	}); err != nil {
		writeErr(w, err)
		return
	}
	a.stats.Forget(id)
	a.audit(r, "peer config issued", "peer", name)
	out, err := a.issue(id, priv)
	if err != nil {
		writeErr(w, err)
		return
	}
	out.ApplyError = a.apply()
	writeJSON(w, http.StatusOK, out)
}

// --- settings, tokens, logs, backup ---

func (a *App) getSettings(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Get()
	writeJSON(w, http.StatusOK, map[string]any{
		"web":           cfg.Web,
		"log":           cfg.Log,
		"adminUsername": cfg.Admin.Username,
		"fingerprint":   a.tls.Fingerprint(),
		"logPath":       a.logPath,
	})
}

func (a *App) patchSettings(w http.ResponseWriter, r *http.Request) {
	m, err := decodeFields(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var restart bool
	err = a.store.Update(func(c *Config) error {
		if err := field(m, "adminUsername", &c.Admin.Username); err != nil {
			return err
		}
		if strings.TrimSpace(c.Admin.Username) == "" {
			return badRequest("username cannot be empty")
		}
		before, _ := json.Marshal(c.Web)
		if err := field(m, "web", &c.Web); err != nil {
			return err
		}
		after, _ := json.Marshal(c.Web)
		restart = string(before) != string(after)
		return field(m, "log", &c.Log)
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	cfg := a.store.Get()
	logLevel.Set(parseLevel(cfg.Log.Level))
	a.audit(r, "app settings changed", "restartRequired", restart)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restartRequired": restart})
}

func (a *App) restart(w http.ResponseWriter, r *http.Request) {
	a.audit(r, "service restart requested")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	go func() {
		time.Sleep(500 * time.Millisecond)
		a.shutdown()
	}()
}

type tokenView struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Scope    string    `json:"scope"`
	Created  time.Time `json:"created"`
	LastUsed *tokenUse `json:"lastUsed"`
}

func (a *App) listTokens(w http.ResponseWriter, r *http.Request) {
	out := []tokenView{}
	for _, t := range a.store.Get().APITokens {
		out = append(out, tokenView{t.ID, t.Name, t.Scope, t.Created, a.auth.TokenUse(t.ID)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

func (a *App) createToken(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name, Scope string }
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 64 {
		writeErr(w, badRequest("token name must be 1–64 characters"))
		return
	}
	if in.Scope != "ro" {
		in.Scope = "rw"
	}
	secret := tokenPrefix + randomString(32)
	t := APIToken{ID: newID(), Name: in.Name, Hash: hashToken(secret), Scope: in.Scope, Created: time.Now().UTC()}
	if err := a.store.Update(func(c *Config) error { c.APITokens = append(c.APITokens, t); return nil }); err != nil {
		writeErr(w, err)
		return
	}
	a.audit(r, "api token created", "token", t.Name, "scope", t.Scope)
	// The pairing payload lets the iOS app connect by scanning one QR code.
	pairing, _ := json.Marshal(map[string]string{
		"url": "https://" + r.Host, "token": secret, "fingerprint": a.tls.Fingerprint(),
	})
	qr, _ := qrDataURL(string(pairing))
	writeJSON(w, http.StatusCreated, map[string]any{
		"token": secret, "id": t.ID, "name": t.Name, "scope": t.Scope, "pairing": string(pairing), "qr": qr,
	})
}

func (a *App) deleteToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var name string
	if err := a.store.Update(func(c *Config) error {
		i := slices.IndexFunc(c.APITokens, func(t APIToken) bool { return t.ID == id })
		if i < 0 {
			return badRequest("no such token")
		}
		name = c.APITokens[i].Name
		c.APITokens = slices.Delete(c.APITokens, i, i+1)
		return nil
	}); err != nil {
		writeErr(w, err)
		return
	}
	a.audit(r, "api token revoked", "token", name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) logs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 200
	if _, err := fmt.Sscan(q.Get("limit"), &limit); err != nil || limit < 1 || limit > 2000 {
		limit = 200
	}
	lines, err := readLogTail(a.logPath, limit, q.Get("level"), q.Get("audit") == "1")
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

func (a *App) downloadLog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", appName+".jsonl"))
	http.ServeFile(w, r, a.logPath)
}

func (a *App) backup(w http.ResponseWriter, r *http.Request) {
	a.audit(r, "backup downloaded")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", appName+"-backup-"+time.Now().Format("2006-01-02")+".json"))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(a.store.Get())
}

func (a *App) restore(w http.ResponseWriter, r *http.Request) {
	var in Config
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if in.Server.PrivateKey == "" {
		writeErr(w, badRequest("this file has no server key; is it a backup of this app?"))
		return
	}
	if err := a.store.Update(func(c *Config) error { *c = in; return nil }); err != nil {
		writeErr(w, err)
		return
	}
	a.audit(r, "backup restored", "peers", len(in.Peers))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applyError": a.apply(), "restartRequired": true})
}
