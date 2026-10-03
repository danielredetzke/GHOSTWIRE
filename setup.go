package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

// The binary installs, updates and removes itself. These commands need root;
// the service they set up runs as the unprivileged user serviceName.

var (
	installDir  = "/opt/" + serviceName
	installBin  = filepath.Join(installDir, appName)
	oldBin      = installBin + ".old" // exists only while an update runs
	configFile  = filepath.Join(installDir, "config.json")
	unitPath    = "/etc/systemd/system/" + serviceName + ".service"
	sysctlPath  = "/etc/sysctl.d/99-" + serviceName + ".conf"
	modulesPath = "/etc/modules-load.d/" + serviceName + ".conf"
)

func usage() {
	fmt.Fprintf(os.Stderr, `%s %s — WireGuard server manager

Usage (as root):
  %s install [-domain vpn.example.net] [-email you@example.net] [-endpoint host] [-port 51820] [-y]
        set up user, folder, config, sysctls and systemd service; start it.
        In a terminal it asks for the settings no flag gave; -y never asks
  %s update [-force]
        replace the installed binary with this one and restart
  %s uninstall [-purge] [-y]
        remove the service, interface and firewall table (-purge also deletes %s)
  %s passwd
        set the admin password of the installed service
  %s version

Without a command it runs the service:
  %s [-config path] [-passwd] [-check] [-down]
`, appName, version, appName, appName, appName, installDir, appName, appName, appName)
	flag.PrintDefaults()
}

func runCommand(cmd string, args []string) error {
	switch cmd {
	case "install":
		return cmdInstall(args)
	case "update":
		return cmdUpdate(args)
	case "uninstall":
		return cmdUninstall(args)
	case "passwd":
		return cmdPasswd(args)
	case "version":
		fmt.Println(appName, version)
		return nil
	case "help":
		usage()
		return nil
	}
	usage()
	return fmt.Errorf("unknown command %q", cmd)
}

// ---------- helpers ----------

func step(format string, a ...any) { fmt.Printf("• "+format+"\n", a...) }

func requireRoot() error {
	if runtime.GOOS != "linux" {
		return errors.New("this command only works on Linux")
	}
	if os.Geteuid() != 0 {
		return errors.New("run as root, e.g. with sudo")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("systemd is required (systemctl not found)")
	}
	return nil
}

func sh(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func shOut(name string, args ...string) string {
	out, _ := exec.Command(name, args...).Output()
	return strings.TrimSpace(string(out))
}

func serviceUser() (uid, gid int, err error) {
	u, err := user.Lookup(serviceName)
	if err != nil {
		return 0, 0, err
	}
	uid, _ = strconv.Atoi(u.Uid)
	gid, _ = strconv.Atoi(u.Gid)
	return uid, gid, nil
}

func fileHash(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	_, _ = io.Copy(h, f)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// copyFile writes src to dst through a temp file and a rename, so a running
// binary is never overwritten in place ("text file busy") or half-written.
func copyFile(src, dst string, mode os.FileMode, uid, gid int) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chown(uid, gid); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// writeIfChanged writes content to path when it differs and reports whether
// it did.
func writeIfChanged(path, content string, mode os.FileMode) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, []byte(content)) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, []byte(content), mode)
}

// installedVersion asks the installed binary for its version.
func installedVersion(path string) string {
	f := strings.Fields(shOut(path, "-version"))
	if len(f) == 2 {
		return f[1]
	}
	return "unknown"
}

