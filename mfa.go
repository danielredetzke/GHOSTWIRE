package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// Two-step sign-in for the web interface: an authenticator app (TOTP) and
// passkeys (WebAuthn, also on a YubiKey), plus one-time recovery codes. A
// passkey signs in on its own and also serves as the second step after a
// password. API tokens never need a second step.
//
// After a correct password, a user with two-step sign-in gets a short-lived
// ticket instead of a session; the ticket and a code or key turn into the
// session. A passkey signs in on its own, without username and password.

// UserMFA is a user's two-step sign-in setup, stored in config.json.
type UserMFA struct {
	TOTPSecret    string     `json:"totpSecret,omitempty"` // base32
	TOTPAdded     *time.Time `json:"totpAdded,omitempty"`
	Keys          []MFAKey   `json:"keys,omitempty"`
	RecoveryCodes []string   `json:"recoveryCodes,omitempty"` // SHA-256 of the unused codes
	Handle        []byte     `json:"handle,omitempty"`        // WebAuthn user handle
}

// MFAKey is a passkey. Keys added by v0.3.0 as plain security keys have
// Passkey false; they still work as the second step.
type MFAKey struct {
	ID         string              `json:"id"`
	Name       string              `json:"name"`
	Passkey    bool                `json:"passkey"` // discoverable: signs in without a password
	Created    time.Time           `json:"created"`
	LastUsed   *time.Time          `json:"lastUsed,omitempty"`
	Credential webauthn.Credential `json:"credential"`
}

func (u *User) hasMFA() bool {
	return u.MFA != nil && (u.MFA.TOTPSecret != "" || len(u.MFA.Keys) > 0)
}

const (
	ticketTTL     = 5 * time.Minute
	recoveryCount = 10
	totpPeriod    = 30
	totpDigits    = 6
	maxKeyName    = 64
)

// --- TOTP (RFC 6238, SHA-1, 6 digits, 30 s) ---

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func newTOTPSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b32.EncodeToString(b)
}

func totpCode(key []byte, counter uint64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, v%1_000_000)
}

// totpMatch returns the time step the code belongs to, allowing one step of
// clock drift either way.
func totpMatch(secret, code string, now time.Time) (uint64, bool) {
	key, err := b32.DecodeString(strings.ToUpper(secret))
	code = strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, code)
	if err != nil || len(code) != totpDigits {
		return 0, false
	}
	step := uint64(now.Unix() / totpPeriod)
	for _, c := range []uint64{step, step - 1, step + 1} {
		if subtle.ConstantTimeCompare([]byte(totpCode(key, c)), []byte(code)) == 1 {
			return c, true
		}
	}
	return 0, false
}

func totpURI(secret, username string) string {
	label := url.PathEscape(appName + ":" + username)
	return "otpauth://totp/" + label + "?secret=" + secret + "&issuer=" + url.QueryEscape(appName) + "&algorithm=SHA1&digits=6&period=30"
}

// --- recovery codes ---

const recoveryAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// newRecoveryCodes returns codes to show once and their hashes to store.
func newRecoveryCodes() (codes, hashes []string) {
	for range recoveryCount {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			panic(err)
		}
		var s strings.Builder
		for i, x := range b {
			if i == 4 {
				s.WriteByte('-')
			}
			s.WriteByte(recoveryAlphabet[int(x)%len(recoveryAlphabet)])
		}
		codes = append(codes, s.String())
		hashes = append(hashes, hashRecovery(s.String()))
	}
	return codes, hashes
}

func hashRecovery(code string) string {
	norm := strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		return r
	}, strings.ToUpper(code))
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])
}

// --- WebAuthn ---

// waUser adapts a User to the webauthn library.
type waUser struct{ u *User }

