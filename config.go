package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Config is the complete desired state of the service. It is persisted as
// config.json and is the single source of truth: the kernel (interface, peers,
// firewall) is reconciled to match it.
type Config struct {
	Version   int        `json:"version"`
	Web       WebConfig  `json:"web"`
	Users     []User     `json:"users"`
	APITokens []APIToken `json:"apiTokens"`
	// Admin is the single account of config version 1; applyDefaults moves
	// it into Users.
	Admin  *Admin      `json:"admin,omitempty"`
	Server Server      `json:"server"`
	Peers  []Peer      `json:"peers"`
	Log    LogConfig   `json:"log"`
	Stats  StatsConfig `json:"stats"`
	Decoy  DecoyConfig `json:"decoy"`
}

// DecoyConfig replaces the web interface with a stock web server page.
// The API keeps working, so the iOS app can turn it off again.
type DecoyConfig struct {
	Enabled bool   `json:"enabled"`
	Page    string `json:"page"` // nginx | apache | soon
}

// StatsConfig sets how long traffic history is kept in stats.json.
type StatsConfig struct {
	HourlyHours int `json:"hourlyHours"` // hourly buckets, for the 24 h charts
	DailyDays   int `json:"dailyDays"`   // daily buckets and connection history
	// GeoIP looks up country and network of peer addresses in the DB-IP Lite
	// databases, downloaded monthly. Default on.
	GeoIP *bool `json:"geoip,omitempty"`
}

func (c StatsConfig) geoEnabled() bool { return c.GeoIP == nil || *c.GeoIP }

// Limits for the retention settings.
const (
	minLogSizeMB, maxLogSizeMB   = 1, 1000
	minLogFiles, maxLogFiles     = 1, 100
	minHourlyHours, maxHourlyHrs = 24, 24 * 31
	minDailyDays, maxDailyDays   = 7, 3660
)

type WebConfig struct {
	Listen       string    `json:"listen"`     // HTTPS (or HTTP when tls.mode is "off") listen address
	HTTPListen   string    `json:"httpListen"` // plain HTTP for ACME http-01 and redirects; "" disables
	TLS          TLSConfig `json:"tls"`
	SessionHours int       `json:"sessionHours"`
}

type TLSConfig struct {
	Mode     string `json:"mode"` // acme | selfsigned | files | off
	Domain   string `json:"domain,omitempty"`
	Email    string `json:"email,omitempty"`
	Staging  bool   `json:"staging,omitempty"` // use the Let's Encrypt staging CA
	CertFile string `json:"certFile,omitempty"`
	KeyFile  string `json:"keyFile,omitempty"`
}

type Admin struct {
	Username     string `json:"username"`
	PasswordHash string `json:"passwordHash"`
}

// User is an account for the web interface. Every user is an admin.
type User struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	Note         string `json:"note,omitempty"`
	PasswordHash string `json:"passwordHash"`
	// MustChangePassword is set when an admin chose a temporary password:
	// the user can do nothing else until they pick their own.
	MustChangePassword bool      `json:"mustChangePassword,omitempty"`
	Created            time.Time `json:"created"`
}

type APIToken struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Hash    string    `json:"hash"`
	Scope   string    `json:"scope"`  // rw | ro
	UserID  string    `json:"userId"` // the user who created it
	Created time.Time `json:"created"`
}

type Server struct {
	Interface      string         `json:"interface"`
	PrivateKey     string         `json:"privateKey"`
	KeyCreated     time.Time      `json:"keyCreated"`
	ListenPort     int            `json:"listenPort"`
	MTU            int            `json:"mtu"`
	IPv4           string         `json:"ipv4"` // tunnel network, e.g. 10.84.12.0/24
	IPv6           string         `json:"ipv6"` // tunnel network, e.g. fd11:5ee:bad:c0de::/64
	IPv6Enabled    bool           `json:"ipv6Enabled"`
	Endpoint       string         `json:"endpoint"`     // host name or IP clients connect to
	EndpointPort   int            `json:"endpointPort"` // 0 = listenPort
	UplinkV4       string         `json:"uplinkV4"`     // "" = interface of the default route
	UplinkV6       string         `json:"uplinkV6"`
	NAT            bool           `json:"nat"`
	PeerToPeer     bool           `json:"peerToPeer"`
	LANAccess      bool           `json:"lanAccess"`
	OpenPort       bool           `json:"openPort"`
	ClientDefaults ClientDefaults `json:"clientDefaults"`
}

