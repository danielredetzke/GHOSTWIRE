package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// --- passwords (argon2id, PHC string format) ---

const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
)

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

func verifyPassword(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	b64 := base64.RawStdEncoding
	salt, err1 := b64.DecodeString(parts[4])
	want, err2 := b64.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func validatePassword(pw string) error {
	if len([]rune(pw)) < 12 {
		return badRequest("password must be at least 12 characters")
	}
	return nil
}

// --- random secrets ---

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

const tokenPrefix = "wgt_"

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// --- sessions, token use and login throttling (in memory) ---

type principal struct {
	Name     string // the username, or the token name
	UserID   string // the user, or the token's owner
	Scope    string // rw | ro
	TokenID  string
	IsAdmin  bool // a signed-in user, not an API token
	RemoteIP string
	// MustChangePassword blocks everything but changing the password.
	MustChangePassword bool
}

type session struct {
	userID string
	// stamp is the user's password hash at sign-in: a changed or reset
	// password ends every session started with the old one.
	stamp   string
	expires time.Time
}

type tokenUse struct {
	At time.Time `json:"at"`
	IP string    `json:"ip"`
}

type failState struct {
	count int
	until time.Time
}

type Auth struct {
	store *Store

	mu       sync.Mutex
	sessions map[string]*session
	used     map[string]tokenUse
	logins   map[string]tokenUse // last sign-in per user ID
	fails    map[string]*failState
}

const (
	maxFailures = 5
	lockoutTime = 15 * time.Minute
)

func newAuth(s *Store) *Auth {
	return &Auth{store: s, sessions: map[string]*session{}, used: map[string]tokenUse{}, logins: map[string]tokenUse{}, fails: map[string]*failState{}}
}

func cookieName() string { return appName + "_session" }

var errLocked = errors.New("too many failed attempts, try again later")

// Login checks the credentials and returns a new session id.
func (a *Auth) Login(user, pw, ip string) (string, error) {
	a.mu.Lock()
	f := a.fails[ip]
	if f != nil && time.Now().Before(f.until) {
		a.mu.Unlock()
		return "", errLocked
	}
	a.mu.Unlock()

	cfg := a.store.Get()
	if !cfg.passwordSet() {
		return "", errors.New("no password is set; run: " + appName + " passwd")
	}
	// An unknown username costs as much time as a wrong password, so the
	// answer time does not tell which usernames exist.
	u := cfg.userByName(strings.TrimSpace(user))
	okUser := u != nil && u.PasswordHash != ""
	hash := dummyHash()
	if okUser {
		hash = u.PasswordHash
	}
	okPw := verifyPassword(hash, pw)

	a.mu.Lock()
	defer a.mu.Unlock()
	if !okUser || !okPw {
		if f == nil {
			f = &failState{}
			a.fails[ip] = f
		}
		f.count++
		if f.count >= maxFailures {
			f.count = 0
			f.until = time.Now().Add(lockoutTime)
		}
		return "", errors.New("wrong username or password")
	}
	delete(a.fails, ip)
	a.logins[u.ID] = tokenUse{At: time.Now(), IP: ip}
	return a.newSessionLocked(cfg, u), nil
}

// NewSession signs a user in again, e.g. after they changed their password.
func (a *Auth) NewSession(u *User) string {
	cfg := a.store.Get()
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.newSessionLocked(cfg, u)
}

func (a *Auth) newSessionLocked(cfg *Config, u *User) string {
	id := randomString(32)
	a.sessions[id] = &session{userID: u.ID, stamp: u.PasswordHash, expires: time.Now().Add(time.Duration(cfg.Web.SessionHours) * time.Hour)}
	return id
}

// LastLogin returns when the user last signed in since the service started.
func (a *Auth) LastLogin(userID string) *tokenUse {
	a.mu.Lock()
	defer a.mu.Unlock()
	if u, ok := a.logins[userID]; ok {
		return &u
	}
	return nil
}

var dummy struct {
	once sync.Once
	hash string
}

func dummyHash() string {
	dummy.once.Do(func() { dummy.hash, _ = hashPassword(randomString(16)) })
	return dummy.hash
}

func (a *Auth) Logout(id string) {
	a.mu.Lock()
	delete(a.sessions, id)
	a.mu.Unlock()
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	// Behind a local reverse proxy the real client is in X-Forwarded-For.
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	return host
}

// Authenticate accepts a session cookie or "Authorization: Bearer wgt_...".
func (a *Auth) Authenticate(r *http.Request) (*principal, bool) {
	ip := remoteIP(r)
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		if !strings.HasPrefix(tok, tokenPrefix) {
			return nil, false
		}
		want := hashToken(tok)
		for _, t := range a.store.Get().APITokens {
			if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(want)) == 1 {
				a.mu.Lock()
				a.used[t.ID] = tokenUse{At: time.Now(), IP: ip}
				a.mu.Unlock()
				return &principal{Name: t.Name, UserID: t.UserID, Scope: t.Scope, TokenID: t.ID, RemoteIP: ip}, true
			}
		}
		return nil, false
	}
	c, err := r.Cookie(cookieName())
	if err != nil {
		return nil, false
	}
	cfg := a.store.Get()
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[c.Value]
	if s == nil || time.Now().After(s.expires) {
		delete(a.sessions, c.Value)
		return nil, false
	}
	// The user is looked up on every request: a deleted user or a changed
	// password ends the session at once.
	_, u := cfg.userByID(s.userID)
	if u == nil || u.PasswordHash != s.stamp {
		delete(a.sessions, c.Value)
		return nil, false
	}
	return &principal{Name: u.Username, UserID: u.ID, Scope: "rw", IsAdmin: true, RemoteIP: ip, MustChangePassword: u.MustChangePassword}, true
}

func (a *Auth) TokenUse(id string) *tokenUse {
	a.mu.Lock()
	defer a.mu.Unlock()
	if u, ok := a.used[id]; ok {
		return &u
	}
	return nil
}

// sweep removes expired sessions and stale lockouts.
func (a *Auth) sweep() {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for id, s := range a.sessions {
		if now.After(s.expires) {
			delete(a.sessions, id)
		}
	}
	for ip, f := range a.fails {
		if now.After(f.until) && f.count == 0 {
			delete(a.fails, ip)
		}
	}
}