// compareVersions compares "1.2.3" style versions (an optional "v" prefix
// and a git-describe suffix like "-4-gabc123" are ignored). ok is false when
// either side is not such a version.
func compareVersions(a, b string) (cmp int, ok bool) {
	parse := func(s string) ([3]int, bool) {
		var v [3]int
		s = strings.TrimPrefix(s, "v")
		if i := strings.IndexByte(s, '-'); i >= 0 {
			s = s[:i]
		}
		parts := strings.Split(s, ".")
		if len(parts) != 3 {
			return v, false
		}
		for i, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil {
				return v, false
			}
			v[i] = n
		}
		return v, true
	}
	va, ok1 := parse(a)
	vb, ok2 := parse(b)
	if !ok1 || !ok2 {
		return 0, false
	}
	for i := range 3 {
		if va[i] != vb[i] {
			if va[i] < vb[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// ---------- system files ----------

func unitFile() string {
	return fmt.Sprintf(`# Written by %[1]s %[5]s. Changes are overwritten by "%[1]s update".
[Unit]
Description=%[1]s WireGuard manager (web interface and API)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=%[2]s
Group=%[2]s
WorkingDirectory=%[3]s
ExecStart=%[4]s -config %[3]s/config.json
ExecReload=/bin/kill -HUP $MAINPID
# Exit code 0 is used by "Restart now" in the web interface.
Restart=always
RestartSec=2

# Runs unprivileged: CAP_NET_ADMIN for netlink, wgctrl and nftables,
# CAP_NET_BIND_SERVICE for ports 443 and 80.
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE
NoNewPrivileges=yes

ProtectSystem=strict
ReadWritePaths=%[3]s
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelModules=yes
ProtectKernelTunables=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_NETLINK AF_UNIX
RestrictNamespaces=yes
RestrictRealtime=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
UMask=0077

[Install]
WantedBy=multi-user.target
`, appName, serviceName, installDir, installBin, unitVersion)
}

// unitVersion changes only when the unit text changes, so "update" does not
// rewrite the unit for every release.
const unitVersion = "unit-1"

const sysctlConf = "net.ipv4.ip_forward=1\nnet.ipv6.conf.all.forwarding=1\n"

// writeSystemFiles writes the unit, sysctl and module files. It reports
// whether the unit changed (systemd must then reload).
func writeSystemFiles() (unitChanged bool, err error) {
	if unitChanged, err = writeIfChanged(unitPath, unitFile(), 0o644); err != nil {
		return false, err
	}
	sysChanged, err := writeIfChanged(sysctlPath, sysctlConf, 0o644)
	if err != nil {
		return false, err
	}
	if sysChanged {
		step("Enabling IP forwarding (%s)", sysctlPath)
		if err := sh("sysctl", "-p", sysctlPath); err != nil {
			return false, err
		}
	}
	if _, err := writeIfChanged(modulesPath, "wireguard\n", 0o644); err != nil {
		return false, err
	}
	if err := sh("modprobe", "wireguard"); err != nil {
		fmt.Fprintln(os.Stderr, "  warning: could not load the wireguard kernel module:", err)
	}
	if unitChanged {
		step("Writing %s", unitPath)
		if err := sh("systemctl", "daemon-reload"); err != nil {
			return false, err
		}
	}
	return unitChanged, nil
}

// restartAndVerify restarts the service and checks that it stays up: the
// main process must still be the same a few seconds later (Restart=always
// would otherwise hide a crash loop).
func restartAndVerify() error {
	if err := sh("systemctl", "restart", serviceName); err != nil {
		return err
	}
	time.Sleep(1500 * time.Millisecond)
	pid1 := shOut("systemctl", "show", "-p", "MainPID", "--value", serviceName)
	time.Sleep(3 * time.Second)
	state := shOut("systemctl", "is-active", serviceName)
	pid2 := shOut("systemctl", "show", "-p", "MainPID", "--value", serviceName)
	if state != "active" || pid1 == "0" || pid1 == "" || pid1 != pid2 {
		return fmt.Errorf("the service did not stay running (state %s); see: journalctl -u %s -n 50", state, serviceName)
	}
	return nil
}

// ---------- install ----------

func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	domain := fs.String("domain", "", "domain for the web interface; enables Let's Encrypt")
	email := fs.String("email", "", "contact email for Let's Encrypt (optional)")
	endpoint := fs.String("endpoint", "", "host or IP clients connect to (default: the domain)")
	port := fs.Int("port", 0, "UDP port WireGuard listens on (default: 51820, or the current port when already installed)")
	yes := fs.Bool("y", false, "do not ask; use the flags and defaults")
	_ = fs.Parse(args)
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if err := requireRoot(); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}

	// Read the current settings without changing anything, so questions
	// and checks happen before the system is touched.
	_, statErr := os.Stat(configFile)
	existing := statErr == nil
	cur, err := loadConfigFile(configFile)
	if err != nil {
		return err
	}
	plan := installPlan{domain: *domain, email: *email, endpoint: *endpoint, port: *port}
	if err := plan.check(); err != nil {
		return err
	}
	if !existing {
		n, err := randomSubnet(24)
		if err != nil {
			return err
		}
		plan.ipv4 = n.String()
	}
	if !*yes && term.IsTerminal(int(os.Stdin.Fd())) {
		if plan, err = askInstall(os.Stdin, cur, existing, given, plan); err != nil {
			return err
		}
	} else if n := plan.reissueCount(cur, existing); n > 0 {
		fmt.Printf("Note: %d existing device(s) need a new config: the endpoint or port changes.\n", n)
	}

	// User and folder
	if _, err := user.Lookup(serviceName); err != nil {
		step("Creating system user %s", serviceName)
		shell := "/usr/sbin/nologin"
		for _, p := range []string{"/usr/sbin/nologin", "/sbin/nologin", "/bin/false"} {
			if _, err := os.Stat(p); err == nil {
				shell = p
				break
			}
		}
		if err := sh("useradd", "--system", "--home-dir", installDir, "--no-create-home", "--shell", shell, serviceName); err != nil {
			return err
		}
	}
	uid, gid, err := serviceUser()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(installDir, 0o750); err != nil {
		return err
	}
	if err := os.Chown(installDir, uid, gid); err != nil {
		return err
	}
	if err := os.Chmod(installDir, 0o750); err != nil {
		return err
	}

	// Binary
	if self != installBin {
		step("Installing %s %s to %s", appName, version, installBin)
		if err := copyFile(self, installBin, 0o755, 0, 0); err != nil {
			return err
		}
	}

	// Config: created with defaults (server key, the chosen subnet) if missing.
	if !existing {
		step("Creating %s", configFile)
		initial := fmt.Sprintf("{\"server\": {\"ipv4\": %q}}\n", plan.ipv4)
		if err := os.WriteFile(configFile, []byte(initial), 0o600); err != nil {
			return err
		}
	}
	if err := os.Chown(configFile, uid, gid); err != nil {
		return err
	}
	store, err := openStore(configFile)
	if err != nil {
		return err
	}
	if plan.changes() {
		step("Saving the settings")
		if err := store.Update(func(c *Config) error { plan.apply(c); return nil }); err != nil {
			return err
		}
	}

	if _, err := writeSystemFiles(); err != nil {
		return err
	}

	if store.Get().Admin.PasswordHash == "" {
		fmt.Println("\nChoose the admin password for the web interface (user \"admin\", at least 12 characters).")
		if err := setPassword(configFile); err != nil {
			return err
		}
	}

	// Everything in the folder belongs to the service user.
	if err := chownTree(installDir, uid, gid); err != nil {
		return err
	}

	step("Starting %s", serviceName)
	if err := sh("systemctl", "enable", serviceName); err != nil {
		return err
	}
	if err := restartAndVerify(); err != nil {
		return err
	}
	printWhereToGo(store.Get())
	return nil
}

func chownTree(root string, uid, gid int) error {
	return filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid)
	})
}

