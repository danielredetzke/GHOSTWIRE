// Command GHOSTWIRE is a small WireGuard server manager with a web interface
// and a JSON API. It keeps its whole state in config.json, applies it to the
// kernel with netlink, wgctrl and nftables, and logs to <appName>.jsonl.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
)

// appName names the binary, the log file (GHOSTWIRE.jsonl), the session
// cookie and the nftables table. It is fixed on purpose.
const appName = "GHOSTWIRE"

// serviceName is the systemd unit, system user and folder under /opt.
const serviceName = "ghostwire"

var version = "0.1.0" // set with -ldflags "-X main.version=..."

func defaultConfigPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(filepath.Dir(exe), "config.json")
}

func main() {
	// Subcommands (install, update, ...) manage the installation; without
	// one, the binary runs the service.
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		if err := runCommand(os.Args[1], os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, appName+":", err)
			os.Exit(1)
		}
		return
	}

	flag.Usage = usage
	configPath := flag.String("config", defaultConfigPath(), "path to config.json; logs and stats are kept next to it")
	passwd := flag.Bool("passwd", false, "set a user's password and exit (username as argument; default: the first user)")
	down := flag.Bool("down", false, "remove the WireGuard interface and firewall rules and exit")
	check := flag.Bool("check", false, "check that config.json is valid for this version and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(appName, version)
		return
	}
	var err error
	switch {
	case *passwd:
		if err = setPassword(*configPath, flag.Arg(0)); err == nil {
			fmt.Fprintf(os.Stderr, "If the service is running: systemctl reload %s\n", serviceName)
		}
	case *down:
		err = teardown(*configPath)
	case *check:
		err = checkConfig(*configPath)
	default:
		err = run(*configPath)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, appName+":", err)
		os.Exit(1)
	}
}

