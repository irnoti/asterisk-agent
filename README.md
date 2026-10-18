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
```

The only third-party dependency is `github.com/coder/websocket` (for the ARI event stream).

Releases: push a tag `vX.Y.Z`; the CI workflow runs the tests, builds the static binaries and attaches them and `SHA256SUMS` to the GitHub release.