func printWhereToGo(c *Config) {
	host := c.Web.TLS.Domain
	if host == "" {
		host = c.Server.Endpoint
	}
	if host == "" {
		host = "<server address>"
	}
	scheme, port := "https", c.Web.Listen
	if c.Web.TLS.Mode == "off" {
		scheme = "http"
	}
	if _, p, _ := strings.Cut(port, ":"); p != "" && p != "443" && p != "80" {
		host += ":" + p
	}
	fmt.Printf("\nDone. %s %s is running.\n  Web interface: %s://%s/\n", appName, version, scheme, host)
	switch c.Web.TLS.Mode {
	case "acme":
		fmt.Println("  The Let's Encrypt certificate is requested on the first visit; ports 443 (and 80) must be reachable.")
	case "selfsigned":
		fmt.Println("  It uses a self-signed certificate, so the browser shows a warning the first time.")
	}
	fmt.Printf("  WireGuard: UDP %d · Log: %s/%s.jsonl · Status: systemctl status %s\n", c.Server.ListenPort, installDir, appName, serviceName)
}

// ---------- update ----------

func requireInstalled() error {
	if _, err := os.Stat(installBin); err != nil {
		return fmt.Errorf("%s is not installed; run: sudo ./%s install", appName, appName)
	}
	if _, err := os.Stat(unitPath); err != nil {
		return fmt.Errorf("%s is missing; run: sudo ./%s install", unitPath, appName)
	}
	return nil
}

