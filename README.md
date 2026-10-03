# GHOSTWIRE

A small WireGuard server manager: one Go binary with a web interface and a JSON
API (also meant for a future iOS app). It configures the WireGuard server,
manages peers (add, change, disable, remove) and records traffic per peer.

- **State:** everything lives in `config.json`. The kernel is reconciled to it,
  so there is no `/etc/wireguard`, no `wg-quick` and no `wireguard-tools`.
- **Kernel access:** netlink creates `wg0` and sets its addresses and MTU;
  wgctrl sets keys and peers; nftables holds the rules in its own
  `inet GHOSTWIRE` table.
- **Live peer changes:** only peers that changed are touched, the same effect
  as `wg syncconf`, so connected peers stay connected.
- **Logs:** written to `GHOSTWIRE.jsonl`, rotated at 10 MB with 5 old files kept by default (Settings → Data retention).
- **Traffic history:** kept in `stats.json`: hourly for 48 h and daily for 400 days by default (Settings → Data retention).
- **Connection history:** every online session per peer, with start, duration, address and traffic. A new session starts when a device changes networks. Country and network operator come from the free [DB-IP Lite](https://db-ip.com) databases (CC BY 4.0). GHOSTWIRE downloads them monthly (about 20 MB) and looks addresses up locally, so peer addresses never leave the server. You can switch this off under Settings → Data retention.
- **Client private keys are never stored.** A config is shown once, as a
  download or QR code. "Issue new config" makes new keys.

## Requirements

- Linux with kernel 5.6 or newer (WireGuard built in), nftables and systemd
- Ports: UDP 51820 (WireGuard), TCP 443 (web), TCP 80 (optional, Let's Encrypt
  http-01 and redirect)

## Build

```sh
make linux-amd64      # dist/amd64/GHOSTWIRE
make linux-arm64      # dist/arm64/GHOSTWIRE (Raspberry Pi 64-bit, ARM servers)
make linux-arm        # dist/armv7/GHOSTWIRE (Raspberry Pi OS 32-bit)
make test
```

## Install

The binary installs itself. Copy it to the server and run it as root:

```sh
scp dist/amd64/GHOSTWIRE server:/tmp/
ssh server
sudo /tmp/GHOSTWIRE install -domain vpn.example.net -email you@example.net
```

`-domain` turns on Let's Encrypt and is also used as the WireGuard endpoint.
Without it, the web interface uses a self-signed certificate; set the endpoint
later in the web interface or with `-endpoint`.

`install`:

1. creates the system user `ghostwire` and `/opt/ghostwire`
2. copies itself to `/opt/ghostwire/GHOSTWIRE`
3. creates `config.json` with defaults, if missing
4. writes `/etc/sysctl.d/99-ghostwire.conf` (IP forwarding) and
   `/etc/modules-load.d/ghostwire.conf`, and loads the kernel module
5. writes `/etc/systemd/system/ghostwire.service`
6. asks for the admin password (first install only)
7. enables and starts the service, and checks that it stays up

Running it again is safe: steps that are already done are skipped.

The service runs as user `ghostwire` with only `CAP_NET_ADMIN` and
`CAP_NET_BIND_SERVICE`, and can write only to `/opt/ghostwire`. Root is needed
only for the commands below, never for the running service.

## Commands (as root)

| Command | What it does |
|---|---|
| `GHOSTWIRE install [-domain d] [-email e] [-endpoint h]` | Sets up and starts the service, as above. |
| `GHOSTWIRE update [-force]` | Run from the new binary, e.g. `sudo /tmp/GHOSTWIRE update`. Checks that it can read the current `config.json` (nothing changes if not), backs up the config to `config.json.bak-<old version>`, replaces the binary, updates the unit if needed and restarts. If the new version does not stay up, the old binary is put back and restarted. It refuses older versions without `-force`. |
| `GHOSTWIRE uninstall [-purge] [-y]` | Stops and removes the service, `wg0` and the firewall table. `-purge` also deletes `/opt/ghostwire` and the user. |
| `GHOSTWIRE passwd` | Sets the admin password and reloads the running service. |
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
| `config.json` | all settings, server key, peers, token hashes (0600) |
| `stats.json` | traffic and connection history per peer |
| `geo-country.mmdb`, `geo-asn.mmdb` | DB-IP Lite databases for country and network lookups |
| `GHOSTWIRE.jsonl` | log, one JSON object per line. Changes carry `"audit":true` |
| `acme/`, `tls/` | certificates |

## API

Base path `/api/v1`. The web interface signs in with a session cookie. Apps and
scripts use `Authorization: Bearer <token>`; create the token under Settings →
Pair iOS app. A read-only token may only use GET. Full-access tokens can do
everything the web interface does except the admin-only endpoints: password,
API tokens, backup and restore, and changing the admin username.

```
POST   /auth/login · /auth/logout        GET /auth/me        POST /auth/password (admin)
GET    /status                           GET /stats?range=24h|7d|30d|90d
GET    /server         PATCH /server     POST /server/rotate-key     GET /server/detect-ip
GET    /peers          POST /peers       (returns the config and QR once)
GET    /peers/{id}     PATCH /peers/{id} DELETE /peers/{id}
POST   /peers/{id}/enable | /disable | /issue-config
GET    /peers/{id}/stats?range=…        GET /peers/{id}/sessions?limit=100
GET    /settings       PATCH /settings   POST /restart
GET    /logs?level=&limit=&audit=1       GET /logs/download
admin: GET|POST /tokens · DELETE /tokens/{id} · GET /backup · POST /restore
```

Traffic is reported from the peer's point of view: `down` is what the peer
downloaded, `up` is what it uploaded.

## Firewall note

GHOSTWIRE's rules sit in their own nftables table. An accept there cannot
override a drop in another table, so if ufw or firewalld is active, allow UDP
51820 (and TCP 443/80) in that firewall too.

## iOS app

`ios/` holds the native iPhone app (SwiftUI, iOS 17+). It does everything the
web interface does except password, API tokens and backups. Pair it in the web
interface under Settings → Pair iOS app: scan the QR code, or tap "Copy pairing
code" and paste it into the app's "Enter manually". Self-signed certificates are
pinned during pairing.

- Open `ios/GHOSTWIRE.xcodeproj` in Xcode to build and run.
- `TEAM_ID=<your team> ios/release.sh` archives and uploads a build to App Store
  Connect.
- `ios/AppStore/` has the store listing text, privacy details, review notes and
  6.9-inch screenshots.

## Development

On macOS (or any non-Linux system), `make dev` starts the app on
http://127.0.0.1:8080 with a traffic simulator in place of the kernel. Set a
password first:

```sh
make build && mkdir -p dev && ./GHOSTWIRE -config dev/config.json -passwd
```