func (w waUser) WebAuthnID() []byte          { return w.u.MFA.Handle }
func (w waUser) WebAuthnName() string        { return w.u.Username }
func (w waUser) WebAuthnDisplayName() string { return w.u.Username }
func (w waUser) WebAuthnCredentials() []webauthn.Credential {
	var out []webauthn.Credential
	if w.u.MFA != nil {
		for _, k := range w.u.MFA.Keys {
			out = append(out, k.Credential)
		}
	}
	return out
}

// keysAvailable reports whether passkeys can work on this
// address: WebAuthn needs a domain name (not an IP address) and a
// certificate the browser trusts, or localhost.
func (a *App) keysAvailable(r *http.Request) bool {
	host := hostOnly(r.Host)
	if host == "localhost" {
		return true
	}
	return host != "" && net.ParseIP(host) == nil && a.store.Get().Web.TLS.Mode != "selfsigned"
}

func (a *App) webAuthn(r *http.Request) (*webauthn.WebAuthn, error) {
	if !a.keysAvailable(r) {
		return nil, badRequest("passkeys need a domain name with a trusted certificate")
	}
	scheme := "https"
	if r.TLS == nil && hostOnly(r.Host) == "localhost" {
		scheme = "http"
	}
	return webauthn.New(&webauthn.Config{
		RPID: hostOnly(r.Host), RPDisplayName: appName, RPOrigins: []string{scheme + "://" + r.Host},
	})
}

// --- pending ceremonies, kept in memory ---

// ticket is a sign-in waiting for its second step.
type ticket struct {
	userID  string
	ip      string
	expires time.Time
	fails   int
	key     *webauthn.SessionData // a passkey challenge, once asked for
}

type ceremony struct {
	userID  string // "" for a passkey sign-in
	passkey bool
	data    *webauthn.SessionData
	expires time.Time
}

type mfaState struct {
	tickets   map[string]*ticket
	logins    map[string]*ceremony // passkey sign-ins by id
	enrolls   map[string]*ceremony // key registrations by user ID
	totpSetup map[string]string    // TOTP secrets waiting for their first code, by user ID
	totpLast  map[string]uint64    // last time step used per user, so a code works once
}

func newMFAState() mfaState {
	return mfaState{tickets: map[string]*ticket{}, logins: map[string]*ceremony{}, enrolls: map[string]*ceremony{},
		totpSetup: map[string]string{}, totpLast: map[string]uint64{}}
}

var errBadTicket = errors.New("the sign-in expired; enter your password again")

// failLocked counts a failed attempt from ip toward the lockout. a.mu must
// be held.
func (a *Auth) failLocked(ip string) {
	f := a.fails[ip]
	if f == nil {
		f = &failState{}
		a.fails[ip] = f
	}
	f.count++
	if f.count >= maxFailures {
		f.count = 0
		f.until = time.Now().Add(lockoutTime)
	}
}

func (a *Auth) lockedLocked(ip string) bool {
	f := a.fails[ip]
	return f != nil && time.Now().Before(f.until)
}

// newTicket starts the second step for a user whose password was right.
// a.mu must be held.
func (a *Auth) newTicketLocked(u *User, ip string) string {
	id := randomString(32)
	a.mfa.tickets[id] = &ticket{userID: u.ID, ip: ip, expires: time.Now().Add(ticketTTL)}
	return id
}

// ticketUser returns the live ticket and its user.
func (a *Auth) ticketUser(id, ip string) (*ticket, *User, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lockedLocked(ip) {
		return nil, nil, errLocked
	}
	t := a.mfa.tickets[id]
	if t == nil || time.Now().After(t.expires) {
		delete(a.mfa.tickets, id)
		return nil, nil, errBadTicket
	}
	_, u := a.store.Get().userByID(t.userID)
	if u == nil {
		delete(a.mfa.tickets, id)
		return nil, nil, errBadTicket
	}
	return t, u, nil
}

// ticketFailed counts a wrong code; five end the ticket.
func (a *Auth) ticketFailed(id, ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failLocked(ip)
	if t := a.mfa.tickets[id]; t != nil {
		t.fails++
		if t.fails >= maxFailures {
			delete(a.mfa.tickets, id)
		}
	}
}