type ClientDefaults struct {
	DNS        []string `json:"dns"`
	AllowedIPs []string `json:"allowedIPs"`
	Keepalive  int      `json:"keepalive"`
}

// Peer is one client. Its private key is never stored: it is shown once when
// the config is issued.
type Peer struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Note         string   `json:"note"`
	Enabled      bool     `json:"enabled"`
	PublicKey    string   `json:"publicKey"`
	PresharedKey string   `json:"presharedKey,omitempty"`
	IPv4         string   `json:"ipv4"`
	DNS          []string `json:"dns,omitempty"`        // nil = server default
	AllowedIPs   []string `json:"allowedIPs,omitempty"` // nil = server default
	Keepalive    *int     `json:"keepalive,omitempty"`  // nil = server default
	// LatencyCheck says when the server pings the peer through the tunnel:
	// "" (off), "active" (while the device sends traffic) or "always".
	LatencyCheck string     `json:"latencyCheck,omitempty"`
	Created      time.Time  `json:"created"`
	ConfigIssued *time.Time `json:"configIssued,omitempty"`
	// Setup is a pending one-time setup link. A peer created with a link has
	// no public key until the link is opened.
	Setup *SetupLink `json:"setup,omitempty"`
}

// hasKey reports whether the peer has a public key, i.e. it can be in the
// kernel. A peer waiting for its setup link has none yet.
func (p *Peer) hasKey() bool { return p.PublicKey != "" }

type LogConfig struct {
	Level     string `json:"level"` // debug | info | warn | error
	MaxSizeMB int    `json:"maxSizeMB"`
	MaxFiles  int    `json:"maxFiles"`
}

const configVersion = 2

