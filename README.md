# irnoti-asterisk

A small program you install on an **Asterisk** server so it can work with the IRNoti VoIP platform. It does two things:

1. **Automatic setup.** It writes the Asterisk configuration the platform needs (`http.conf`, `ari.conf` and a dialplan file) in files of its own and in clearly marked blocks, takes a backup before every change, checks that Asterisk accepts the result, and rolls everything back if it does not.
2. **A tunnel to the platform.** It keeps an **outbound** HTTPS connection to the IRNoti API open. The platform sends ARI requests (originate a call, play a prompt, record, hang up …) through that connection, and the agent runs them on the local Asterisk and sends back the answers and the ARI events. No inbound port is opened, so it works behind NAT and firewalls.

The agent contains **no business logic**. Pricing, call-duration limits, routing, IVR, billing and anti-fraud rules all stay in the platform. The agent only configures Asterisk and relays allow-listed requests. See [docs/PROTOCOL.md](docs/PROTOCOL.md) for the exact protocol.

## Requirements

- Linux (amd64 or arm64) with `systemd` (use `--no-service` otherwise).
- Asterisk 18 or newer with the ARI modules (`res_ari*`, `res_stasis*`, `res_http_websocket`). Asterisk 20/22 LTS is recommended.
- Root access on the server (the agent edits `/etc/asterisk` and reloads Asterisk).
- Outbound HTTPS from the server to your IRNoti API.
- An agent token created by your platform administrator.

The binary is statically linked and has no runtime dependencies, so it also runs on old CentOS-based FreePBX/Issabel images.

## Install

1. In the IRNoti admin panel (`/admin/voip`, section "Asterisk agents") create a new agent. You get an install command; the token is shown **once**.
2. Run the command on the Asterisk server as root:

```bash
curl -fsSL https://raw.githubusercontent.com/irnoti/asterisk-agent/main/scripts/install.sh | sudo bash -s -- \
  --api https://api.example.com --token irna_xxxxxxxxxxxxxxxx
```

The script downloads the binary for your CPU from the GitHub release, verifies it against `SHA256SUMS`, installs it as `/usr/local/bin/irnoti-asterisk` and runs `irnoti-asterisk install` with your arguments.

Add `--dry-run` to see what would change without touching anything.

3. Point your trunk's **inbound** context at `irnoti-inbound`:
   - plain PJSIP: set `context=irnoti-inbound` on the trunk endpoint;
   - FreePBX/Issabel: use a Custom Destination `irnoti-inbound,${EXTEN},1`.

   Numbers that have an IRNoti route are handled by the platform. Numbers without a route continue in the context you named as the "fallback context" when the agent was created (or are hung up if you gave none).
4. In the panel the agent should show **Online** and **ARI connected**. Bind a VoIP number to the new server and configure its route.

### Install from a package (RPM / DEB)

Each release also contains `.rpm` and `.deb` packages for amd64 and arm64 (listed in `SHA256SUMS`). They install only the binary to `/usr/bin/irnoti-asterisk`; you then run `irnoti-asterisk install --api URL --token TOKEN` yourself, because the token is not known at package time.

```bash
sudo rpm -ivh irnoti-asterisk-<version>-1.x86_64.rpm      # CentOS / Rocky / Issabel / FreePBX
sudo dpkg -i  irnoti-asterisk_<version>_amd64.deb          # Debian / Ubuntu
sudo irnoti-asterisk install --api https://api.example.com --token irna_xxxxxxxxxxxxxxxx
```

Removing the package does not touch the Asterisk configuration; run `irnoti-asterisk uninstall` first if you want that undone.

## What the installer changes

| Where | What |
|---|---|
| `/etc/asterisk/irnoti/` | The agent's own directory: `ari_irnoti.conf` (the agent's ARI user, generated password, mode 0640) and `extensions_irnoti.conf` (the dialplan the platform generates). |
| `ari.conf` | One managed block with `#include "irnoti/ari_irnoti.conf"`. |
| `http.conf` | A managed block that enables the HTTP server on `127.0.0.1:8088` **only if no working HTTP server is configured yet**. An existing setup (for example FreePBX on another port) is left alone and its address is detected. |
| `extensions_custom.conf` (if present, as on FreePBX/Issabel) or `extensions.conf` | One managed block with `#include "irnoti/extensions_irnoti.conf"`. |
| `/etc/irnoti-asterisk/agent.json` | The agent's own settings: API URL, token, ARI password (mode 0600). |
| `/etc/systemd/system/irnoti-asterisk.service` | The service (skipped with `--no-service`). |
| `/var/lib/irnoti-asterisk/backups/<time>/` | Copies of every file the agent modified, plus a manifest of files it created. |

