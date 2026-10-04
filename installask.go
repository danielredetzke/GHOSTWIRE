package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// installPlan is what install changes in config.json. It comes from the
// flags, and in a terminal from the answers to its questions.
type installPlan struct {
	domain, email, endpoint string
	port                    int
	noDomain                bool   // turn an existing domain off (self-signed certificate)
	noEmail                 bool   // remove an existing Let's Encrypt email
	passwordHash            string // asked in the terminal; empty: asked later
	ipv4                    string // tunnel network of a new install
}

var domainRe = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,63}$`)

func checkDomain(d string) error {
	if !domainRe.MatchString(d) {
		return fmt.Errorf("%q is not a domain name like vpn.example.net", d)
	}
	return nil
}

func checkEmail(e string) error {
	if at := strings.Index(e, "@"); at < 1 || at == len(e)-1 || strings.ContainsAny(e, " ,;") {
		return fmt.Errorf("%q is not an email address", e)
	}
	return nil
}

func checkEndpoint(h string) error {
	if net.ParseIP(h) != nil || domainRe.MatchString(h) {
		return nil
	}
	return fmt.Errorf("%q is not a host name or IP address (without port)", h)
}

func checkPort(p int) error {
	if p < 1 || p > 65535 {
		return errors.New("the port must be 1–65535")
	}
	return nil
}

// check validates the flags before anything is changed.
func (p installPlan) check() error {
	if p.domain != "" {
		if err := checkDomain(p.domain); err != nil {
			return err
		}
	}
	if p.email != "" {
		if err := checkEmail(p.email); err != nil {
			return err
		}
	}
	if p.endpoint != "" {
		if err := checkEndpoint(p.endpoint); err != nil {
			return err
		}
	}
	if p.port != 0 {
		return checkPort(p.port)
	}
	return nil
}

func (p installPlan) changes() bool {
	return p.domain != "" || p.email != "" || p.endpoint != "" || p.port != 0 || p.noDomain || p.noEmail || p.passwordHash != ""
}

func (p installPlan) apply(c *Config) {
	if p.noDomain {
		if c.Web.TLS.Mode == "acme" {
			c.Web.TLS.Mode = "selfsigned"
		}
		c.Web.TLS.Domain, c.Web.TLS.Email = "", ""
	}
	if p.domain != "" {
		c.Web.TLS.Mode, c.Web.TLS.Domain = "acme", p.domain
		if c.Server.Endpoint == "" {
			c.Server.Endpoint = p.domain
		}
	}
	if p.noEmail {
		c.Web.TLS.Email = ""
	}
	if p.email != "" {
		c.Web.TLS.Email = p.email
	}
	if p.endpoint != "" {
		c.Server.Endpoint = p.endpoint
	}
	if p.port != 0 {
		c.Server.ListenPort = p.port
	}
	if p.passwordHash != "" {
		c.Users[0].PasswordHash = p.passwordHash
	}
}

// reissueCount is the number of devices whose config stops working because
// the endpoint host or port changes.
func (p installPlan) reissueCount(cur *Config, existing bool) int {
	if !existing {
		return 0
	}
	next := cur.clone()
	p.apply(next)
	if endpointString(next) == endpointString(cur) {
		return 0
	}
	n := 0
	for i := range cur.Peers {
		if cur.Peers[i].hasKey() {
			n++
		}
	}
	return n
}

// --- questions ---

var errCancelled = errors.New("install cancelled; nothing was changed")

type prompter struct{ r *bufio.Reader }

// ask prints a question and returns the answer, def on Enter. check may
// reject an answer; the question is then asked again.
func (pr prompter) ask(question, def string, check func(string) error) (string, error) {
	for {
		if def != "" {
			fmt.Printf("  %s [%s]\n  > ", question, def)
		} else {
			fmt.Printf("  %s\n  > ", question)
		}
		line, err := pr.r.ReadString('\n')
		if err != nil && (err != io.EOF || line == "") {
			fmt.Println()
			return "", errCancelled
		}
		v := strings.TrimSpace(line)
		if v == "" {
			v = def
		}
		if check != nil {
			if err := check(v); err != nil {
				fmt.Printf("  ✗ %v\n", err)
				continue
			}
		}
		return v, nil
	}
}

func (pr prompter) confirm(question string) (bool, error) {
	fmt.Printf("%s [Y/n] ", question)
	line, err := pr.r.ReadString('\n')
	if err != nil && line == "" {
		fmt.Println()
		return false, errCancelled
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true, nil
	}
	return false, nil
}

// askInstall asks for every setting no flag gave, shows a summary and asks
// for confirmation. Nothing on the system has changed when it returns.
func askInstall(in io.Reader, cur *Config, existing bool, given map[string]bool, p installPlan) (installPlan, error) {
	pr := prompter{bufio.NewReader(in)}
	fmt.Printf("\n%s %s — WireGuard server manager\n", appName, version)
	if existing {
		fmt.Println("Already installed: the current settings are in [brackets]. Press Enter to keep them.")
	} else {
		fmt.Println("Press Enter to accept the value in [brackets].")
	}

	// Web interface
	fmt.Println("\nWeb interface")
	curDomain := ""
	if cur.Web.TLS.Mode == "acme" {
		curDomain = cur.Web.TLS.Domain
	}
	domain := p.domain
	if !given["domain"] {
		q := "Domain name for the web interface (empty: no domain, self-signed certificate)"
		if curDomain != "" {
			q = "Domain name for the web interface (none: no domain, self-signed certificate)"
		}
		v, err := pr.ask(q, curDomain, func(v string) error {
			if v == "" || v == "none" {
				return nil
			}
			return checkDomain(v)
		})
		if err != nil {
			return p, err
		}
		switch {
		case v == "none" || v == "":
			p.noDomain, domain = curDomain != "", ""
		case v != curDomain:
			p.domain, domain = v, v
		default:
			domain = v
		}
	}
	if domain == "" && cur.Web.TLS.Mode == "acme" && !p.noDomain {
		domain = curDomain
	}
	if domain != "" && !given["email"] {
		def := cur.Web.TLS.Email
		q := "Email for Let's Encrypt expiry warnings (optional)"
		if def != "" {
			q = "Email for Let's Encrypt expiry warnings (none: no email)"
		}
		v, err := pr.ask(q, def, func(v string) error {
			if v == "" || v == "none" {
				return nil
			}
			return checkEmail(v)
		})
		if err != nil {
			return p, err
		}
		switch {
		case v == "none":
			p.noEmail = true
		case v != cur.Web.TLS.Email:
			p.email = v
		}
	}

	// WireGuard
	fmt.Println("\nWireGuard")
	if !given["endpoint"] {
		def, hint := cur.Server.Endpoint, ""
		// An endpoint that followed the old domain follows the new one.
		if def == "" || (domain != "" && def == curDomain) {
			def = domain
		}
		if def == "" {
			if ip, err := detectPublicIP(context.Background()); err == nil {
				def, hint = ip.String(), " (detected public IP)"
			}
		}
		v, err := pr.ask("Address devices connect to"+hint, def, func(v string) error {
			if v == "" {
				return errors.New("devices need an address to connect to")
			}
			return checkEndpoint(v)
		})
		if err != nil {
			return p, err
		}
		if v != cur.Server.Endpoint {
			p.endpoint = v
		}
	}
	if !given["port"] {
		v, err := pr.ask("UDP port", strconv.Itoa(cur.Server.ListenPort), func(v string) error {
			n, err := strconv.Atoi(v)
			if err != nil {
				return errors.New("the port must be a number")
			}
			return checkPort(n)
		})
		if err != nil {
			return p, err
		}
		if n, _ := strconv.Atoi(v); n != cur.Server.ListenPort {
			p.port = n
		}
	}

	// First user, only when nobody has a password yet.
	if !cur.passwordSet() {
		fmt.Println("\nAdmin account")
		for {
			pw, err := readSecret(fmt.Sprintf("  Password for %q (at least 12 characters): ", cur.Users[0].Username))
			if err != nil {
				return p, errCancelled
			}
			if err := validatePassword(pw); err != nil {
				fmt.Printf("  ✗ %v\n", err)
				continue
			}
			again, err := readSecret("  Repeat password: ")
			if err != nil {
				return p, errCancelled
			}
			if again != pw {
				fmt.Println("  ✗ the passwords do not match")
				continue
			}
			if p.passwordHash, err = hashPassword(pw); err != nil {
				return p, err
			}
			break
		}
	}

	printInstallSummary(cur, existing, p)
	ok, err := pr.confirm("\nInstall with these settings?")
	if err != nil {
		return p, err
	}
	if !ok {
		return p, errCancelled
	}
	fmt.Println()
	return p, nil
}

func printInstallSummary(cur *Config, existing bool, p installPlan) {
	next := cur.clone()
	if !existing {
		next.Server.IPv4 = p.ipv4
		next.Server.IPv6Enabled = hasGlobalIPv6()
	}
	p.apply(next)
	if next.Server.Endpoint == "" {
		next.Server.Endpoint = next.Web.TLS.Domain
	}

	host := next.Web.TLS.Domain
	if host == "" {
		host = next.Server.Endpoint
	}
	_, webPort, _ := strings.Cut(next.Web.Listen, ":")
	if webPort != "" && webPort != "443" {
		host += ":" + webPort
	}
	web := "https://" + host + "/"
	switch next.Web.TLS.Mode {
	case "acme":
		web += " (Let's Encrypt"
		if next.Web.TLS.Email != "" {
			web += ", " + next.Web.TLS.Email
		}
		web += ")"
	case "selfsigned":
		web += " (self-signed certificate: the browser warns once)"
	case "files":
		web += " (your certificate files)"
	case "off":
		web = "http://" + host + "/ (plain HTTP behind a reverse proxy)"
	}

	ep := endpointString(next) + "/udp"
	if n := p.reissueCount(cur, existing); n > 0 {
		ep += fmt.Sprintf("   (was %s — %d existing device(s) need a new config)", endpointString(cur), n)
	}
	tunnel := next.Server.IPv4
	if !existing {
		tunnel += " (random free range)"
	}
	if next.Server.IPv6Enabled {
		tunnel += " · IPv6 on"
	} else {
		tunnel += " · IPv6 off (no public IPv6 address)"
	}

	var ports []string
	if next.Web.TLS.Mode != "off" {
		if webPort == "" {
			webPort = "443"
		}
		ports = append(ports, webPort+"/tcp")
		if next.Web.TLS.Mode == "acme" {
			ports = append(ports, "80/tcp")
		}
	}
	ports = append(ports, strconv.Itoa(next.Server.ListenPort)+"/udp")

	fmt.Println("\nSummary")
	fmt.Printf("  Web interface   %s\n", web)
	fmt.Printf("  Endpoint        %s\n", ep)
	fmt.Printf("  Tunnel network  %s\n", tunnel)
	fmt.Printf("  Firewall        %s must be reachable\n", strings.Join(ports, ", "))
	if p.passwordHash != "" {
		fmt.Printf("  Admin           %s (password set)\n", next.Users[0].Username)
	}
}
