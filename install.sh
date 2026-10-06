#!/bin/sh
# Downloads the latest GHOSTWIRE release, checks it against SHA256SUMS and
# runs its install command. Usage:
#
#   curl -fsSL https://ghostwi.re/install | sh
#   curl -fsSL https://ghostwi.re/install | sh -s -- -y -domain vpn.example.net -email you@example.net
#
# ghostwi.re/install redirects to this file on Gitea; the GitHub mirror has it
# at raw.githubusercontent.com/danielredetzke/GHOSTWIRE/main/install.sh.
#
# Arguments are passed on to "GHOSTWIRE install". Everything is wrapped in
# main so that a cut-off download runs nothing.
set -eu

GITEA=https://git.redetzke.aero/Redetzke/GHOSTWIRE
GITEA_API=https://git.redetzke.aero/api/v1/repos/Redetzke/GHOSTWIRE/releases/latest
GITHUB=https://github.com/danielredetzke/GHOSTWIRE
GITHUB_API=https://api.github.com/repos/danielredetzke/GHOSTWIRE/releases/latest

die() { echo "GHOSTWIRE: $*" >&2; exit 1; }

latest() { curl -fsSL "$1" 2>/dev/null | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p'; }

main() {
	[ "$(uname -s)" = Linux ] || die "runs on Linux only"
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	armv7l | armv8l) arch=armv7 ;;
	*) die "no build for $(uname -m)" ;;
	esac
	command -v curl >/dev/null || die "needs curl"
	command -v sha256sum >/dev/null || die "needs sha256sum"

	sudo=
	if [ "$(id -u)" -ne 0 ]; then
		command -v sudo >/dev/null || die "run as root or install sudo"
		sudo=sudo
	fi

	# Gitea first, GitHub if it is not reachable.
	repo=$GITEA
	version=$(latest "$GITEA_API")
	if [ -z "$version" ]; then
		repo=$GITHUB
		version=$(latest "$GITHUB_API")
	fi
	[ -n "$version" ] || die "could not find the latest release"

	file=GHOSTWIRE-$version-linux-$arch
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	echo "Downloading GHOSTWIRE $version ($arch) from $repo"
	curl -fsSL -o "$tmp/$file" "$repo/releases/download/$version/$file"
	curl -fsSL -o "$tmp/SHA256SUMS" "$repo/releases/download/$version/SHA256SUMS"
	(cd "$tmp" && grep " $file\$" SHA256SUMS | sha256sum -c --status) ||
		die "checksum of $file does not match SHA256SUMS"
	mv "$tmp/$file" "$tmp/GHOSTWIRE"
	chmod 755 "$tmp/GHOSTWIRE"

	# The script itself arrives on stdin, so the questions read the terminal.
	if [ -t 1 ] && [ -r /dev/tty ]; then
		$sudo "$tmp/GHOSTWIRE" install "$@" </dev/tty
	else
		$sudo "$tmp/GHOSTWIRE" install "$@"
	fi
}

main "$@"