// finishSignIn turns a passed second step into a session.
func (a *Auth) finishSignIn(u *User, ip string) string {
	cfg := a.store.Get()
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.fails, ip)
	a.logins[u.ID] = tokenUse{At: time.Now(), IP: ip}
	return a.newSessionLocked(cfg, u, sessionInfo{Started: time.Now(), IP: ip})
}

// --- sign-in endpoints (public) ---

func (a *App) signedIn(w http.ResponseWriter, r *http.Request, u *User, how string) {
	ip := remoteIP(r)
	a.setSessionCookie(w, r, a.auth.finishSignIn(u, ip))
	slog.Info("login", "audit", true, "actor", u.Username, "remote", ip, "method", how)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) signInFailed(w http.ResponseWriter, err error) {
	code := http.StatusUnauthorized
	if errors.Is(err, errLocked) {
		code = http.StatusTooManyRequests
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// signInOptions tells the sign-in page whether to offer a passkey.
func (a *App) signInOptions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"passkeys": a.keysAvailable(r)})
}

func (a *App) loginTOTP(w http.ResponseWriter, r *http.Request) {
	var in struct{ Ticket, Code string }
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	ip := remoteIP(r)
	_, u, err := a.auth.ticketUser(in.Ticket, ip)
	if err != nil {
		a.signInFailed(w, err)
		return
	}
	if u.MFA == nil || u.MFA.TOTPSecret == "" || !a.auth.useTOTP(u.ID, u.MFA.TOTPSecret, in.Code) {
		a.auth.ticketFailed(in.Ticket, ip)
		slog.Warn("login failed", "user", u.Username, "remote", ip, "reason", "wrong authenticator code")
		a.signInFailed(w, errors.New("wrong code"))
		return
	}
	a.auth.dropTicket(in.Ticket)
	a.signedIn(w, r, u, "totp")
}

