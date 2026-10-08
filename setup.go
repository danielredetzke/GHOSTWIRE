package main

import (
	"bufio"
	"bytes"
	"cmp"
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
	"slices"
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
  %s install [-domain vpn.example.net] [-email you@example.net] [-endpoint host] [-port 51820] [-import-pivpn] [-no-wait] [-y]
        set up user, folder, config, sysctls and systemd service; start it.
        In a terminal it asks for the settings no flag gave; -y never asks.
        On a pivpn server it offers to take over pivpn's WireGuard and clients
  %s update [-force]
        replace the installed binary with this one and restart
  %s uninstall [-purge] [-y]
        remove the service, interface and firewall table (-purge also deletes %s)
  %s passwd [username]
        set a user's password (default: the first user) of the installed service
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

// sysctlConf turns on forwarding. With IPv6 forwarding on, Linux ignores
// router announcements unless accept_ra is 2, and a server that gets its
// IPv6 route from them (SLAAC, e.g. a Raspberry Pi at home) loses IPv6 when
// the route expires. So every interface in ras keeps accepting them, as
// pivpn does for its uplink.
func sysctlConf(ras []string) string {
	var b strings.Builder
	b.WriteString("net.ipv4.ip_forward=1\nnet.ipv6.conf.all.forwarding=1\nnet.ipv6.conf.default.accept_ra=2\n")
	for _, name := range ras {
		fmt.Fprintf(&b, "net.ipv6.conf.%s.accept_ra=2\n", name)
	}
	return b.String()
}

// raInterfaces returns the network cards and the interface of the IPv6
// default route, except those where router announcements are switched off
// (accept_ra 0). The directories are /proc/sys/net/ipv6/conf and
// /sys/class/net, routes is /proc/net/ipv6_route.
func raInterfaces(confDir, netDir, routes string) []string {
	want := map[string]bool{}
	if b, err := os.ReadFile(routes); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) == 10 && f[0] == strings.Repeat("0", 32) && f[1] == "00" && f[9] != "lo" {
				want[f[9]] = true
			}
		}
	}
	entries, _ := os.ReadDir(netDir)
	for _, e := range entries {
		// Only real devices: bridges, veth and tunnels come and go.
		if _, err := os.Stat(filepath.Join(netDir, e.Name(), "device")); err == nil {
			want[e.Name()] = true
		}
	}
	var out []string
	for name := range want {
		v := readSysctl(filepath.Join(confDir, name, "accept_ra"))
		if v == "1" || v == "2" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// writeSystemFiles writes the unit, sysctl and module files. It reports
// whether the unit changed (systemd must then reload).
func writeSystemFiles() (unitChanged bool, err error) {
	if unitChanged, err = writeIfChanged(unitPath, unitFile(), 0o644); err != nil {
		return false, err
	}
	ras := raInterfaces("/proc/sys/net/ipv6/conf", "/sys/class/net", "/proc/net/ipv6_route")
	sysChanged, err := writeIfChanged(sysctlPath, sysctlConf(ras), 0o644)
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

func cmdInstall(args []string) (err error) {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	domain := fs.String("domain", "", "domain for the web interface; enables Let's Encrypt")
	email := fs.String("email", "", "contact email for Let's Encrypt (optional)")
	endpoint := fs.String("endpoint", "", "host or IP clients connect to (default: the domain)")
	port := fs.Int("port", 0, "UDP port WireGuard listens on (default: 51820, or the current port when already installed)")
	yes := fs.Bool("y", false, "do not ask; use the flags and defaults")
	importPivpn := fs.Bool("import-pivpn", false, "take over pivpn's WireGuard server and clients (new installs only)")
	noWait := fs.Bool("no-wait", false, "after a pivpn takeover, do not wait for connected devices to come back")
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
	interactive := !*yes && term.IsTerminal(int(os.Stdin.Fd()))

	// pivpn: a new install takes over its WireGuard server, or stops, since
	// both would run the same interface. An existing install stops while
	// pivpn's WireGuard is switched on again.
	var pv *pivpnSetup
	var pivpnUnit string // pivpn's wg-quick unit, when it runs or starts at boot
	pivpnIf := pivpnDev("/")
	if !existing {
		if pv, err = readPivpn("/"); err != nil {
			return err
		}
	} else if pivpnIf != "" {
		u := "wg-quick@" + pivpnIf
		if shOut("systemctl", "is-enabled", u) == "enabled" || shOut("systemctl", "is-active", u) == "active" {
			pivpnUnit = u
		}
	}
	if err := checkPivpn(existing, *importPivpn, interactive, pv, pivpnUnit); err != nil {
		return err
	}
	if pv != nil {
		pv.apply(cur)
		if err := cur.validate(); err != nil {
			return fmt.Errorf("pivpn's setup cannot be taken over, nothing changed: %w", err)
		}
		plan.pivpn = pv
	}

	if !existing && pv == nil {
		n, err := randomSubnet(24)
		if err != nil {
			return err
		}
		plan.ipv4 = n.String()
	}
	if interactive {
		if plan, err = askInstall(os.Stdin, cur, existing, given, plan); err != nil {
			return err
		}
	} else {
		if n := plan.reissueCount(cur, existing); n > 0 {
			fmt.Printf("Note: %d existing device(s) need a new config: the endpoint or port changes.\n", n)
		}
		if !cur.passwordSet() {
			if plan.passwordHash, err = stdinPassword(cur.Users[0].Username); err != nil {
				return err
			}
		}
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

	// A takeover that fails before pivpn is stopped takes its config and
	// unit back out, so the next install offers the takeover again.
	var switched time.Time
	if pv != nil {
		defer func() {
			if err != nil && switched.IsZero() {
				_ = os.Remove(configFile)
				if os.Remove(unitPath) == nil {
					_ = sh("systemctl", "daemon-reload")
				}
			}
		}()
	}

	// Config: created with defaults (server key, the chosen subnet) if
	// missing, or with everything taken over from pivpn.
	switch {
	case pv != nil:
		step("Creating %s from pivpn (%s)", configFile, plural(len(pv.Peers), "peer"))
		if err := writeFileAtomic(configFile, cur, 0o600); err != nil {
			return err
		}
	case !existing:
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

	// Everything in the folder belongs to the service user.
	if err := chownTree(installDir, uid, gid); err != nil {
		return err
	}

	// pivpn hands over its interface: note who is connected, then stop it.
	var connected []string
	if pv != nil {
		connected = pivpnConnected(pv)
		step("Peers connected to pivpn right now: %s", cmp.Or(strings.Join(connected, ", "), "none"))
		step("Stopping pivpn's WireGuard (systemctl disable --now wg-quick@%s)", pv.Dev)
		if err := sh("systemctl", "disable", "--now", "wg-quick@"+pv.Dev); err != nil {
			return err
		}
		switched = time.Now()
	}
	if pivpnIf != "" {
		removePivpnNAT()
	}

	step("Starting %s", serviceName)
	err = sh("systemctl", "enable", serviceName)
	if err == nil {
		err = restartAndVerify()
	}
	if err != nil {
		if pv != nil {
			fmt.Fprintln(os.Stderr, " ", err)
			return pivpnBack(pv, store.Get(), err)
		}
		return err
	}
	if pv != nil {
		waitForPeers(pv, connected, switched, *noWait, interactive)
	}
	printWhereToGo(store.Get())
	if pv != nil {
		fmt.Printf("\npivpn is still installed but no longer runs %s. Manage the peers here from now on.\n", pv.Dev)
		fmt.Printf("Its files in /etc/wireguard and /etc/pivpn are untouched, including the client\n")
		fmt.Printf("configs with private keys. Once everything works, delete %s.\n", pv.ClientKeys)
		fmt.Printf("Don't run \"pivpn uninstall\": it removes WireGuard packages and firewall rules.\n")
	}
	return nil
}

// checkPivpn decides whether install may go on with pivpn on the server. pv
// is pivpn's setup, read on a new install only; pivpnUnit is pivpn's
// wg-quick unit when an existing install finds it running or enabled.
func checkPivpn(existing, importPivpn, interactive bool, pv *pivpnSetup, pivpnUnit string) error {
	switch {
	case existing && pivpnUnit != "":
		return fmt.Errorf("pivpn's WireGuard (%[2]s) is switched on, and %[1]s would run the same interface. "+
			"To keep %[1]s and its peers: systemctl disable --now %[2]s, then install again. "+
			"To take pivpn over again instead: %[1]s uninstall -purge, then install -import-pivpn", appName, pivpnUnit)
	case importPivpn && existing:
		return fmt.Errorf("-import-pivpn works only on a new install, and %s exists", configFile)
	case importPivpn && pv == nil:
		return fmt.Errorf("-import-pivpn: pivpn's WireGuard setup was not found (/%s)", pivpnSetupVars)
	case pv != nil && !interactive && !importPivpn:
		return fmt.Errorf("pivpn runs WireGuard on %s here; add -import-pivpn to take it over, or remove pivpn first", pv.Dev)
	}
	return nil
}

// stdinPassword reads the admin password of an install without questions
// from standard input (or the terminal with -y). It runs before anything is
// changed.
func stdinPassword(username string) (string, error) {
	pw, err := readSecret(fmt.Sprintf("Password for %q (at least 12 characters): ", username))
	if errors.Is(err, io.EOF) && pw != "" {
		err = nil // the last line without a newline
	}
	if err != nil || pw == "" {
		return "", fmt.Errorf("no admin password: pass it on standard input, e.g. echo \"$PASSWORD\" | %s install -y; nothing was changed", appName)
	}
	if err := validatePassword(pw); err != nil {
		return "", fmt.Errorf("admin password: %w; nothing was changed", err)
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		if again, err := readSecret("Repeat password: "); err != nil || again != pw {
			return "", errors.New("the passwords do not match; nothing was changed")
		}
	}
	return hashPassword(pw)
}

// pivpnConnected names the peers with a handshake in the last 3 minutes.
func pivpnConnected(pv *pivpnSetup) []string {
	k, err := newKernel()
	if err != nil {
		return nil
	}
	defer k.Close()
	samples, err := k.Sample(pv.Dev)
	if err != nil {
		return nil
	}
	var out []string
	for _, p := range pv.Peers {
		for _, s := range samples {
			if s.PublicKey == p.PublicKey && !s.LastHandshake.IsZero() && time.Since(s.LastHandshake) < onlineWindow {
				out = append(out, p.Name)
			}
		}
	}
	return out
}

// waitForPeers waits up to 30 s for the peers that were connected to pivpn
// to make a handshake with the new service. It only reports: a device that
// is idle may take minutes to send its next packet. Enter in a terminal
// skips the rest of the wait; -no-wait skips it entirely.
func waitForPeers(pv *pivpnSetup, names []string, since time.Time, noWait, interactive bool) {
	switch {
	case len(names) == 0:
		step("No peer was connected before the switch; devices connect when they come back online.")
		return
	case noWait:
		step("Not waiting for %s (-no-wait).", strings.Join(names, ", "))
		fmt.Printf("  %s\n", onlineLater(len(names)))
		return
	}
	k, err := newKernel()
	if err != nil {
		return
	}
	defer k.Close()
	var skip <-chan struct{}
	hint := ""
	if interactive {
		ch := make(chan struct{})
		go func() {
			_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
			close(ch)
		}()
		skip, hint = ch, " (Enter skips)"
	}
	step("Waiting up to 30 s for %s to come back%s", strings.Join(names, ", "), hint)
	back, skipped := waitBack(pv, names, func() ([]PeerSample, error) { return k.Sample(pv.Dev) }, since, 30*time.Second, 2*time.Second, skip)
	secs := int(time.Since(since).Round(time.Second).Seconds())
	var missing []string
	for _, n := range names {
		if !slices.Contains(back, n) {
			missing = append(missing, n)
		}
	}
	switch {
	case len(missing) == 0:
		step("%d of %d peers that were connected before are back (after %d s)", len(back), len(names), secs)
		return
	case skipped:
		step("Skipped after %d s: %d of %d back so far", secs, len(back), len(names))
		fmt.Printf("  %s not back yet.\n  %s\n", strings.Join(missing, ", "), onlineLater(len(missing)))
	default:
		step("%d of %d are back after %d s", len(back), len(names), secs)
		fmt.Printf("  %s not back yet. A device that is idle can take a few minutes to send its next\n", strings.Join(missing, ", "))
		fmt.Printf("  packet. %s\n", onlineLater(len(missing)))
	}
}

// waitBack polls the kernel until every named peer made a handshake after
// since, the timeout passes or skip is closed. It returns the peers that are
// back and whether the wait was skipped.
func waitBack(pv *pivpnSetup, names []string, sample func() ([]PeerSample, error), since time.Time, timeout, every time.Duration, skip <-chan struct{}) (back []string, skipped bool) {
	key := map[string]string{}
	for _, p := range pv.Peers {
		key[p.Name] = p.PublicKey
	}
	deadline := time.After(time.Until(since.Add(timeout)))
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		back = back[:0]
		if samples, err := sample(); err == nil {
			for _, n := range names {
				for _, s := range samples {
					if s.PublicKey == key[n] && s.LastHandshake.After(since) {
						back = append(back, n)
					}
				}
			}
		}
		if len(back) == len(names) {
			return back, false
		}
		select {
		case <-skip:
			return back, true
		case <-deadline:
			return back, false
		case <-tick.C:
		}
	}
}

// onlineLater tells where peers that are not back yet will show up.
func onlineLater(n int) string {
	if n == 1 {
		return "It shows as online on the Peers page once it is back."
	}
	return "They show as online on the Peers page once they are back."
}

// pivpnBack undoes the takeover after the service failed to start: it stops
// the service, removes its interface, firewall table and the config it was
// given, and starts pivpn's WireGuard again. Without the config, the next
// install offers the takeover again instead of fighting pivpn for wg0.
func pivpnBack(pv *pivpnSetup, c *Config, cause error) error {
	_ = sh("systemctl", "disable", "--now", serviceName)
	if k, err := newKernel(); err == nil {
		_ = k.Down(c)
		k.Close()
	}
	_ = os.Remove(configFile)
	restorePivpnNAT()
	step("Starting pivpn's WireGuard again (systemctl enable --now wg-quick@%s)", pv.Dev)
	if err := sh("systemctl", "enable", "--now", "wg-quick@"+pv.Dev); err != nil {
		return fmt.Errorf("install failed, and starting pivpn's WireGuard again failed too: %v (original error: %w)", err, cause)
	}
	return fmt.Errorf("install failed; pivpn runs %s as before: %w", pv.Dev, cause)
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
	backup := newUpdateBackupPath(configFile, oldVersion, time.Now())
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
		// The new version may have upgraded config.json to a format the old
		// one cannot read.
		if rErr := copyFile(backup, configFile, 0o600, uid, gid); rErr != nil {
			return fmt.Errorf("update failed and restoring %s failed too: %v (original error: %w)", configFile, rErr, err)
		}
		if rErr := restartAndVerify(); rErr != nil {
			return fmt.Errorf("update failed and the old version does not start either: %v (original error: %w)", rErr, err)
		}
		return fmt.Errorf("update failed, %s %s is running again: %w", appName, oldVersion, err)
	}
	if n, err := pruneUpdateBackups(configFile, keepUpdateBackups); err != nil {
		fmt.Fprintln(os.Stderr, "  Could not remove older config backups:", err)
	} else if n > 0 {
		step("Removed %d older config backups, kept the newest %d", n, keepUpdateBackups)
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
	if _, statErr := os.Stat(configFile); err != nil || statErr != nil {
		// Without a config of ours, e.g. after a pivpn takeover was undone,
		// the interface may belong to someone else: only the firewall table
		// goes.
		c = &Config{}
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
	if pivpnDev("/") != "" {
		restorePivpnNAT()
	}

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
	if err := setPassword(*path, fs.Arg(0)); err != nil {
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
