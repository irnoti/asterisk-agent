# Agent ↔ API protocol

The agent only makes **outbound HTTPS requests** to the platform API; no port is opened on the PBX. The platform puts ARI requests into a queue, the agent receives them by long-polling, runs them on its local ARI and posts the answers. ARI events travel the other way. All paths are under `/api/v1/agent`.

## Authentication

Every request carries `Authorization: Bearer irna_<token>`. The platform stores only the SHA-256 of the token; it is shown once when an agent is created or rotated, and can be revoked at any time. `401` means the token is wrong or revoked (the agent keeps retrying slowly until an operator fixes it).

The ARI credentials are **not** part of this protocol. The agent generates the ARI password on the PBX and adds it itself when it executes a request.

Each request also sends `User-Agent: irnoti-asterisk/<version>`.

## Endpoints

| Endpoint | Request body | Response |
|---|---|---|
| `POST /hello` | `{version, hostname, os, arch, asterisk}` | `{agentId, app, pollWaitSec, configVersion}` |
| `GET /config` | – | `{version, files: [{name, content, sha256}]}` |
| `POST /poll` | `{}` | `{requests: [{id, method, path, body?}]}`; empty after at most `pollWaitSec` (25) seconds |
| `POST /respond` | `{id, status, body, encoding, error?}` | `204` |
| `POST /events` | `{events: [<ARI event>, …]}`, at most 100 | `204` |
| `POST /heartbeat` | `{channels, ariUp, uptimeSec}` every 30 s | `{configVersion}` |

Details:

- `hello` is called at start-up and after every reconnect. `app` is the Stasis application name the agent subscribes to (default `irnoti`).
- `path` in a polled request is the ARI path **without** the `/ari` prefix and including the query string, for example `/channels?endpoint=PJSIP/1001&app=irnoti&appArgs=join,b1`. `body` is a JSON string or absent.
- In a response, `status` is the HTTP status ARI returned (or `403` if the request was refused by the allow-list, `502` if ARI could not be reached, with `error` set). `encoding` is `utf8`, or `base64` for binary bodies (recordings downloaded from `GET /recordings/stored/{name}/file`). A response body may be up to 25 MiB.
- Requests that were not answered by the agent within the platform's timeout (20 s, 70 s for downloads) fail on the platform side as a transient error, so the platform can try another server.
- `events` carries the raw ARI event objects. The agent batches up to 50 events or 20 ms, whichever comes first. If delivery fails it retries up to 8 times with backoff before dropping the batch.
- `heartbeat` reports the number of active channels and whether the agent's ARI event stream is connected. If the returned `configVersion` differs from the one last applied, the agent fetches `GET /config`.

## What can pass through the tunnel (ARI allow-list)

Only requests the platform itself makes are forwarded (see `internal/proxy/allowlist.go`):

| Area | Allowed |
|---|---|
| Server info | `GET /asterisk/info` |
| Channels | `GET/POST /channels`, `GET/DELETE /channels/{id}`, `POST /channels/{id}/{answer,play,record,snoop,continue,redirect,hold,ring,dtmf}`, `DELETE /channels/{id}/hold` |
| Bridges | `GET/POST /bridges`, `DELETE /bridges/{id}`, `POST /bridges/{id}/{addChannel,removeChannel,record}` |
| Playbacks | `DELETE /playbacks/{id}` |
| Recordings | `POST /recordings/live/{name}/stop`, `GET /recordings/stored/{name}/file`, `DELETE /recordings/stored/{name}` |
| Softphone accounts | `PUT/DELETE /asterisk/config/dynamic/res_pjsip/{auth,aor,endpoint}/{id}` |

Everything else (event subscriptions, applications, modules, logging, other configuration objects …) is answered with `403`. Paths containing `.` or `..` segments, `//`, backslashes, percent-encoded `/`, `.` or `\`, an absolute URL, or an `api_key` query parameter are refused as well.

## The configuration bundle

`GET /config` returns the dialplan the platform wants on the PBX, as a few small `.conf` files with a `version` (a hash of the content). The agent validates the bundle completely before writing anything:

- file names match `^[a-z0-9][a-z0-9_.-]{0,63}\.conf$`, no duplicates, at most 10 files, at most 128 KiB each;
- each file's content matches its `sha256`;
- the bundle must not contain `ari_irnoti.conf`: the ARI user is always written by the agent itself, so the platform can never set ARI credentials;
- the dialplan may use only these applications: `NoOp`, `Set`, `ExecIf`, `GotoIf`, `Goto`, `Playback`, `MixMonitor`, `Dial`, `Hangup`, `Stasis`, `Answer`, `Wait`, `Ringing`, `Progress`, `Congestion`, `Busy`, `Return`; the functions `SHELL`, `CURL`, `FILE`, `ENV`, `SYSTEM`, `EVAL`, `ARRAY`, `HASH`, `AST_CONFIG`, `DB`, `ODBC_*`, `AGENT_*`, `REALTIME*` and the directives `#include`/`#exec` are rejected (also inside the arguments of `ExecIf`).

A bundle that fails validation is not applied at all, and the agent keeps running with the previous configuration.

Files are written only inside `<asterisk-dir>/irnoti/`. Before any write the agent saves copies of the files it will modify under `/var/lib/irnoti-asterisk/backups/<UTC timestamp>/` with a manifest of the files it creates; if the following `dialplan reload` fails, the previous state is restored.

## Behaviour when the connection is lost

- ARI events are queued in memory (up to 5,000; the oldest are dropped first when the queue is full, because a stale key press or hang-up is useless later) and sent when the API is reachable again.
- Polling retries with exponential backoff from 1 s to 30 s.
- Channels that were inside the Stasis application when the connection broke are ended by Asterisk. New calls are not affected for long: the generated `irnoti-inbound` context continues in the configured fallback context if the call is not handled by the platform.
- A `401` from the API makes the agent wait (at least one minute between attempts) instead of retrying quickly.

## Platform side (for reference)

The platform maps each agent to a VoIP server (provider) whose configuration contains `agentId`. Its ARI client then uses the agent queue instead of an HTTP connection, so every feature (calls, campaigns, live control, recordings, softphone accounts, conferences) works unchanged for a PBX behind an agent. Requests, responses and events are exchanged between API instances through Redis (lists for requests and events, pub/sub for responses), or in memory in single-process mode.
