# GHOSTWIRE – App Store listing

Copy these into App Store Connect. Fields marked **[YOU]** need your input.

## App information

| Field | Value |
|---|---|
| Name | GHOSTWIRE |
| Subtitle (30 chars) | Manage your own WireGuard VPN |
| Bundle ID | aero.redetzke.ghostwire |
| SKU | ghostwire-ios |
| Primary category | Utilities |
| Secondary category | Developer Tools |
| Age rating | 4+ (answer "None" to every question) |
| Price | **[YOU]** (free suggested) |
| Support URL | **[YOU]** e.g. https://git.redetzke.aero/Redetzke/GHOSTWIRE |
| Privacy policy URL | **[YOU]** host the text from "Privacy policy" below |
| Copyright | **[YOU]** e.g. 2026 Daniel Redetzke |

## Promotional text (170 chars)

Your WireGuard® server in your pocket: see who is online, add devices with a QR code and watch traffic per device – all on your own server, nothing in between.

## Description

GHOSTWIRE is the companion app for the GHOSTWIRE server manager, a small program that sets up and runs a WireGuard® VPN server on your own Linux machine.

Pair the app once by scanning a QR code in the GHOSTWIRE web interface. From then on you can:

• See at a glance which devices are online and how much they transfer
• Add a device and show its config as a QR code to scan with the WireGuard app
• Issue a new config when a phone is replaced – the old one stops working
• Disable or delete devices instantly
• Follow traffic per device over 24 hours, 7 and 30 days
• Change the server's port, networks, DNS, routing and firewall options
• Check the server's health and read its log
• Set log and traffic history retention

Private by design:
• The app talks only to your server – there is no cloud service and no account.
• It signs in with a token you can revoke at any time.
• Self-signed certificates are pinned during pairing; Let's Encrypt certificates are checked normally.
• No analytics, no tracking, no data collection.

Requires a GHOSTWIRE server (Linux with kernel 5.6 or newer).

WireGuard is a registered trademark of Jason A. Donenfeld. GHOSTWIRE is not affiliated with or endorsed by the WireGuard project.

## Keywords (100 chars)

wireguard,vpn,server,admin,peers,qr,self-hosted,homelab,tunnel,network,raspberry pi

## What's new (1.0)

First release.

## App privacy (App Store Connect → App Privacy)

Data collection: **No, we do not collect data from this app.**

## Export compliance

The app only uses HTTPS (Apple's built-in TLS). `ITSAppUsesNonExemptEncryption` is set to NO in the build, so App Store Connect does not ask again.

## Privacy policy

> GHOSTWIRE (the iOS app) does not collect, store or share any personal data. The app connects only to the GHOSTWIRE server that you pair it with; the server address and access token are stored in the iOS keychain on your device. No data is sent to the developer or to third parties. Removing the app or tapping "Disconnect this iPhone" deletes the stored pairing.

## App Review information

Reviewers cannot use the app without a server. Provide a demo server:

1. Run GHOSTWIRE on a public test server with a Let's Encrypt certificate.
2. Add a few demo peers.
3. In the web interface: Settings → Pair iOS app → name "App Review", access "Full access" → **Copy pairing code**.

Notes for the reviewer (paste into "Notes"):

> GHOSTWIRE manages a self-hosted WireGuard VPN server. To review: open the app, tap "Enter manually", paste the pairing code below into "Pairing code" and tap Connect. You can then browse the dashboard, peers and server settings. Adding a peer shows a QR code for the WireGuard app; this demo server does not route real traffic.
>
> Pairing code: **[YOU: paste the pairing code]**

Sign-in required: **No** (pairing code instead of an account). Demo account fields: leave empty.

Revoke the "App Review" token after approval.

## Screenshots

`screenshots/` holds 6.9-inch iPhone screenshots (1320 × 2868), the size App Store Connect requires; it scales them down for smaller iPhones. Upload them in file-name order.
