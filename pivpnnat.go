package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// pivpn masquerades its tunnel networks with iptables rules of its own,
// tagged with a comment, and saves them for boot: in iptables-persistent's
// rules.v4/v6, or with ufw in before.rules/before6.rules. After a takeover
// they would go on masquerading with NAT switched off here, so install
// deletes them from the kernel and comments them out in those files. When
// pivpn comes back (a failed takeover, uninstall), the commented lines are
// the record of what to put back: the files belong to root, unlike
// /opt/ghostwire, which the service can write.

const (
	pivpnNATTag = "wireguard-nat-rule"
	natOffMark  = "# switched off by GHOSTWIRE, uninstall puts it back: "
)

// pivpnNATFiles are the files that load pivpn's rules at boot, with the
// command that loads each rule into the kernel.
var pivpnNATFiles = []struct{ path, tool string }{
	{"/etc/iptables/rules.v4", "iptables"},
	{"/etc/iptables/rules.v6", "ip6tables"},
	{"/etc/ufw/before.rules", "iptables"},
	{"/etc/ufw/before6.rules", "ip6tables"},
}

// pivpnNATRules returns pivpn's rules in the output of "iptables -t nat -S"
// or in a rules file, as the words after -A/-I (chain and match). Lines with
// quotes are skipped: they would not split into words correctly, and
// pivpn's rules have none.
func pivpnNATRules(text string) [][]string {
	var out [][]string
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || (f[0] != "-A" && f[0] != "-I") || !strings.Contains(line, pivpnNATTag) || strings.Contains(line, `"`) {
			continue
		}
		out = append(out, f[1:])
	}
	return out
}

// switchNATLines comments pivpn's rules out (off) or back in (!off), and
// returns the new text and the lines it changed, as they read when on.
func switchNATLines(text string, off bool) (string, []string) {
	lines := strings.SplitAfter(text, "\n")
	var changed []string
	for i, l := range lines {
		switch {
		case off && strings.Contains(l, pivpnNATTag) && !strings.HasPrefix(strings.TrimSpace(l), "#"):
			lines[i] = natOffMark + l
			changed = append(changed, l)
		case !off && strings.HasPrefix(l, natOffMark):
			lines[i] = strings.TrimPrefix(l, natOffMark)
			changed = append(changed, lines[i])
		}
	}
	return strings.Join(lines, ""), changed
}

// switchNATFiles applies switchNATLines to every file that exists and
// returns the rules it changed, per command.
func switchNATFiles(off bool) (map[string][][]string, error) {
	changed := map[string][][]string{}
	for _, f := range pivpnNATFiles {
		b, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		text, lines := switchNATLines(string(b), off)
		if len(lines) == 0 {
			continue
		}
		fi, err := os.Stat(f.path)
		if err != nil {
			return changed, err
		}
		if _, err := writeIfChanged(f.path, text, fi.Mode().Perm()); err != nil {
			return changed, err
		}
		changed[f.tool] = append(changed[f.tool], pivpnNATRules(strings.Join(lines, ""))...)
	}
	return changed, nil
}

// removePivpnNAT deletes pivpn's NAT rules from the kernel and comments them
// out for the next boot. Errors are only warnings: GHOSTWIRE works with the
// rules in place, only switching NAT off has no effect then.
func removePivpnNAT() {
	if _, err := switchNATFiles(true); err != nil {
		fmt.Fprintln(os.Stderr, "  warning: pivpn's NAT rules:", err)
	}
	n := 0
	for _, tool := range []string{"iptables", "ip6tables"} {
		if _, err := exec.LookPath(tool); err != nil {
			continue
		}
		for _, r := range pivpnNATRules(shOut(tool, "-t", "nat", "-S", "POSTROUTING")) {
			if err := sh(tool, append([]string{"-t", "nat", "-D"}, r...)...); err != nil {
				fmt.Fprintln(os.Stderr, "  warning:", err)
				continue
			}
			n++
		}
	}
	if n > 0 {
		step("Removed pivpn's NAT rules (%d): the NAT setting in %s takes over", n, appName)
	}
}

// restorePivpnNAT puts back what removePivpnNAT took out: the lines in the
// files, and their rules into the kernel unless they are there already.
func restorePivpnNAT() {
	changed, err := switchNATFiles(false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "  warning: pivpn's NAT rules:", err)
	}
	n := 0
	for tool, rules := range changed {
		for _, r := range rules {
			if sh(tool, append([]string{"-t", "nat", "-C"}, r...)...) == nil {
				continue
			}
			if err := sh(tool, append([]string{"-t", "nat", "-A"}, r...)...); err != nil {
				fmt.Fprintln(os.Stderr, "  warning:", err)
				continue
			}
			n++
		}
	}
	if n > 0 {
		step("Put pivpn's NAT rules back (%d)", n)
	}
}
