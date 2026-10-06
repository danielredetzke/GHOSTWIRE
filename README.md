<p align="center">
  <img src="favicon.svg" width="120" height="120" alt="GHOSTWIRE logo: the Hannya mask">
</p>

# GHOSTWIRE

**ゴーストワイヤー** · A self-hosted WireGuard server manager in a single Go
binary, with a web interface, a JSON API and a native iPhone app.

GHOSTWIRE sets up the WireGuard server, manages peers (add, change, disable,
remove), hands out client configs as a download or QR code, and records traffic
and connection history per peer. There are no dependencies on the server:
the binary installs, updates and removes itself.

## Quick start

On a Linux server, run:

```sh
curl -fsSL https://ghostwi.re/install | sh
```

The short link leads to the script on Gitea. The same script can also be
fetched directly from Gitea or from the GitHub mirror:

```sh
curl -fsSL https://git.redetzke.aero/Redetzke/GHOSTWIRE/raw/branch/main/install.sh | sh
curl -fsSL https://raw.githubusercontent.com/danielredetzke/GHOSTWIRE/main/install.sh | sh
```

It downloads the latest release for the server's architecture, checks it
against `SHA256SUMS` and starts the [install](#install), which asks a few
questions and changes nothing until you confirm.

> **Coming from pivpn?** GHOSTWIRE takes over a pivpn WireGuard server with
> the [quick start](#quick-start) command above. Your phones and laptops keep their
> current configs and reconnect on their own, with nothing to re-scan or
> re-send. See [Moving from pivpn](#moving-from-pivpn).

![Dashboard with the protection check, peers online, traffic of the last 24 hours, the peer list and recent activity](screenshots/dashboard.png)

## Features

- **pivpn takeover:** install finds a pivpn WireGuard server and takes over
  its key, networks and every client with its keys and addresses, so devices
  keep working without new configs. pivpn comes back by itself if the switch
  fails. [Details](#moving-from-pivpn).
- **One file of state:** everything lives in `config.json`. The kernel is
  reconciled to it, so there is no `/etc/wireguard`, no `wg-quick` and no
  `wireguard-tools`.
- **Kernel access:** netlink creates `wg0` and sets its addresses and MTU;
  wgctrl sets keys and peers; nftables holds the rules in its own
  `inet GHOSTWIRE` table.
- **Live peer changes:** only peers that changed are touched, the same effect
  as `wg syncconf`, so connected peers stay connected.
- **IPv4 and IPv6:** IPv6 inside the tunnel is turned on automatically when the
  server has a global IPv6 address.
- **Traffic history:** kept in `stats.json`, hourly for 48 h and daily for
  400 days by default (Settings → Logs & history).
- **Protection check:** the dashboard shows the address websites see for your
  browser next to the server's. The same address means you are behind the
  VPN; a peer that only routes the VPN network counts as not protected.
- **Live view:** the speed of every peer right now, updated every 2 seconds,
  with the last 2 minutes as a chart. Kept in memory only.
- **Connection history:** every online session per peer, with start, duration,
  address and traffic. A new session starts when a device changes networks.
  Country and network operator come from the free
  [DB-IP Lite](https://db-ip.com) databases (CC BY 4.0). GHOSTWIRE downloads
  them monthly (about 20 MB) and looks addresses up locally, so peer addresses
  never leave the server. You can switch this off under Settings → Logs &
  history.
- **Logs:** written to `GHOSTWIRE.jsonl`, rotated at 10 MB with 5 old files
  kept by default, and shown on the Log page. Changes are marked as audit
  entries.
- **Update notice:** once a day the server asks Gitea or GitHub (your choice
  under Settings → Updates) for the latest release. A newer one shows in the
  sidebar, on the Dashboard and in Settings, with its release notes and the
  commands to update this server. Nothing about the server is sent; the check
  can be switched off.
- **HTTPS built in:** Let's Encrypt, a self-signed certificate, your own
  certificate files, or plain HTTP behind a reverse proxy.

## Security

- **Client private keys are never stored.** A config is shown once, as a
  download or QR code. "Issue new config" makes new keys.
- **Setup links:** instead of showing the QR code, you can send the device's
  owner a one-time link, valid for 1 hour, 24 hours or 7 days and protected by
  a 4-digit PIN by default. The keys are made only when the link is opened.
  The link works once, and 5 wrong PINs revoke it.
- **The service is not root.** It runs as user `ghostwire` with only
  `CAP_NET_ADMIN` and `CAP_NET_BIND_SERVICE`, and can write only to
  `/opt/ghostwire`.
- **Sign-in:** one or more users, all admins. Passwords are stored as argon2id hashes.
  After 5 failed attempts from one IP address, sign-in from it is locked for 15
  minutes; wrong two-step codes count too. Sessions use an
  HttpOnly, SameSite=Strict cookie and last 12 hours by default.
- **Two-step sign-in:** each user can add an authenticator app (TOTP) and
  passkeys under My account. A passkey signs in on its own, without username
  and password, and also works as the second step after a password. It can live
  on the device (Touch ID, Face ID, Windows Hello), in a password manager, or on
  a YubiKey with a PIN set. Turning it on gives 10 one-time recovery codes. An
  admin can require it for everyone (Settings → Sign-in) and reset it for a user
  who lost their phone or key. Passkeys use WebAuthn and need the server's
  domain name with a trusted certificate (Let's Encrypt, certificate files, or a
  reverse proxy); on a self-signed certificate or an IP address, only the
  authenticator app is offered. API tokens never need a second step.
- **API tokens** are stored only as hashes and can be read-only or full access.
- `config.json` holds the server private key and is readable only by the
  service (0600).

## Requirements

- Linux with kernel 5.6 or newer (WireGuard built in), nftables and systemd
- Ports: UDP 51820 (WireGuard; another port can be chosen at install), TCP 443
  (web), TCP 80 (optional, Let's Encrypt http-01 and redirect)

## Build

Building needs Go 1.27 or newer. The binaries are static (no cgo), so they run
on any Linux distribution.

```sh
make linux-amd64      # dist/amd64/GHOSTWIRE
make linux-arm64      # dist/arm64/GHOSTWIRE (Raspberry Pi 64-bit, ARM servers)
make linux-arm        # dist/armv7/GHOSTWIRE (Raspberry Pi OS 32-bit)
make test
```

## Install

The quickest way is the [one-line install](#quick-start). Arguments after
`sh -s --` are passed on to `install` and skip their questions:

```sh
curl -fsSL https://ghostwi.re/install | sh -s -- -domain vpn.example.net -email you@example.net
```

The binary installs itself, so you can also copy it to the server and run it
as root:

```sh
scp dist/amd64/GHOSTWIRE server:/tmp/
ssh server
sudo /tmp/GHOSTWIRE install
```

It asks a few questions, shows a summary and changes nothing until you
confirm:

```
Web interface
  Domain name for the web interface (empty: no domain, self-signed certificate)
  > vpn.example.net
  Email for Let's Encrypt expiry warnings (optional)
  > you@example.net

WireGuard
  Address devices connect to [vpn.example.net]
  >
  UDP port [51820]
  >

Admin account
  Password for "admin" (at least 12 characters): ************
  Repeat password: ************

Summary
  Web interface   https://vpn.example.net/ (Let's Encrypt, you@example.net)
  Endpoint        vpn.example.net:51820/udp
  Tunnel network  10.214.86.0/24 (random free range) · IPv6 on
  Firewall        443/tcp, 80/tcp, 51820/udp must be reachable

Install with these settings? [Y/n]
```

A domain turns on Let's Encrypt and is also the default WireGuard endpoint.
Without one, the web interface uses a self-signed certificate and the
endpoint defaults to the server's detected public IP.

### Unattended install

For scripts, cloud-init or Ansible, give the settings as flags. Questions
are skipped for every flag given, and entirely with `-y` or when there is no
terminal:

```sh
sudo /tmp/GHOSTWIRE install -y -domain vpn.example.net -email you@example.net -port 51820
```

| Flag | Default |
|---|---|
| `-domain` | none: self-signed certificate |
| `-email` | none |
| `-endpoint` | the domain |
| `-port` | 51820, or the current port when already installed |
| `-import-pivpn` | off: see [Moving from pivpn](#moving-from-pivpn) |
| `-no-wait` | off: after a pivpn takeover, don't wait for devices to reconnect |

The admin password is then read from standard input, e.g.
`echo "$PASSWORD" | sudo ./GHOSTWIRE install -y …`. Every value is checked
before anything is changed.

`install`:

1. creates the system user `ghostwire` and `/opt/ghostwire`
2. copies itself to `/opt/ghostwire/GHOSTWIRE`
3. creates `config.json` with defaults, if missing
4. writes `/etc/sysctl.d/99-ghostwire.conf` (IP forwarding) and
   `/etc/modules-load.d/ghostwire.conf`, and loads the kernel module
5. writes `/etc/systemd/system/ghostwire.service`
6. sets the admin password (first install only)
7. enables and starts the service, and checks that it stays up

Running it again is safe: steps that are already done are skipped, and the
questions offer the current settings, so Enter keeps them. If a changed
endpoint or port means existing devices need a new config, the summary says
how many.

Then open `https://vpn.example.net` and sign in as `admin`. Add more users
under Settings → Users. Root is needed only for the commands below, never for
the running service.

## Moving from pivpn

On a server that runs pivpn's WireGuard, a new install offers to take it
over. Devices keep their current config: GHOSTWIRE takes pivpn's server key,
port, MTU, tunnel networks (IPv4 and IPv6), endpoint, DNS, AllowedIPs and
keepalive, and every client with its public key, preshared key and addresses.
Clients pivpn switched off are imported switched off, with the note
"Imported from pivpn". Client private keys, which pivpn keeps in
`/etc/wireguard/configs`, are not read or stored.

After the summary, install notes which peers are connected, stops pivpn's
WireGuard (`systemctl disable --now wg-quick@wg0`), starts GHOSTWIRE on the
same `wg0` and waits up to 30 s for those peers to come back. Devices that
send traffic reconnect after about 15 s; an idle device reconnects the next
time it sends something. The wait only reports: Enter skips it, and so does
`-no-wait` in scripts. If the service does not stay running, install puts
pivpn back as it was.

Without a terminal, the takeover needs `-import-pivpn`; install refuses to
run next to pivpn otherwise. pivpn's files stay as they were. Manage peers in
GHOSTWIRE from then on, delete `/etc/wireguard/configs` once everything works,
and don't run `pivpn uninstall`, which removes WireGuard packages. To go back
to pivpn: `GHOSTWIRE uninstall`, then `systemctl enable --now wg-quick@wg0`.

## Commands (as root)

| Command | What it does |
|---|---|
| `GHOSTWIRE install [-domain d] [-email e] [-endpoint h] [-port p] [-import-pivpn] [-no-wait] [-y]` | Sets up and starts the service, as above. Asks for the settings no flag gave; `-y` never asks. On a pivpn server it takes over pivpn's WireGuard (see above). |
| `GHOSTWIRE update [-force]` | Run from the new binary, e.g. `sudo /tmp/GHOSTWIRE update`. Checks that it can read the current `config.json` (nothing changes if not), backs up the config to `config.json.bak-<old version>` (keeping the newest 3 such copies), replaces the binary, updates the unit if needed and restarts. If the new version does not stay up, the old binary and config are put back and restarted. It refuses older versions without `-force`. |
| `GHOSTWIRE uninstall [-purge] [-y]` | Stops and removes the service, `wg0` and the firewall table. `-purge` also deletes `/opt/ghostwire` and the user. |
| `GHOSTWIRE passwd [username]` | Sets a user's password (default: the first user) and reloads the running service. The way back in if you are locked out. |
| `GHOSTWIRE version` | Prints the version. |

Updating restarts only the management service. VPN connections stay up,
because `wg0` lives in the kernel.

## config.json

A minimal file is enough. Missing values are filled with defaults on first
start: server key, a random free /24 subnet, port 51820, MTU 1420, Quad9 DNS,
full tunnel.

```json
{
  "web": {
    "listen": ":443",
    "httpListen": ":80",
    "tls": { "mode": "acme", "domain": "vpn.example.net", "email": "admin@example.net" }
  },
  "server": { "endpoint": "vpn.example.net" }
}
```

`web.tls.mode` can be:

| Mode | What it does |
|---|---|
| `acme` | Let's Encrypt, automatic. Uses tls-alpn-01 on :443, or http-01 when `httpListen` is set. Certificates are cached in `/opt/ghostwire/acme`. `"staging": true` uses the test CA. |
| `selfsigned` | Generates a certificate in `/opt/ghostwire/tls`. The iOS app pins its fingerprint. |
| `files` | Uses `certFile` and `keyFile`, and reloads them when they change. |
| `off` | Plain HTTP, for running behind a reverse proxy on localhost. |

After editing `config.json` by hand, run `sudo systemctl reload ghostwire`.

## Files in /opt/ghostwire

| File | Content |
|---|---|
| `GHOSTWIRE` | the program |
| `config.json` | all settings, server key, peers, pending setup links with their PINs, user password hashes, authenticator app secrets, passkeys, recovery code and token hashes (0600) |
| `config.json.bak-*` | copies of `config.json` made by `update`; the newest 3 are kept, and Settings → Upkeep lists and removes them |
| `stats.json` | traffic and connection history per peer |
| `geo-country.mmdb`, `geo-asn.mmdb` | DB-IP Lite databases for country and network lookups |
| `GHOSTWIRE.jsonl` | log, one JSON object per line. Changes carry `"audit":true` |
| `acme/`, `tls/` | certificates |

## API

Base path `/api/v1`. The web interface signs in with a session cookie; every
user is an admin. Apps and scripts use `Authorization: Bearer <token>`; create
the token under Settings → Pair iOS app. A token belongs to the user who made
it and is revoked when that user is deleted. A read-only token may only use
GET. Full-access tokens can do everything the web interface does except the
endpoints marked "signed in": users, passwords, API tokens, the sign-in rules,
backup and restore.

For a user with two-step sign-in, `POST /auth/login` answers
`{"mfa": true, "ticket": "…", "methods": ["key", "totp", "recovery"]}`
instead of starting a session; the ticket is good for 5 minutes, and one of
the `/auth/login/…` steps turns it into the session. `PATCH /settings`
`{"signin": {"requireMfa": true}}` requires two-step sign-in for every user;
only a signed-in user can change it.

`POST /users` and `POST /users/{id}/reset-password` take
`{"password": "…", "mustChangePassword": true}`; with `true` (the default) the
user can do nothing but choose a new password at the next sign-in.

```
POST   /auth/login · /auth/logout        GET /auth/me
signed in: POST /auth/password (own password)
signed in: GET|POST /users · PATCH|DELETE /users/{id}
signed in: POST /users/{id}/reset-password · /users/{id}/reset-mfa
GET    /auth/options (public: is passkey sign-in offered here)
POST   /auth/login/totp · /auth/login/recovery {"ticket", "code"}
POST   /auth/login/key/begin {"ticket"} · /auth/login/key/finish?ticket=  (body: the WebAuthn credential)
POST   /auth/login/passkey/begin · /auth/login/passkey/finish?id=
signed in: GET /auth/mfa · POST /auth/mfa/totp/setup · /auth/mfa/totp/confirm · DELETE /auth/mfa/totp
signed in: POST /auth/mfa/keys/begin · /auth/mfa/keys/finish?name= · PATCH|DELETE /auth/mfa/keys/{id}
signed in: POST /auth/mfa/recovery-codes
GET    /status                           GET /stats?range=24h|7d|30d|90d
GET    /live?since=                      (speed per peer, last 2 minutes in 2-second steps)
GET    /live/stream                      (the same as server-sent events)
GET    /server         PATCH /server     POST /server/rotate-key     GET /server/detect-ip
GET    /peers          POST /peers       (returns the config and QR once)
GET    /peers/{id}     PATCH /peers/{id} DELETE /peers/{id}
POST   /peers/{id}/enable | /disable | /issue-config
GET    /peers/{id}/stats?range=…        GET /peers/{id}/sessions?limit=100
GET    /peers/{id}/latency               (24 h, one point per 5 minutes)
GET    /peers/{id}/setup (not read-only) DELETE /peers/{id}/setup
GET    /settings       PATCH /settings   POST /restart   POST /updates/check
GET    /logs?level=&limit=&audit=1       GET /logs/download
signed in: GET|POST /tokens · DELETE /tokens/{id} · GET /backup · POST /restore
signed in: GET|DELETE /update-backups · DELETE /update-backups/{name}   (config copies made by update)
public: GET /setup/{token} · POST /setup/{token} {"pin"}   (what a setup link opens)
```

`POST /peers` and `POST /peers/{id}/issue-config` take
`{"delivery": "link", "linkHours": 1|24|168, "linkPIN": true}` to answer with a
setup link (`setup.url`, `setup.pin`, `setup.qr`) instead of a config. With a
link, the peer's current keys keep working until the link is opened.

`GET /settings` includes `updates`: the running and latest version,
`available`, the release notes and the download links for this server's
platform. `PATCH /settings` `{"updates": {"source": "gitea"|"github",
"check": false}}` picks the source or switches the daily check off;
`POST /updates/check` checks now. `GET /auth/me` has `updateAvailable` with
the newer version while there is one.

Traffic is reported from the peer's point of view: `down` is what the peer
downloaded, `up` is what it uploaded.

`GET /live` answers `{"step": 2, "size": 60, "points": [{"t": …, "peers":
{"<id>": [down, up]}}]}` with speeds in bits per second, kept only in memory.
With `since` (unix seconds) it returns only newer steps. `GET /live/stream`
sends the same messages as server-sent events: the history first, then one
message per new step.

Latency is measured by pinging the peer's tunnel address every 30 seconds. Set
it per peer with `PATCH /peers/{id}` `{"latencyCheck": "off"|"active"|"always"}`
(default `off`). `active` pings only while the device sends traffic, so idle
phones are not woken up; `always` keeps the tunnel up, so the peer always shows
as online. The service opens an unprivileged ICMP socket, which needs its group
in the sysctl `net.ipv4.ping_group_range` (systemd allows all groups by
default). Devices that block ping, such as Windows with its default firewall,
show no reply.

## Firewall note

GHOSTWIRE's rules sit in their own nftables table. An accept there cannot
override a drop in another table, so if ufw or firewalld is active, allow UDP
51820 (and TCP 443/80) in that firewall too.

## iOS app

The native iPhone app (SwiftUI, iOS 17+) lives in its own project,
GHOSTWIRE-Companion. It manages peers, the server and the app settings and
shows stats and logs. Users, passwords, API tokens, two-step sign-in, backup
and restore stay in the web interface. Pair it in the web interface under
Settings → Pair iOS app: scan the QR code, or tap "Copy pairing code" and paste
it into the app's "Enter manually". Self-signed certificates are pinned during
pairing.

The iOS app is currently in beta testing. For an invite, email
[engineroom@redetzke.aero](mailto:engineroom@redetzke.aero).

## Development

On macOS (or any non-Linux system), `make dev` starts the app on
http://127.0.0.1:8080 with a traffic simulator in place of the kernel. Set a
password first:

```sh
make build && mkdir -p dev && ./GHOSTWIRE -config dev/config.json -passwd
```

## License

GHOSTWIRE is released under the [MIT License](LICENSE).

The wordmark typeface, Shippori Mincho B1 by the Shippori Mincho Project
Authors, is bundled as a subset under the
[SIL Open Font License 1.1](OFL-ShipporiMincho.txt).

The DB-IP Lite databases it downloads are by [DB-IP](https://db-ip.com) and
licensed under [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/).
