<p align="center">
  <img src="docs/assets/banner.jpg" alt="TigerSB VPN panel" width="100%">
</p>

<p align="center">
  <a href="LICENSE"><img alt="License GPL-3.0" src="https://img.shields.io/badge/license-GPL--3.0-FF6A00?style=for-the-badge&labelColor=1c1c1c"></a>
  <a href="https://github.com/Stilltiger45/TigerSB-panel/actions/workflows/ci.yml"><img alt="CI" src="https://img.shields.io/github/actions/workflow/status/Stilltiger45/TigerSB-panel/ci.yml?branch=main&style=for-the-badge&label=CI&labelColor=1c1c1c&color=FF6A00"></a>
  <img alt="Debian 11+ and Ubuntu 20.04+" src="https://img.shields.io/badge/Debian%20%7C%20Ubuntu-amd64%20%7C%20arm64-FF6A00?style=for-the-badge&logo=linux&logoColor=white&labelColor=1c1c1c">
</p>

<p align="center">
  <a href="https://github.com/XTLS/Xray-core"><img alt="Xray-core 26.9.30" src="https://img.shields.io/badge/Xray--core-26.9.30-FF8A1F?style=for-the-badge&labelColor=1c1c1c"></a>
  <a href="https://github.com/SagerNet/sing-box"><img alt="sing-box 1.14.4" src="https://img.shields.io/badge/sing--box-1.14.4-FF8A1F?style=for-the-badge&labelColor=1c1c1c"></a>
  <a href="https://caddyserver.com"><img alt="Caddy 2.11.4" src="https://img.shields.io/badge/Caddy-2.11.4-FF8A1F?style=for-the-badge&logo=caddy&logoColor=white&labelColor=1c1c1c"></a>
  <img alt="Docker" src="https://img.shields.io/badge/Docker-2%20cores-FF8A1F?style=for-the-badge&logo=docker&logoColor=white&labelColor=1c1c1c">
</p>

<p align="center">
  <img alt="Go 1.26" src="https://img.shields.io/badge/Go-1.26-FFB861?style=for-the-badge&logo=go&logoColor=white&labelColor=1c1c1c">
  <img alt="SQLite" src="https://img.shields.io/badge/SQLite-no%20cgo-FFB861?style=for-the-badge&logo=sqlite&logoColor=white&labelColor=1c1c1c">
  <img alt="systemd" src="https://img.shields.io/badge/systemd-services-FFB861?style=for-the-badge&labelColor=1c1c1c">
</p>

<p align="center">
  <a href="README.ru.md">🇷🇺 Русский</a> &nbsp;|&nbsp; 🇬🇧 <b>English</b>
</p>

---

**TigerSB-panel** is a self-hosted VPN panel on Xray and sing-box that installs with one command. Add a person, send them a key or a QR code, and they are connected. From the outside the server looks like an ordinary website under maintenance.