// applyDefaults fills zero values. It never overwrites values that are set,
// so a minimal hand-written config.json grows into a complete one.
func (c *Config) applyDefaults() {
	if c.Version < configVersion {
		c.Version = configVersion
	}
	if c.Web.Listen == "" {
		c.Web.Listen = ":443"
	}
	if c.Web.TLS.Mode == "" {
		if c.Web.TLS.Domain != "" {
			c.Web.TLS.Mode = "acme"
		} else {
			c.Web.TLS.Mode = "selfsigned"
		}
	}
	if c.Web.TLS.Mode == "acme" && c.Web.HTTPListen == "" {
		c.Web.HTTPListen = ":80"
	}
	if c.Web.SessionHours == 0 {
		c.Web.SessionHours = 12
	}
	if len(c.Users) == 0 {
		u := User{ID: newID(), Username: "admin", Created: time.Now().UTC()}
		if c.Admin != nil {
			u.Username = cmp.Or(c.Admin.Username, "admin")
			u.PasswordHash = c.Admin.PasswordHash
		}
		c.Users = []User{u}
	}
	c.Admin = nil
	for i := range c.APITokens {
		if c.APITokens[i].UserID == "" {
			c.APITokens[i].UserID = c.Users[0].ID // tokens from before users existed
		}
	}
	s := &c.Server
	if s.Interface == "" {
		s.Interface = "wg0"
	}
	if s.ListenPort == 0 {
		s.ListenPort = 51820
	}
	if s.MTU == 0 {
		s.MTU = 1420
	}
	if s.ClientDefaults.DNS == nil {
		s.ClientDefaults.DNS = []string{"9.9.9.9", "149.112.112.112"}
	}
	if s.ClientDefaults.AllowedIPs == nil {
		s.ClientDefaults.AllowedIPs = []string{"0.0.0.0/0", "::/0"}
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.MaxSizeMB == 0 {
		c.Log.MaxSizeMB = 10
	}
	if c.Log.MaxFiles == 0 {
		c.Log.MaxFiles = 5
	}
	if c.Stats.HourlyHours == 0 {
		c.Stats.HourlyHours = 48
	}
	if c.Stats.DailyDays == 0 {
		c.Stats.DailyDays = 400
	}
	if c.Decoy.Page == "" {
		c.Decoy.Page = "nginx"
	}
	if c.APITokens == nil {
		c.APITokens = []APIToken{}
	}
	if c.Peers == nil {
		c.Peers = []Peer{}
	}
}

// initServer runs once, when the server has no key yet: it generates the key,
// picks a free tunnel subnet and turns on the defaults that are booleans.
func (c *Config) initServer() (bool, error) {
	s := &c.Server
	if s.PrivateKey != "" {
		return false, nil
	}
	key, err := newPrivateKey()
	if err != nil {
		return false, err
	}
	s.PrivateKey = key.String()
	s.KeyCreated = time.Now().UTC()
	if s.IPv4 == "" {
		n, err := randomSubnet(24)
		if err != nil {
			return false, err
		}
		s.IPv4 = n.String()
	}
	if s.IPv6 == "" {
		s.IPv6 = "fd11:5ee:bad:c0de::/64"
		s.IPv6Enabled = hasGlobalIPv6()
	}
	s.NAT = true
	s.PeerToPeer = true
	s.OpenPort = true
	if s.Endpoint == "" {
		s.Endpoint = c.Web.TLS.Domain
	}
	return true, nil
}

var peerNameRe = regexp.MustCompile(`^[a-zA-Z0-9.@_-]{1,32}$`)

func validateUsername(name string) error {
	if !peerNameRe.MatchString(name) || strings.HasPrefix(name, "-") || strings.HasPrefix(name, ".") {
		return errors.New("username must be 1–32 characters: letters, digits and . @ _ -, not starting with - or .")
	}
	return nil
}

func validatePeerName(name string) error {
	switch {
	case !peerNameRe.MatchString(name):
		return errors.New("name must be 1–32 characters: letters, digits and . @ _ -")
	case strings.Trim(name, "0123456789") == "":
		return errors.New("name cannot be only digits")
	case strings.HasPrefix(name, "-") || strings.HasPrefix(name, "."):
		return errors.New("name cannot start with - or .")
	case name == "server":
		return errors.New("name \"server\" is reserved")
	}
	return nil
}

func validateHostList(list []string, field string, wantCIDR bool) error {
	for _, v := range list {
		if wantCIDR {
			if _, err := netip.ParsePrefix(v); err != nil {
				return fmt.Errorf("%s: %q is not a network in CIDR notation", field, v)
			}
		} else if _, err := netip.ParseAddr(v); err != nil {
			return fmt.Errorf("%s: %q is not an IP address", field, v)
		}
	}
	return nil
}

// validate checks the whole config for consistency. It runs before every save.
func (c *Config) validate() error {
	s := &c.Server
	if s.ListenPort < 1 || s.ListenPort > 65535 {
		return errors.New("listen port must be 1–65535")
	}
	if s.EndpointPort < 0 || s.EndpointPort > 65535 {
		return errors.New("endpoint port must be 0–65535")
	}
	if s.MTU < 1280 || s.MTU > 9000 {
		return errors.New("MTU must be 1280–9000")
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,15}$`).MatchString(s.Interface) {
		return errors.New("interface name must be 1–15 characters: letters, digits, _ and -")
	}
	v4, err := netip.ParsePrefix(s.IPv4)
	if err != nil || !v4.Addr().Is4() || v4.Bits() > 30 || v4.Bits() < 8 {
		return errors.New("IPv4 network must be an IPv4 CIDR between /8 and /30")
	}
	if v4.Masked() != v4 {
		return fmt.Errorf("IPv4 network must be the network address, e.g. %s", v4.Masked())
	}
	v6, err := netip.ParsePrefix(s.IPv6)
	if err != nil || !v6.Addr().Is6() || v6.Bits() > 96 {
		return errors.New("IPv6 network must be an IPv6 CIDR of /96 or larger")
	}
	if v6.Masked() != v6 {
		return fmt.Errorf("IPv6 network must be the network address, e.g. %s", v6.Masked())
	}
	if s.Endpoint != "" && strings.ContainsAny(s.Endpoint, " /:") && net.ParseIP(s.Endpoint) == nil {
		return errors.New("endpoint must be a host name or IP address without port")
	}
	if err := validateHostList(s.ClientDefaults.DNS, "DNS", false); err != nil {
		return err
	}
	if err := validateHostList(s.ClientDefaults.AllowedIPs, "AllowedIPs", true); err != nil {
		return err
	}
	if s.ClientDefaults.Keepalive < 0 || s.ClientDefaults.Keepalive > 3600 {
		return errors.New("keepalive must be 0–3600 seconds")
	}
	if l := c.Log; l.MaxSizeMB < minLogSizeMB || l.MaxSizeMB > maxLogSizeMB {
		return fmt.Errorf("log file size must be %d–%d MB", minLogSizeMB, maxLogSizeMB)
	} else if l.MaxFiles < minLogFiles || l.MaxFiles > maxLogFiles {
		return fmt.Errorf("kept log files must be %d–%d", minLogFiles, maxLogFiles)
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log level must be debug, info, warn or error")
	}
	if st := c.Stats; st.HourlyHours < minHourlyHours || st.HourlyHours > maxHourlyHrs {
		return fmt.Errorf("hourly traffic history must be %d–%d hours", minHourlyHours, maxHourlyHrs)
	} else if st.DailyDays < minDailyDays || st.DailyDays > maxDailyDays {
		return fmt.Errorf("daily traffic history must be %d–%d days", minDailyDays, maxDailyDays)
	}
	if _, ok := decoyPages[c.Decoy.Page]; !ok {
		return fmt.Errorf("unknown decoy page %q", c.Decoy.Page)
	}
	switch c.Web.TLS.Mode {
	case "acme":
		if c.Web.TLS.Domain == "" {
			return errors.New("tls.domain is required for Let's Encrypt")
		}
	case "files":
		if c.Web.TLS.CertFile == "" || c.Web.TLS.KeyFile == "" {
			return errors.New("tls.certFile and tls.keyFile are required for mode \"files\"")
		}
	case "selfsigned", "off":
	default:
		return fmt.Errorf("unknown tls.mode %q", c.Web.TLS.Mode)
	}

	if len(c.Users) == 0 {
		return errors.New("at least one user is required")
	}
	userIDs := map[string]bool{}
	usernames := map[string]bool{}
	for _, u := range c.Users {
		if err := validateUsername(u.Username); err != nil {
			return fmt.Errorf("user %q: %w", u.Username, err)
		}
		if usernames[strings.ToLower(u.Username)] {
			return fmt.Errorf("username %q is used twice", u.Username)
		}
		if u.ID == "" || userIDs[u.ID] {
			return fmt.Errorf("user %q: missing or duplicate id", u.Username)
		}
		if len(u.Note) > 200 {
			return fmt.Errorf("user %q: note must be at most 200 characters", u.Username)
		}
		usernames[strings.ToLower(u.Username)] = true
		userIDs[u.ID] = true
	}
	for _, t := range c.APITokens {
		if !userIDs[t.UserID] {
			return fmt.Errorf("API token %q belongs to no user", t.Name)
		}
	}

	names := map[string]bool{}
	ips := map[netip.Addr]bool{}
	keys := map[string]bool{}
	for _, p := range c.Peers {
		if err := validatePeerName(p.Name); err != nil {
			return fmt.Errorf("peer %q: %w", p.Name, err)
		}
		if names[p.Name] {
			return fmt.Errorf("peer name %q is used twice", p.Name)
		}
		names[p.Name] = true
		ip, err := netip.ParseAddr(p.IPv4)
		if err != nil || !v4.Contains(ip) {
			return fmt.Errorf("peer %q: address %s is outside %s", p.Name, p.IPv4, v4)
		}
		if ip == v4.Addr() || ip == serverIPv4(v4) || ip == lastAddr(v4) {
			return fmt.Errorf("peer %q: address %s is reserved", p.Name, ip)
		}
		if ips[ip] {
			return fmt.Errorf("address %s is used twice", ip)
		}
		ips[ip] = true
		if p.hasKey() && keys[p.PublicKey] {
			return fmt.Errorf("peer %q: public key is used by another peer", p.Name)
		}
		keys[p.PublicKey] = true
		if err := validateHostList(p.DNS, "DNS", false); err != nil {
			return fmt.Errorf("peer %q: %w", p.Name, err)
		}
		if err := validateHostList(p.AllowedIPs, "AllowedIPs", true); err != nil {
			return fmt.Errorf("peer %q: %w", p.Name, err)
		}
		if p.Keepalive != nil && (*p.Keepalive < 0 || *p.Keepalive > 3600) {
			return fmt.Errorf("peer %q: keepalive must be 0–3600 seconds", p.Name)
		}
		if !validLatencyCheck(p.LatencyCheck) {
			return fmt.Errorf("peer %q: latency check must be off, active or always", p.Name)
		}
	}
	return nil
}

func (c *Config) userByID(id string) (int, *User) {
	for i := range c.Users {
		if c.Users[i].ID == id {
			return i, &c.Users[i]
		}
	}
	return -1, nil
}

// userByName finds a user regardless of letter case.
func (c *Config) userByName(name string) *User {
	for i := range c.Users {
		if strings.EqualFold(c.Users[i].Username, name) {
			return &c.Users[i]
		}
	}
	return nil
}

// passwordSet reports whether anyone can sign in yet.
func (c *Config) passwordSet() bool {
	return slices.ContainsFunc(c.Users, func(u User) bool { return u.PasswordHash != "" })
}

func (c *Config) peerByID(id string) (int, *Peer) {
	for i := range c.Peers {
		if c.Peers[i].ID == id {
			return i, &c.Peers[i]
		}
	}
	return -1, nil
}

func (c *Config) clone() *Config {
	b, _ := json.Marshal(c)
	var out Config
	_ = json.Unmarshal(b, &out)
	return &out
}

// Store guards the config and persists every change atomically.
type Store struct {
	mu   sync.Mutex
	path string
	cfg  *Config
	// onChange is called after a successful update, outside the lock.
	onChange func(old, new *Config)
}

func loadConfigFile(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		b = []byte("{}")
	} else if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Version > configVersion {
		return nil, fmt.Errorf("%s: config version %d is newer than this program supports", path, c.Version)
	}
	c.applyDefaults()
	return &c, nil
}

func openStore(path string) (*Store, error) {
	c, err := loadConfigFile(path)
	if err != nil {
		return nil, err
	}
	if _, err := c.initServer(); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s := &Store{path: path, cfg: c}
	if err := writeFileAtomic(path, c, 0o600); err != nil {
		return nil, err
	}
	return s, nil
}

// Get returns a deep copy that the caller may read freely.
func (s *Store) Get() *Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.clone()
}

// Update applies fn to a copy, validates and saves it, then swaps it in.
func (s *Store) Update(fn func(c *Config) error) error {
	s.mu.Lock()
	old := s.cfg
	next := old.clone()
	if err := fn(next); err != nil {
		s.mu.Unlock()
		return err
	}
	next.applyDefaults()
	if err := next.validate(); err != nil {
		s.mu.Unlock()
		return &userError{err.Error()}
	}
	if err := writeFileAtomic(s.path, next, 0o600); err != nil {
		s.mu.Unlock()
		return err
	}
	s.cfg = next
	s.mu.Unlock()
	if s.onChange != nil {
		s.onChange(old.clone(), next.clone())
	}
	return nil
}

// Reload re-reads config.json from disk, e.g. after "-passwd" changed it.
func (s *Store) Reload() error {
	c, err := loadConfigFile(s.path)
	if err != nil {
		return err
	}
	if err := c.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	old := s.cfg
	s.cfg = c
	s.mu.Unlock()
	if s.onChange != nil {
		s.onChange(old.clone(), c.clone())
	}
	return nil
}

// writeFileAtomic writes JSON to a temp file in the same directory, syncs it
// and renames it over the target, so a crash never leaves a partial file.
func writeFileAtomic(path string, v any, mode os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	// When root edits the file (e.g. "-passwd" under sudo), keep the owner so
	// the service user can still read it.
	if os.Geteuid() == 0 {
		if st, err := os.Stat(path); err == nil {
			if sys, ok := st.Sys().(*syscall.Stat_t); ok {
				_ = f.Chown(int(sys.Uid), int(sys.Gid))
			}
		}
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// userError marks errors caused by invalid input; the API returns them as 400.
type userError struct{ msg string }

func (e *userError) Error() string { return e.msg }

func badRequest(format string, a ...any) error { return &userError{fmt.Sprintf(format, a...)} }