// checkConfig loads and validates config.json without changing it.
func checkConfig(path string) error {
	c, err := loadConfigFile(path)
	if err != nil {
		return err
	}
	if err := c.validate(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	fmt.Println("config OK")
	return nil
}

func readSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return string(b), err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

// setPassword sets a user's password from the terminal; it is the way back
// in for someone locked out. An empty username means the first user.
func setPassword(path, username string) error {
	store, err := openStore(path)
	if err != nil {
		return err
	}
	cfg := store.Get()
	u := &cfg.Users[0]
	if username != "" {
		if u = cfg.userByName(username); u == nil {
			names := make([]string, len(cfg.Users))
			for i, x := range cfg.Users {
				names[i] = x.Username
			}
			return fmt.Errorf("no user %q; users: %s", username, strings.Join(names, ", "))
		}
	}
	pw, err := readSecret(fmt.Sprintf("New password for %q: ", u.Username))
	if err != nil {
		return err
	}
	if err := validatePassword(pw); err != nil {
		return err
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		again, err := readSecret("Repeat password: ")
		if err != nil {
			return err
		}
		if again != pw {
			return errors.New("passwords do not match")
		}
	}
	hash, err := hashPassword(pw)
	if err != nil {
		return err
	}
	id := u.ID
	if err := store.Update(func(c *Config) error {
		_, u := c.userByID(id)
		u.PasswordHash, u.MustChangePassword = hash, false
		return nil
	}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Password for %q saved.\n", u.Username)
	return nil
}

func teardown(path string) error {
	c, err := loadConfigFile(path)
	if err != nil {
		return err
	}
	k, err := newKernel()
	if err != nil {
		return err
	}
	defer k.Close()
	return k.Down(c)
}

func run(configPath string) error {
	configPath, _ = filepath.Abs(configPath)
	dataDir := filepath.Dir(configPath)

	store, err := openStore(configPath)
	if err != nil {
		return err
	}
	cfg := store.Get()
	logPath := filepath.Join(dataDir, appName+".jsonl")
	logw, err := setupLogging(logPath, cfg.Log)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer logw.Close()
	slog.Info("starting", "version", version, "config", configPath)

	// Every name this service looks up goes through the DNS setting.
	dns := newDNSRouter(cfg.DNS)
	net.DefaultResolver = dns.resolver()
	if !cfg.passwordSet() {
		slog.Warn("no password set; run: sudo " + installBin + " passwd")
		fmt.Fprintf(os.Stderr, "No password set. Run: sudo %s passwd\n", installBin)
	}

	kernel, err := newKernel()
	if err != nil {
		slog.Error("kernel access failed", "err", err)
		return err
	}
	defer kernel.Close()

	recon := newReconciler(kernel, store)
	_ = recon.ApplyNow() // errors are shown in the UI; the UI must stay reachable

	stats, err := openStats(filepath.Join(dataDir, "stats.json"), store, kernel)
	if err != nil {
		return fmt.Errorf("open stats: %w", err)
	}

	geo := newGeo(dataDir, cfg.Stats.geoEnabled())
	defer geo.Close()
	stats.geo = geo

	webTLS, err := setupTLS(cfg, dataDir)
	if err != nil {
		slog.Error("tls setup failed", "err", err)
		return fmt.Errorf("tls: %w", err)
	}

	stop := make(chan struct{})
	var stopOnce sync.Once
	shutdown := func() { stopOnce.Do(func() { close(stop) }) }

	speeds := newSpeeds(store, kernel)
	auth := newAuth(store)
	app := &App{
		store: store, kernel: kernel, recon: recon, stats: stats, speeds: speeds, auth: auth, tls: webTLS,
		logPath: logPath, logw: logw, geo: geo, updates: newUpdater(cfg.Updates), started: time.Now(), shutdown: shutdown,
		webAddrs: []string{cfg.Web.Listen}, dns: dns,
	}
	if cfg.Web.HTTPListen != "" && cfg.Web.TLS.Mode != "off" {
		app.webAddrs = append(app.webAddrs, cfg.Web.HTTPListen)
	}

	var wg sync.WaitGroup
	wg.Add(6)
	go func() { defer wg.Done(); recon.Run(stop) }()
	go func() { defer wg.Done(); stats.Run(stop) }()
	go func() { defer wg.Done(); speeds.Run(stop) }()
	go func() { defer wg.Done(); stats.RunPings(stop) }()
	go func() { defer wg.Done(); geo.Run(stop) }()
	go func() { defer wg.Done(); app.updates.Run(stop) }()
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				auth.sweep()
			}
		}
	}()

	srv := &http.Server{
		Addr:              cfg.Web.Listen,
		Handler:           app.routes(),
		TLSConfig:         webTLS.Config,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelDebug),
	}
	servers := []*http.Server{srv}
	errc := make(chan error, 2)

	ln, err := net.Listen("tcp", cfg.Web.Listen)
	if err != nil {
		slog.Error("listen failed", "addr", cfg.Web.Listen, "err", err)
		return fmt.Errorf("listen on %s: %w", cfg.Web.Listen, err)
	}
	go func() {
		if webTLS.Config != nil {
			errc <- srv.ServeTLS(ln, "", "")
		} else {
			errc <- srv.Serve(ln)
		}
	}()
	slog.Info("web interface listening", "addr", cfg.Web.Listen, "tls", cfg.Web.TLS.Mode)

	if cfg.Web.HTTPListen != "" && cfg.Web.TLS.Mode != "off" {
		var h http.Handler = http.HandlerFunc(redirectToHTTPS(cfg.Web.Listen))
		if webTLS.ACME != nil {
			h = webTLS.ACME.HTTPHandler(h) // also answers http-01 challenges
		}
		hs := &http.Server{Addr: cfg.Web.HTTPListen, Handler: h, ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, hs)
		go func() {
			if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("http listener failed", "addr", cfg.Web.HTTPListen, "err", err)
			}
		}()
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for running := true; running; {
		select {
		case s := <-sigs:
			if s == syscall.SIGHUP {
				if err := store.Reload(); err != nil {
					slog.Error("reload failed", "err", err)
				} else {
					app.applyRuntime(store.Get())
					recon.Kick()
					slog.Info("config reloaded")
				}
				continue
			}
			slog.Info("stopping", "signal", s.String())
			running = false
		case <-stop:
			running = false
		case err := <-errc:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("web server failed", "err", err)
				shutdown()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				for _, s := range servers {
					_ = s.Shutdown(ctx)
				}
				wg.Wait()
				return err
			}
		}
	}
	shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(ctx)
	}
	wg.Wait()
	slog.Info("stopped")
	return nil
}

func redirectToHTTPS(listen string) http.HandlerFunc {
	_, port, _ := net.SplitHostPort(listen)
	return func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if port != "" && port != "443" {
			host = net.JoinHostPort(host, port)
		}
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusMovedPermanently)
	}
}