// useTOTP checks a code and makes sure it is not used twice.
func (a *Auth) useTOTP(userID, secret, code string) bool {
	step, ok := totpMatch(secret, code, time.Now())
	if !ok {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if last, seen := a.mfa.totpLast[userID]; seen && step <= last {
		return false
	}
	a.mfa.totpLast[userID] = step
	return true
}

// ticketUserID returns the ticket's user without checking the lockout.
func (a *Auth) ticketUserID(id string) (string, *User) {
	a.mu.Lock()
	t := a.mfa.tickets[id]
	a.mu.Unlock()
	if t == nil {
		return "", nil
	}
	_, u := a.store.Get().userByID(t.userID)
	return t.userID, u
}

// mfaMethods lists what the second step can use: "key", "totp", "recovery".
func mfaMethods(u *User) []string {
	out := []string{}
	if u == nil || u.MFA == nil {
		return out
	}
	if len(u.MFA.Keys) > 0 {
		out = append(out, "key")
	}
	if u.MFA.TOTPSecret != "" {
		out = append(out, "totp")
	}
	if len(u.MFA.RecoveryCodes) > 0 {
		out = append(out, "recovery")
	}
	return out
}

func (a *Auth) dropTicket(id string) {
	a.mu.Lock()
	delete(a.mfa.tickets, id)
	a.mu.Unlock()
}

func (a *App) loginRecovery(w http.ResponseWriter, r *http.Request) {
	var in struct{ Ticket, Code string }
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	ip := remoteIP(r)
	_, u, err := a.auth.ticketUser(in.Ticket, ip)
	if err != nil {
		a.signInFailed(w, err)
		return
	}
	h := hashRecovery(in.Code)
	var left int
	used := false
	_ = a.store.Update(func(c *Config) error {
		_, cu := c.userByID(u.ID)
		if cu == nil || cu.MFA == nil {
			return nil
		}
		for i, x := range cu.MFA.RecoveryCodes {
			if subtle.ConstantTimeCompare([]byte(x), []byte(h)) == 1 {
				cu.MFA.RecoveryCodes = slices.Delete(cu.MFA.RecoveryCodes, i, i+1)
				used = true
				break
			}
		}
		left = len(cu.MFA.RecoveryCodes)
		return nil
	})
	if !used {
		a.auth.ticketFailed(in.Ticket, ip)
		slog.Warn("login failed", "user", u.Username, "remote", ip, "reason", "wrong recovery code")
		a.signInFailed(w, errors.New("wrong or used recovery code"))
		return
	}
	a.auth.dropTicket(in.Ticket)
	slog.Info("recovery code used", "audit", true, "actor", u.Username, "remote", ip, "left", left)
	a.signedIn(w, r, u, "recovery code")
}

// loginKeyBegin asks for one of the user's passkeys, as the second step.
func (a *App) loginKeyBegin(w http.ResponseWriter, r *http.Request) {
	var in struct{ Ticket string }
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	t, u, err := a.auth.ticketUser(in.Ticket, remoteIP(r))
	if err != nil {
		a.signInFailed(w, err)
		return
	}
	wa, err := a.webAuthn(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if u.MFA == nil || len(u.MFA.Keys) == 0 {
		writeErr(w, badRequest("no passkey is set up"))
		return
	}
	opts, data, err := wa.BeginLogin(waUser{u}, webauthn.WithUserVerification(protocol.VerificationDiscouraged))
	if err != nil {
		writeErr(w, err)
		return
	}
	a.auth.mu.Lock()
	t.key = data
	a.auth.mu.Unlock()
	writeJSON(w, http.StatusOK, opts)
}

// loginKeyFinish checks the key's answer. The ticket is in the query, the
// body is the browser's credential.
func (a *App) loginKeyFinish(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("ticket")
	ip := remoteIP(r)
	t, u, err := a.auth.ticketUser(id, ip)
	if err != nil {
		a.signInFailed(w, err)
		return
	}
	wa, err := a.webAuthn(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	a.auth.mu.Lock()
	data := t.key
	t.key = nil
	a.auth.mu.Unlock()
	if data == nil {
		writeErr(w, badRequest("ask for the key first"))
		return
	}
	cred, err := wa.FinishLogin(waUser{u}, *data, r)
	if err != nil {
		a.auth.ticketFailed(id, ip)
		slog.Warn("login failed", "user", u.Username, "remote", ip, "reason", "passkey: "+err.Error())
		a.signInFailed(w, errors.New("the passkey was not accepted"))
		return
	}
	a.keyUsed(u.ID, cred)
	a.auth.dropTicket(id)
	a.signedIn(w, r, u, "passkey")
}

// keyUsed stores the key's new signature counter and when it was used.
func (a *App) keyUsed(userID string, cred *webauthn.Credential) {
	now := time.Now().UTC()
	_ = a.store.Update(func(c *Config) error {
		if _, u := c.userByID(userID); u != nil && u.MFA != nil {
			for i := range u.MFA.Keys {
				if k := &u.MFA.Keys[i]; bytes.Equal(k.Credential.ID, cred.ID) {
					k.Credential.Authenticator = cred.Authenticator
					k.Credential.Flags = cred.Flags
					k.LastUsed = &now
				}
			}
		}
		return nil
	})
}

// loginPasskeyBegin starts a sign-in with a passkey alone.
func (a *App) loginPasskeyBegin(w http.ResponseWriter, r *http.Request) {
	wa, err := a.webAuthn(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	opts, data, err := wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		writeErr(w, err)
		return
	}
	id := randomString(24)
	a.auth.mu.Lock()
	a.auth.mfa.logins[id] = &ceremony{data: data, expires: time.Now().Add(ticketTTL)}
	a.auth.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "options": opts})
}

func (a *App) loginPasskeyFinish(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	ip := remoteIP(r)
	a.auth.mu.Lock()
	cer := a.auth.mfa.logins[id]
	delete(a.auth.mfa.logins, id)
	locked := a.auth.lockedLocked(ip)
	a.auth.mu.Unlock()
	if locked {
		a.signInFailed(w, errLocked)
		return
	}
	if cer == nil || time.Now().After(cer.expires) {
		a.signInFailed(w, errors.New("the sign-in expired; try again"))
		return
	}
	wa, err := a.webAuthn(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	cfg := a.store.Get()
	var found *User
	cred, err := wa.FinishDiscoverableLogin(func(rawID, handle []byte) (webauthn.User, error) {
		for i := range cfg.Users {
			u := &cfg.Users[i]
			if u.MFA != nil && len(u.MFA.Handle) > 0 && bytes.Equal(u.MFA.Handle, handle) {
				for _, k := range u.MFA.Keys {
					if k.Passkey && bytes.Equal(k.Credential.ID, rawID) {
						found = u
						return waUser{u}, nil
					}
				}
			}
		}
		return nil, errors.New("unknown passkey")
	}, *cer.data, r)
	if err != nil || found == nil {
		a.auth.mu.Lock()
		a.auth.failLocked(ip)
		a.auth.mu.Unlock()
		slog.Warn("login failed", "remote", ip, "reason", "passkey not accepted")
		a.signInFailed(w, errors.New("this passkey is not known here"))
		return
	}
	a.keyUsed(found.ID, cred)
	a.signedIn(w, r, found, "passkey")
}

// --- managing your own two-step sign-in (signed-in users) ---

type keyView struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Passkey  bool       `json:"passkey"`
	Created  time.Time  `json:"created"`
	LastUsed *time.Time `json:"lastUsed"`
}

func (a *App) mfaStatus(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Get()
	_, u := cfg.userByID(who(r).UserID)
	if u == nil {
		writeErr(w, badRequest("no such user"))
		return
	}
	out := map[string]any{"totp": false, "totpAdded": nil, "keys": []keyView{}, "recoveryLeft": 0,
		"keysAvailable": a.keysAvailable(r), "required": cfg.SignIn.RequireMFA}
	if m := u.MFA; m != nil {
		keys := []keyView{}
		for _, k := range m.Keys {
			keys = append(keys, keyView{k.ID, k.Name, k.Passkey, k.Created, k.LastUsed})
		}
		out["totp"], out["totpAdded"], out["keys"], out["recoveryLeft"] = m.TOTPSecret != "", m.TOTPAdded, keys, len(m.RecoveryCodes)
	}
	writeJSON(w, http.StatusOK, out)
}

// addFirstCodes gives a user recovery codes with their first method. It
// returns the codes to show, or nil when the user already has codes. It runs
// inside a store update.
func addFirstCodes(u *User) []string {
	if len(u.MFA.RecoveryCodes) > 0 {
		return nil
	}
	codes, hashes := newRecoveryCodes()
	u.MFA.RecoveryCodes = hashes
	return codes
}

func (a *App) totpSetup(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	secret := newTOTPSecret()
	a.auth.mu.Lock()
	a.auth.mfa.totpSetup[p.UserID] = secret
	a.auth.mu.Unlock()
	_, u := a.store.Get().userByID(p.UserID)
	if u == nil {
		writeErr(w, badRequest("no such user"))
		return
	}
	uri := totpURI(secret, u.Username)
	qr, _ := qrDataURL(uri)
	writeJSON(w, http.StatusOK, map[string]any{"secret": secret, "uri": uri, "qr": qr})
}

func (a *App) totpConfirm(w http.ResponseWriter, r *http.Request) {
	var in struct{ Code string }
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	p := who(r)
	a.auth.mu.Lock()
	secret := a.auth.mfa.totpSetup[p.UserID]
	a.auth.mu.Unlock()
	if secret == "" {
		writeErr(w, badRequest("start the setup again"))
		return
	}
	if !a.auth.useTOTP(p.UserID, secret, in.Code) {
		writeErr(w, badRequest("wrong code; check the time on your phone and try the next one"))
		return
	}
	var codes []string
	now := time.Now().UTC()
	if err := a.store.Update(func(c *Config) error {
		_, u := c.userByID(p.UserID)
		if u == nil {
			return badRequest("no such user")
		}
		if u.MFA == nil {
			u.MFA = &UserMFA{}
		}
		u.MFA.TOTPSecret, u.MFA.TOTPAdded = secret, &now
		codes = addFirstCodes(u)
		return nil
	}); err != nil {
		writeErr(w, err)
		return
	}
	a.auth.mu.Lock()
	delete(a.auth.mfa.totpSetup, p.UserID)
	a.auth.mu.Unlock()
	a.audit(r, "authenticator app added")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "recoveryCodes": codes})
}