Managed blocks are wrapped in `; BEGIN irnoti-asterisk (<tag>)` / `; END irnoti-asterisk (<tag>)`. Nothing outside those blocks and the `irnoti/` directory is ever modified.

The generated dialplan defines three contexts: `irnoti-outbound` (click-to-call: the customer's phone was rung first, now it is bridged to the destination, with an optional talk-time cap and call recording), `irnoti-inbound` (calls arriving from the trunk, handed to the platform) and `irnoti-softphone` (calls placed from the browser softphone).

After writing, the agent runs `module reload http`, `module reload res_ari.so` and `dialplan reload`, then waits until ARI answers with its own credentials. If any of that fails, **every file is restored** and the files it created are removed.

## Commands

```
irnoti-asterisk install   --api URL --token TOKEN [--asterisk-dir DIR] [--extensions-file FILE]
                          [--config FILE] [--no-service] [--dry-run]
irnoti-asterisk run       [--config FILE]       # the daemon (started by systemd)
irnoti-asterisk status    [--config FILE]       # Asterisk/ARI and platform reachability
irnoti-asterisk rollback  [--config FILE]       # restore the files saved before the last change
irnoti-asterisk uninstall [--config FILE] [--purge]
irnoti-asterisk version
```

- `install` can be run again at any time: it keeps the ARI password and never duplicates a block.
- `--asterisk-dir` defaults to `/etc/asterisk`. `--extensions-file` overrides which dialplan file receives the include.
- `uninstall` removes the managed blocks and the `irnoti/` directory, reloads Asterisk and stops the service. Backups are kept. `--purge` also deletes `agent.json` (token and ARI password).
- The `IRNOTI_AGENT_TOKEN` environment variable can replace `--token`.

Service management: `systemctl status irnoti-asterisk`, logs with `journalctl -u irnoti-asterisk -f`.

## How it behaves at runtime

- **Startup:** the agent registers with the API (version, host name, Asterisk version), fetches the current dialplan bundle and applies it if it changed.
- **Polling:** it long-polls the API (up to 25 s per request). Each ARI request it receives is checked against the allow-list, executed on the local ARI with the agent's own credentials, and answered. Up to 32 requests run in parallel.
- **Events:** it keeps the ARI event WebSocket open for the `irnoti` Stasis application and forwards events to the API in small batches.
- **Heartbeat:** every 30 seconds it reports the number of active channels and whether ARI is reachable. If the platform announces a new configuration version, the agent downloads the bundle, writes only the files that changed, and reloads the dialplan (rolling back if the reload fails).
- **Network trouble:** polling and event delivery retry with exponential backoff (1 s up to 30 s). Up to 5,000 events are queued while the API is unreachable (the oldest are dropped first). Calls that were inside the Stasis application when the connection broke are ended by Asterisk; new calls fall back to the context configured as the fallback.

## Security model

The agent is installed on a machine the customer controls, so its code is visible to them and nothing relies on the code being secret.

- **The token is the only credential.** The platform stores only its SHA-256 hash. It is shown once, can be rotated or revoked at any time from the admin panel, and gives access to nothing except this one server's tunnel. It is stored on the PBX in `agent.json` with mode 0600.
- **The ARI password never leaves the PBX.** It is generated locally and added by the agent when it executes a request; the platform never sees or sends it.
- **ARI allow-list.** Only the requests the platform itself needs are forwarded: channels (originate, answer, play, record, snoop, continue, redirect, hold, ring, dtmf, hang up), bridges, playback control, recordings (stop, download, delete) and creation/removal of PJSIP `auth`, `aor` and `endpoint` objects. Everything else (event subscriptions, applications, modules, logging, other configuration) is refused with 403, as is any path containing `..`, `//`, an encoded slash or dot, or an `api_key` parameter.
- **Configuration is validated before it is written.** File names must match `^[a-z0-9][a-z0-9_.-]{0,63}\.conf$`; at most 10 files of 128 KiB each; every file must match its SHA-256; the agent refuses a bundle that tries to define the ARI user. The dialplan may only use a small set of applications (`NoOp Set ExecIf GotoIf Goto Playback MixMonitor Dial Hangup Stasis Answer Wait Ringing Progress Congestion Busy Return`); functions such as `SHELL`, `CURL`, `FILE`, `ENV`, `ODBC_*`, `DB` and the directives `#include`/`#exec` are rejected. Even a compromised platform therefore cannot run commands on your PBX.
- **Every change is reversible:** backups before each write, automatic rollback on failure, and a manual `rollback`.
- **Hardened service:** the unit uses `NoNewPrivileges`, `ProtectSystem=full`, `ProtectHome` and `PrivateTmp`, and may write only to `/etc/asterisk`, `/etc/irnoti-asterisk` and `/var/lib/irnoti-asterisk`. It runs as root only because it edits Asterisk's configuration and reloads it.

## Browser phone (PJSIP over WSS)

The platform's in-page phone (`/panel/voip/softphone`) lets a customer call and answer from the browser. The browser registers a PJSIP account over **WSS** (SIP over a secure WebSocket) with JsSIP, and the platform creates those accounts through ARI. The agent itself does not set any of this up, so the PBX needs a few extra things: an HTTPS listener for the WebSocket, a `transport-wss`, PJSIP configuration stored in a database (*realtime*), and ICE for the audio path. `scripts/pjsip-webrtc-setup.sh` does all of it in one go.

Skip this whole section if you do not use the browser phone: calls, IVR, campaigns and prompts work without it.

### What you need first

- Asterisk **16 or newer** with `res_pjsip` running. Check with `asterisk -rx "module show like pjsip"`.
- A domain and TLS in **one** of two places:
  - **Asterisk ends TLS** (`--cert`/`--key`): a certificate the browser trusts for the domain (full chain + private key, PEM). A self-signed or CDN "origin" certificate is not trusted when the browser connects directly.
  - **A CDN or reverse proxy ends TLS** (`--behind-proxy`): no certificate on the PBX. The browser connects to `wss://your-domain/ws`, the CDN opens it and passes a plain WebSocket to the PBX (see below).
- MySQL/MariaDB on the PBX (or reachable from it) and a MySQL/MariaDB **ODBC driver** registered (`odbcinst -q -d`).
- Nothing else to install for the tables: the script downloads a **prebuilt, checksum-pinned** SQL file for your Asterisk version from this repository (`scripts/pjsip-schema/`, raw.githubusercontent.com). Only if none exists for your version does it fall back to Asterisk's own migrations, which need `python3` with `venv`/`pip` and access to `downloads.asterisk.org`.
- On **Asterisk 16, `chan_sip` must not be loaded.** `chan_sip` claims the WebSocket protocol name `sip`, so PJSIP's WebSocket transport (`res_pjsip_transport_websocket`) refuses to start while it is loaded. The signature in `/var/log/asterisk/full` is `Registered SIP service WebSocket Transport Module` immediately followed by `Unregistered SIP service WebSocket Transport Module`. Disabling `chan_sip` stops every extension and trunk that uses it (typical on Issabel/Elastix), so do it on a test PBX, after moving them to PJSIP, or use a separate Asterisk for IRNoti. Asterisk 20+ no longer ships `chan_sip`.

### Run it

Always look first. `--check` and `--dry-run` change nothing:

```bash
curl -fsSLO https://raw.githubusercontent.com/irnoti/asterisk-agent/main/scripts/pjsip-webrtc-setup.sh
sudo bash pjsip-webrtc-setup.sh --check
sudo bash pjsip-webrtc-setup.sh --cert /root/fullchain.pem --key /root/privkey.pem --disable-chan-sip --dry-run
sudo bash pjsip-webrtc-setup.sh --cert /root/fullchain.pem --key /root/privkey.pem --disable-chan-sip --restart
```

| Option | Meaning |
|---|---|
| `--cert FILE`, `--key FILE` | Asterisk ends TLS: certificate chain and private key for the PBX domain. The script checks that the key belongs to the certificate. |
| `--tls-port N` | With `--cert`: HTTPS/WSS port (default `8089`). |
| `--behind-proxy` | TLS ends at a CDN/proxy. No certificate needed; excludes `--cert/--key`. Configures a plain `transport-ws`, checks that Asterisk's HTTP server is on `127.0.0.1` (ARI must stay private) and prints the proxy snippet. |
| `--proxy-port N` | With `--behind-proxy`: port the proxy listens on for the CDN (default `8080`). |
| `--disable-chan-sip` | Adds `noload => chan_sip.so` (see above). |
| `--db-name`, `--db-user`, `--db-pass` | Database and user for the PJSIP tables (default `irnoti_pjsip`; the password is generated when omitted). Only letters, digits, `_`, `.`, `-`. |
| `--mysql-root-pass` | MySQL root password. Otherwise it is read from `/etc/issabel.conf` if present, then a plain `root` login is tried. |
| `--skip-db` | Do not create the database or tables (they exist); only wire ODBC and the config files. |
| `--schema-sql FILE` | Create the tables from this SQL file instead of the prebuilt one (it may only contain table creation; anything else is refused because it runs as the database root). |
| `--no-prebuilt-schema` | Do not download the prebuilt file; run Asterisk's migrations on this PBX (needs `python3`). |
| `--ast-source DIR` | With the migrations: use this Asterisk source tree for `contrib/ast-db-manage` instead of downloading it. |
| `--stun HOST:PORT` | STUN server for ICE (`rtp.conf`). |
| `--restart` | Restart Asterisk at the end. `noload` and the realtime mapping only take effect after a restart (a reload is not enough). |
| `--check` | Report the state (Asterisk version, module states, transports, ODBC); change nothing. |
| `--dry-run` | Print every step; change nothing. |

### What it does, in order

| Step | Where | Change |
|---|---|---|
| 1. `chan_sip` | `modules.conf` | With `--disable-chan-sip`: `noload => chan_sip.so`. Without it, a warning if `chan_sip` is running. |
| 2. HTTPS *(with `--behind-proxy`: proxy check instead)* | `http.conf`, `keys/irnoti/` | `tlsenable`, `tlsbindaddr=0.0.0.0:<port>`, certificate and key (copied to `/etc/asterisk/keys/irnoti/`, key readable by Asterisk only). An existing `tlsenable` is left alone and reported. |
| 3. Transport | `pjsip_irnoti_wss.conf` (`..._ws.conf` behind a proxy), included from `pjsip_custom.conf` (or `pjsip.conf`) | `[transport-wss]` with `protocol=wss` (`[transport-ws]` with `protocol=ws` behind a proxy). |
| 4. ICE | `rtp.conf` | `icesupport=yes` (+ `stunaddr`). |
| 5. Database | MySQL | Creates the database (`utf8`) and a user limited to it, then builds the `ps_*` tables with **Asterisk's own alembic migrations** for the same version (so the columns match exactly what Asterisk reads and writes). Skipped if `ps_endpoints` already exists. |
| 6. ODBC | `/etc/odbc.ini`, `res_odbc.conf` | A data source `irnoti-pjsip`. |
| 7. Realtime | `sorcery.conf`, `extconfig.conf` | PJSIP `endpoint`/`auth`/`aor` come from **both** `pjsip.conf` (existing trunks keep working) and the database (accounts the platform creates). |

Anything already configured by hand is left untouched and listed under "Needs your attention" at the end.

### Table schema (prebuilt)

The `ps_*` tables must match what the running Asterisk reads and writes, so they come from Asterisk's own alembic migrations (`contrib/ast-db-manage`). Those are generated **in advance** (alembic's offline mode, no database involved) and kept in `scripts/pjsip-schema/`, one file per Asterisk version: `ps-schema-16.7.0.sql`, `16.30.1`, `18.26.4`, `20.21.0`, `22.11.0`. The setup script picks the file for your version (exact match, else the newest of your major, which is a superset of older releases), downloads it, checks it against the SHA-256 pinned inside the script, and refuses anything that is not table creation before loading it as the database root. The PBX needs no python and no copy of Asterisk's source.