It is made for the [TigerSB-client](https://github.com/Stilltiger45/TigerSB-client) app, and its plain `https://` subscriptions work in v2rayNG, Hiddify, Happ, Streisand and other Xray and sing-box clients.

<p align="center">
  <img src="docs/assets/screens.jpg" alt="Panel tabs: Overview, Users, Control and Protocols" width="100%">
</p>

<sub>Screenshots from a test server. The panel interface is in Russian.</sub>

## Features

- **One-command install.** The installer sets up the panel, Caddy, Xray and sing-box, gets a Let's Encrypt certificate and prints the panel address. Running it again breaks nothing.
- **Hidden panel.** It opens only on a secret path of your domain; every other address shows a harmless "Maintenance" page (four templates: bakery, photo blog, garden diary, book club). `tigersb close` hides the secret path too, and only `tigersb open` over SSH brings it back.
- **Users in two clicks.** A name and a device count. Each user gets a `tigersb://` key and an `https://` subscription, both with a QR code. New users connect without restarting Xray, so nobody else is dropped.
- **Device limit.** Devices are counted by HWID when they fetch the subscription, and over the limit no subscription is given out. The list shows model, OS, app, IP and last update, and a button removes a device.
- **Three protocols in two containers.** Xray carries VLESS + REALITY over XHTTP and gRPC; port 443 is shared by the site and the protocols, and the server tells connections apart by SNI. sing-box carries TUIC v5 (QUIC over UDP 8443) with the site's real Let's Encrypt certificate. Each protocol card shows only its port; the rest folds away under "All settings", and every field is editable. The core checks the config before it is applied. A scanner finds SNI candidates for REALITY among the server's neighbours. A new protocol goes into the core that supports it.
- **Protected subscriptions.** The TigerSB profile is signed with Ed25519, so nobody on the way can swap the servers. A wrong token returns the same maintenance page with the same headers as any other page of the site. Subscriptions keep working while the panel is closed or asleep.
- **Server control.** CPU, memory, disk and network charts for 6 hours, a day or a week. Health checks for services, certificate, domain, disk space, clock and database, each with a hint on what to do.
- **One-button update.** A restore point is made first, and if the new version does not start, the panel rolls back by itself and says why.

Coming next: a cascade through a routing point in Russia (gRPC and TUIC), XHTTP through Yandex Cloud CDN.

## Components

| Component | Role | License |
|---|---|---|
| [Xray-core](https://github.com/XTLS/Xray-core) 26.9.30 | VPN core: VLESS + REALITY, XHTTP, gRPC. Official image in Docker, pinned by digest | MPL-2.0 |
| [sing-box](https://github.com/SagerNet/sing-box) 1.14.4 | second VPN core: TUIC v5. Official image in Docker, pinned by digest | GPL-3.0-or-later |
| [Caddy](https://caddyserver.com) 2.11.4 | maintenance site, Let's Encrypt certificate, panel and subscription routes | Apache-2.0 |
| [Docker](https://www.docker.com) | runs Xray and sing-box without privileges on a read-only file system | Apache-2.0 |
| [Go](https://go.dev) 1.26 | the panel, the port 443 splitter, the subscription service, metrics | BSD-3-Clause |
| [SQLite](https://sqlite.org) (modernc, no cgo) | users, devices, settings, charts | BSD-3-Clause |
| [pkg/profile](https://github.com/Stilltiger45/TigerSB-client/tree/main/pkg/profile) from TigerSB-client | TigerSB profile format and signature | GPL-3.0 |

## Install

You need a clean VPS with Debian 11+ or Ubuntu 20.04+ (amd64 or arm64), a domain with an A record pointing to it, and root access:

```bash
curl -fsSL https://raw.githubusercontent.com/Stilltiger45/TigerSB-panel/main/scripts/install.sh | sudo bash
```

The installer asks for:

1. **Server role.** The "main server" sends traffic to the internet and hosts the panel. The "routing point" is for the upcoming cascade.
2. The domain and an email for the certificate.
3. The admin login and password (at least 10 characters).

At the end it prints the panel address, `https://<domain>/<secret path>/`. Keep it; `tigersb info` on the server shows it again. Building from source takes a couple of minutes.

If your hosting has its own firewall, open ports 443 and 8443 for TCP and 8443 for UDP (TUIC) there.

## First steps

1. Open the panel address and log in.
2. On the **Пользователи** (Users) tab, add a person and click them: the TigerSB key and the https subscription appear with QR codes.
3. Send the key or show the QR code. In [TigerSB](https://github.com/Stilltiger45/TigerSB-client) the key is all it takes; in v2rayNG, Hiddify and others, add the https link as a subscription.
4. When you are done, press «Закрыть панель» (Close panel) on the Overview tab. To open it again, run `tigersb open` over SSH.

## Update and uninstall

The easiest way to update is the «Обновить панель» (Update panel) button on the Контроль (Control) tab, or `tigersb update` on the server. Settings, users and the database are kept, and the VPN keeps working during the update.

To remove everything from the server (asks for confirmation and offers to save a copy of the settings and database):

```bash
curl -fsSL https://raw.githubusercontent.com/Stilltiger45/TigerSB-panel/main/scripts/uninstall.sh | sudo bash
```

## Server commands

| Command | What it does |
|---|---|
| `tigersb info` | show the panel address and the fallback SSH tunnel command |
| `tigersb open` | open the panel at `https://<domain>/<secret path>/` |
| `tigersb close` | hide the panel: the secret path shows the maintenance page again |
| `tigersb site status` | domain, page template, Caddy and certificate state |
| `tigersb site set --domain <domain> [--email <email>]` | change the domain from the console if the panel is unreachable |
| `tigersb site apply` | rewrite the maintenance page and Caddy settings and apply them |
| `tigersb passwd --admin <login>` | change the password and end all sessions |
| `tigersb sleep` | put the panel to sleep: the process stops and the secret path shows the maintenance page. VPN and subscriptions keep working |
| `tigersb wake` | wake the panel |
| `tigersb status` | panel state |
| `tigersb update` | update from GitHub: restore point, update, rollback on failure |
| `tigersb restore list` | list restore points |
| `tigersb restore create` | create a restore point |
| `tigersb restore apply <name>` | roll back to a restore point |
| `tigersb xray status` | Xray and port 443 state, protocols and user count |
| `tigersb xray apply` | rebuild the Xray config from the database, test it and restart |
| `tigersb singbox status` | sing-box state and the TUIC port |
| `tigersb singbox apply` | rebuild the sing-box config and certificate copy, test them and restart |
| `tigersb monitor` | take metrics and run health checks now (a timer does this every 5 minutes) |

## Documentation

- [Panel guide](docs/guide.md) (in Russian): every tab, login and the SSH tunnel, subscriptions and the device limit, updates and restore points, login protection.
- [Architecture](docs/architecture.md) (in Russian): services, ports, database, how settings are applied.
- [Profile and key format](https://github.com/Stilltiger45/TigerSB-client/blob/main/docs/profile.md) in the client repository.
- [SECURITY.md](SECURITY.md): how to report a vulnerability.

## Development

```bash
go test ./...
CGO_ENABLED=0 go build -o tigersb ./cmd/tigersb
```

Installing from a clone builds the panel from the working copy:

```bash
git clone https://github.com/Stilltiger45/TigerSB-panel.git
cd TigerSB-panel
sudo ./scripts/install.sh            # or --binary ./tigersb-linux-amd64 with a ready binary
```

CI runs `gofmt`, `go vet`, the tests with `-race`, `shellcheck` on the scripts and a dependency license check. A `v*` tag builds a release with `tigersb-linux-amd64` and `tigersb-linux-arm64`.

## License

GPL-3.0, see [LICENSE](LICENSE). It matches the license of [TigerSB-client](https://github.com/Stilltiger45/TigerSB-client), so the panel can use its `pkg/profile` package. All Go dependencies are MIT or BSD and compatible with GPL-3.0. Third-party components, including Caddy (Apache-2.0), and borrowed ideas are listed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) and on the «О панели» (About) tab. `scripts/check-licenses.sh` enforces this in CI: it fails when a dependency has no license or an incompatible one (AGPL, SSPL, BSL and similar).