// lastMethodCheck refuses to remove the last method while two-step sign-in
// is required.
func lastMethodCheck(c *Config, u *User) error {
	if c.SignIn.RequireMFA && !u.hasMFA() {
		return badRequest("two-step sign-in is required here; add another method first")
	}
	if !u.hasMFA() && u.MFA != nil {
		u.MFA.RecoveryCodes = nil
	}
	return nil
}

func (a *App) totpRemove(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	if err := a.store.Update(func(c *Config) error {
		_, u := c.userByID(p.UserID)
		if u == nil || u.MFA == nil || u.MFA.TOTPSecret == "" {
			return badRequest("no authenticator app is set up")
		}
		u.MFA.TOTPSecret, u.MFA.TOTPAdded = "", nil
		return lastMethodCheck(c, u)
	}); err != nil {
		writeErr(w, err)
		return
	}
	a.audit(r, "authenticator app removed")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// keyBegin starts adding a passkey.
func (a *App) keyBegin(w http.ResponseWriter, r *http.Request) {
	wa, err := a.webAuthn(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	p := who(r)
	// The user handle is made once and never changes.
	if err := a.store.Update(func(c *Config) error {
		_, u := c.userByID(p.UserID)
		if u == nil {
			return badRequest("no such user")
		}
		if u.MFA == nil {
			u.MFA = &UserMFA{}
		}
		if len(u.MFA.Handle) == 0 {
			u.MFA.Handle = make([]byte, 32)
			if _, err := rand.Read(u.MFA.Handle); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		writeErr(w, err)
		return
	}
	_, u := a.store.Get().userByID(p.UserID)
	var exclude []protocol.CredentialDescriptor
	for _, k := range u.MFA.Keys {
		exclude = append(exclude, k.Credential.Descriptor())
	}
	sel := protocol.AuthenticatorSelection{ResidentKey: protocol.ResidentKeyRequirementRequired, UserVerification: protocol.VerificationRequired}
	opts, data, err := wa.BeginRegistration(waUser{u}, webauthn.WithAuthenticatorSelection(sel), webauthn.WithExclusions(exclude))
	if err != nil {
		writeErr(w, err)
		return
	}
	a.auth.mu.Lock()
	a.auth.mfa.enrolls[p.UserID] = &ceremony{userID: p.UserID, passkey: true, data: data, expires: time.Now().Add(ticketTTL)}
	a.auth.mu.Unlock()
	writeJSON(w, http.StatusOK, opts)
}

// keyFinish stores the new key. The name is in the query, the body is the
// browser's credential.
func (a *App) keyFinish(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	a.auth.mu.Lock()
	cer := a.auth.mfa.enrolls[p.UserID]
	delete(a.auth.mfa.enrolls, p.UserID)
	a.auth.mu.Unlock()
	if cer == nil || time.Now().After(cer.expires) {
		writeErr(w, badRequest("adding the key took too long; try again"))
		return
	}
	wa, err := a.webAuthn(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	_, u := a.store.Get().userByID(p.UserID)
	if u == nil {
		writeErr(w, badRequest("no such user"))
		return
	}
	cred, err := wa.FinishRegistration(waUser{u}, *cer.data, r)
	if err != nil {
		writeErr(w, badRequest("the key was not accepted: %v", err))
		return
	}
	if name == "" {
		name = "Passkey"
	}
	if len(name) > maxKeyName {
		name = name[:maxKeyName]
	}
	var codes []string
	key := MFAKey{ID: newID(), Name: name, Passkey: cer.passkey, Created: time.Now().UTC(), Credential: *cred}
	if err := a.store.Update(func(c *Config) error {
		_, u := c.userByID(p.UserID)
		if u == nil || u.MFA == nil {
			return badRequest("no such user")
		}
		u.MFA.Keys = append(u.MFA.Keys, key)
		codes = addFirstCodes(u)
		return nil
	}); err != nil {
		writeErr(w, err)
		return
	}
	a.audit(r, "passkey added", "key", name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "recoveryCodes": codes})
}

func (a *App) keyRename(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name string }
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > maxKeyName {
		writeErr(w, badRequest("name must be 1–%d characters", maxKeyName))
		return
	}
	id := r.PathValue("id")
	if err := a.store.Update(func(c *Config) error {
		_, u := c.userByID(who(r).UserID)
		if u == nil || u.MFA == nil {
			return badRequest("no such key")
		}
		for i := range u.MFA.Keys {
			if u.MFA.Keys[i].ID == id {
				u.MFA.Keys[i].Name = in.Name
				return nil
			}
		}
		return badRequest("no such key")
	}); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) keyRemove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var name string
	if err := a.store.Update(func(c *Config) error {
		_, u := c.userByID(who(r).UserID)
		if u == nil || u.MFA == nil {
			return badRequest("no such key")
		}
		i := slices.IndexFunc(u.MFA.Keys, func(k MFAKey) bool { return k.ID == id })
		if i < 0 {
			return badRequest("no such key")
		}
		name = u.MFA.Keys[i].Name
		u.MFA.Keys = slices.Delete(u.MFA.Keys, i, i+1)
		return lastMethodCheck(c, u)
	}); err != nil {
		writeErr(w, err)
		return
	}
	a.audit(r, "passkey removed", "key", name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) newRecoveryCodesHandler(w http.ResponseWriter, r *http.Request) {
	var codes []string
	if err := a.store.Update(func(c *Config) error {
		_, u := c.userByID(who(r).UserID)
		if u == nil || !u.hasMFA() {
			return badRequest("turn on two-step sign-in first")
		}
		var hashes []string
		codes, hashes = newRecoveryCodes()
		u.MFA.RecoveryCodes = hashes
		return nil
	}); err != nil {
		writeErr(w, err)
		return
	}
	a.audit(r, "recovery codes replaced")
	writeJSON(w, http.StatusOK, map[string]any{"recoveryCodes": codes})
}

// resetMFA removes another user's two-step sign-in, for a lost phone or key.
// Their user handle stays, so passkeys they still hold are just unknown.
func (a *App) resetMFA(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == who(r).UserID {
		writeErr(w, badRequest("manage your own two-step sign-in under My account"))
		return
	}
	var name string
	if err := a.store.Update(func(c *Config) error {
		_, u := c.userByID(id)
		if u == nil {
			return badRequest("no such user")
		}
		name = u.Username
		if u.MFA != nil {
			u.MFA = &UserMFA{Handle: u.MFA.Handle}
		}
		return nil
	}); err != nil {
		writeErr(w, err)
		return
	}
	a.audit(r, "two-step sign-in reset", "user", name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// mfaSummary is what user lists show.
func mfaSummary(u *User) map[string]any {
	out := map[string]any{"totp": false, "keys": 0, "passkeys": 0}
	if m := u.MFA; m != nil {
		keys, passkeys := 0, 0
		for _, k := range m.Keys {
			if k.Passkey {
				passkeys++
			} else {
				keys++
			}
		}
		out["totp"], out["keys"], out["passkeys"] = m.TOTPSecret != "", keys, passkeys
	}
	return out
}