- A version without a file (for example another major) falls back to running the migrations on the PBX (needs `python3`; the script prepares the tools **before** it changes any file).
- Add a version: `scripts/gen-pjsip-schema.sh 22.12.0` (needs git and python 3.8-3.11), then put the printed sha256 in `schema_hash()` and the version in `schema_latest()` of the setup script. `go test ./...` fails if a pin and its file disagree.
- Your own SQL (generated elsewhere, or edited): `--schema-sql FILE`. Mirror the files: set `IRNOTI_SCHEMA_BASE` to your URL.

### TLS ended by a CDN (`--behind-proxy`)

```
browser --wss://pbx.example.com/ws--> CDN (TLS ends here) --ws://PBX:8080/ws--> proxy on the PBX --> Asterisk 127.0.0.1:8088/ws
browser <==== audio: DTLS-SRTP over UDP, straight to the PBX's public IP (never through the CDN) ====> PBX
```

- Asterisk serves `/ari` and `/ws` from the **same** HTTP server, so never open it to the network. Keep `bindaddr=127.0.0.1` and put a small proxy in front that forwards **only** `/ws` (the script prints an nginx snippet and warns if Asterisk is bound elsewhere).
- Allow the proxy port only from the CDN's address ranges. Cloudflare reaches an origin over HTTP only on ports 80, 8080, 8880, 2052, 2082, 2086, 2095; WebSockets must be enabled for the zone.
- The audio does not go through the CDN: the PBX needs a public IP with the RTP range (`rtp.conf`) open, plus `external_media_address`/`external_signaling_address` on the transport if it is behind NAT.
- In the platform admin set `sipWsUrl` to `wss://pbx.example.com/ws` (no port) and `sipTransport` to `transport-ws`, because Asterisk sees a plain WebSocket.