func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	force := fs.Bool("force", false, "install even if it is the same build or an older version")
	_ = fs.Parse(args)
	if err := requireRoot(); err != nil {
		return err
	}
	if err := requireInstalled(); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self, _ = filepath.EvalSymlinks(self); self == installBin {
		return fmt.Errorf("run update from the new binary, e.g.: sudo /tmp/%s update", appName)
	}

	// 1. Is this an update?
	oldVersion := installedVersion(installBin)
	if fileHash(self) == fileHash(installBin) && !*force {
		fmt.Printf("Already up to date (%s %s).\n", appName, oldVersion)
		return nil
	}
	if c, ok := compareVersions(version, oldVersion); ok && c < 0 && !*force {
		return fmt.Errorf("this is %s, older than the installed %s; use -force to downgrade", version, oldVersion)
	}
	fmt.Printf("Updating %s %s → %s\n", appName, oldVersion, version)

	// 2. Can this version read the current config?
	if c, err := loadConfigFile(configFile); err != nil {
		return fmt.Errorf("this version cannot read %s, nothing changed: %w", configFile, err)
	} else if err := c.validate(); err != nil {
		return fmt.Errorf("this version rejects %s, nothing changed: %w", configFile, err)
	}

	// 3. Back up the config, and keep the old binary until the new one runs.
	uid, gid, err := serviceUser()
	if err != nil {
		return err
	}
	backup := configFile + ".bak-" + oldVersion
	step("Backing up config to %s", backup)
	if err := copyFile(configFile, backup, 0o600, uid, gid); err != nil {
		return err
	}
	if err := copyFile(installBin, oldBin, 0o755, uid, gid); err != nil {
		return err
	}
	defer os.Remove(oldBin)

	// 4. Install.
	step("Installing the new binary")
	if err := copyFile(self, installBin, 0o755, uid, gid); err != nil {
		return err
	}
	if _, err := writeSystemFiles(); err != nil {
		return err
	}

	// 5. Restart. If the new version does not stay up, put the old one back
	// so the web interface stays reachable.
	step("Restarting %s", serviceName)
	if err := restartAndVerify(); err != nil {
		fmt.Fprintln(os.Stderr, "  The new version failed to start:", err)
		step("Restoring %s %s", appName, oldVersion)
		if rErr := copyFile(oldBin, installBin, 0o755, uid, gid); rErr != nil {
			return fmt.Errorf("update failed and restoring the old binary failed too: %v (original error: %w)", rErr, err)
		}
		if rErr := restartAndVerify(); rErr != nil {
			return fmt.Errorf("update failed and the old version does not start either: %v (original error: %w)", rErr, err)
		}
		return fmt.Errorf("update failed, %s %s is running again: %w", appName, oldVersion, err)
	}
	fmt.Printf("\nUpdated %s %s → %s.\n", appName, oldVersion, version)
	return nil
}

// ---------- uninstall ----------

func cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	purge := fs.Bool("purge", false, "also delete "+installDir+" (config, keys, logs) and the user "+serviceName)
	yes := fs.Bool("y", false, "do not ask for confirmation")
	_ = fs.Parse(args)
	if err := requireRoot(); err != nil {
		return err
	}
	if *purge && !*yes {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return errors.New("-purge deletes all peers and keys; add -y to confirm")
		}
		fmt.Printf("This deletes %s with all peers, keys and logs. Type \"delete\" to continue: ", installDir)
		var answer string
		_, _ = fmt.Scanln(&answer)
		if answer != "delete" {
			return errors.New("cancelled")
		}
	}

	step("Stopping %s", serviceName)
	_ = sh("systemctl", "disable", "--now", serviceName)

	step("Removing the WireGuard interface and firewall table")
	c, err := loadConfigFile(configFile)
	if err != nil {
		c = &Config{}
		c.applyDefaults()
	}
	if k, err := newKernel(); err == nil {
		if err := k.Down(c); err != nil {
			fmt.Fprintln(os.Stderr, "  warning:", err)
		}
		k.Close()
	}

	step("Removing system files")
	for _, p := range []string{unitPath, sysctlPath, modulesPath} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	_ = sh("systemctl", "daemon-reload")

	if *purge {
		step("Deleting %s and the user %s", installDir, serviceName)
		if err := os.RemoveAll(installDir); err != nil {
			return err
		}
		_ = sh("userdel", serviceName)
		fmt.Println("\nRemoved everything.")
	} else {
		fmt.Printf("\nRemoved. Config, keys and logs are still in %s (uninstall -purge deletes them).\n", installDir)
	}
	fmt.Println("IP forwarding stays on until reboot; run 'sysctl -w net.ipv4.ip_forward=0' if nothing else needs it.")
	return nil
}

// ---------- passwd ----------

func cmdPasswd(args []string) error {
	fs := flag.NewFlagSet("passwd", flag.ExitOnError)
	path := fs.String("config", configFile, "config.json to change")
	_ = fs.Parse(args)
	if os.Geteuid() != 0 && runtime.GOOS == "linux" {
		return errors.New("run as root, e.g. with sudo")
	}
	if err := setPassword(*path); err != nil {
		return err
	}
	if *path == configFile && shOut("systemctl", "is-active", serviceName) == "active" {
		if err := sh("systemctl", "reload", serviceName); err != nil {
			return err
		}
		fmt.Println("The running service uses the new password now.")
	}
	return nil
}