### Safety

- Every change is a block between `;--- BEGIN irnoti-pjsip:<name> ---` and `;--- END irnoti-pjsip:<name> ---`. Running the script again replaces the blocks instead of duplicating them.
- Each file is copied to `/var/lib/irnoti-asterisk/pjsip-backup/<UTC time>/` before it is touched.
- Nothing is restarted without `--restart`, and nothing is downloaded or created in `--check`/`--dry-run`.
- FreePBX/Issabel regenerate `pjsip.conf` and similar files on *Apply Config*; the script writes to `*_custom.conf` where it exists. Run `--check` again after applying configuration.

### Undo

Delete the `irnoti-pjsip` blocks (or restore the files from the backup directory), remove the `noload` block to bring `chan_sip` back, and restart Asterisk. The database is separate and can be dropped (`DROP DATABASE irnoti_pjsip`).

### After it ran

1. Check: `asterisk -rx "module show like pjsip_transport_websocket"` is *Running*, `pjsip show transports` lists `transport-wss`, `odbc show` shows `irnoti-pjsip` connected.
2. Open the HTTPS/WSS port (TCP) and the RTP range from `rtp.conf` (UDP) in the firewall.
3. In the platform admin (`/admin/providers`), set `sipDomain` and `sipWsUrl` (`wss://your-domain:<port>/ws`) on this server.
4. In `/panel/voip/softphone` create an account and press *Connect*.

The full guide (diagnosis, certificates and CDNs, manual steps for each file, troubleshooting) is `docs/VOIP-PJSIP-WEBRTC.md` in the platform repository, `dadehnamair/irnoti`.

## Troubleshooting

| Symptom | What to check |
|---|---|
| `the API did not accept the token` during install | The token was copied incompletely, was rotated, or the agent was deleted. Create or rotate it in the admin panel. |
| Install ends with `Asterisk did not accept the configuration (rolled back)` | The message includes Asterisk's own output. Fix the reported problem (often a missing module or a syntax error in an existing file) and run `install` again. Your files were restored. |
| Install ends with `ARI did not come up ... (rolled back)` | The HTTP server or ARI modules are not loaded. Check `asterisk -rx "module show like ari"` and `asterisk -rx "http show status"`. |
| Panel shows the agent **Offline** | `systemctl status irnoti-asterisk`; check outbound HTTPS to the API and the log for `cannot reach the API` or `the token was refused`. |
| Agent online but **ARI disconnected** | Asterisk is down or the ARI user was changed. Run `irnoti-asterisk status`. |
| Calls to a number with a route are not answered by the platform | The trunk's inbound context is not `irnoti-inbound`. |
| A number without a route is hung up | Set a fallback context for the agent in the admin panel. |

## Uninstall

```bash
sudo irnoti-asterisk uninstall --purge
sudo rm /usr/local/bin/irnoti-asterisk
```

Then delete the agent in the admin panel to revoke its token.

## Development

Requires Go 1.24 or newer.

```bash
go vet ./... && go test ./... -race
make build        # static binaries for linux/amd64 and linux/arm64 in dist/, with SHA256SUMS
make packages     # also .rpm and .deb (needs nfpm); then scripts/test-packages.sh installs them in Rocky 8, CentOS 7 and Debian 12 containers (needs Docker)
```

Layout:

```
cmd/irnoti-asterisk/    CLI entry point
internal/api/           client for the platform protocol
internal/ari/           local ARI REST client and event stream
internal/proxy/         the ARI allow-list
internal/tunnel/        the daemon: polling, events, heartbeat, config sync
internal/installer/     config writer: managed blocks, backups, rollback, dialplan validation, systemd unit
internal/config/        the agent's own settings file
scripts/install.sh      the curl|bash installer
packaging/nfpm.yaml     RPM/DEB definition
```

The only third-party dependency is `github.com/coder/websocket` (for the ARI event stream).

Releases: push a tag `vX.Y.Z`; the CI workflow runs the tests, builds the static binaries and attaches them, the `.rpm`/`.deb` packages and `SHA256SUMS` to the GitHub release.
