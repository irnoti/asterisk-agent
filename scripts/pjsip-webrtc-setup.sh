#!/usr/bin/env bash
# pjsip-webrtc-setup.sh: prepares an Asterisk (16+) server for the IRNoti in-page (browser) phone.
#
# The browser registers a PJSIP account over WSS and the platform creates those accounts through ARI, so the PBX needs:
#   1. a WebSocket entry point: either Asterisk's own HTTPS listener (http.conf), or - with --behind-proxy, when a CDN/reverse
#      proxy ends TLS - a small proxy in front of Asterisk that forwards only /ws (nothing else is exposed)
#   2. a PJSIP transport (wss, or ws behind a proxy)   (pjsip_irnoti_ws[s].conf, included from pjsip_custom.conf / pjsip.conf)
#   3. ICE for the audio path                       (rtp.conf)
#   4. PJSIP objects stored in a database (realtime): MySQL/MariaDB + ODBC + the ps_* tables from Asterisk's own migrations
#   5. chan_sip off (optional flag): on Asterisk 16 it blocks PJSIP's WebSocket transport
#
# Safe by design: every change sits in a ";--- BEGIN irnoti-pjsip:<name> ---" block, each file is backed up to
# /var/lib/irnoti-asterisk/pjsip-backup/<time>/ first, re-running replaces the blocks, and nothing is restarted without --restart.
# Start with --check, then --dry-run. Full documentation: README.md ("Browser phone") and docs/VOIP-PJSIP-WEBRTC.md in dadehnamair/irnoti.
set -euo pipefail

ASTERISK_DIR="${ASTERISK_DIR:-/etc/asterisk}"
STATE_DIR="${STATE_DIR:-/var/lib/irnoti-asterisk}"
ODBC_INI="${ODBC_INI:-/etc/odbc.ini}"
SCHEMA_BASE="${IRNOTI_SCHEMA_BASE:-https://raw.githubusercontent.com/irnoti/asterisk-agent/main/scripts/pjsip-schema}"
TAG="irnoti-pjsip"
DSN="irnoti-pjsip"

CERT="" KEY="" TLS_PORT=8089 DB_NAME="irnoti_pjsip" DB_USER="irnoti_pjsip" DB_PASS="" MYSQL_ROOT_PASS=""
SCHEMA_SQL="" SCHEMA_PLAN="" PREBUILT=1 BIND_LOCAL=0 BEHIND_PROXY=0 PROXY_PORT=8080 STUN="" DISABLE_CHAN_SIP=0 RESTART=0 DRY=0 SKIP_DB=0 CHECK_ONLY=0 AST_SOURCE=""
MANUAL=()

usage() {
  cat <<'EOF'
Usage: pjsip-webrtc-setup.sh (--cert FILE --key FILE | --behind-proxy) [options]

  TLS is ended in one of two places; pick one:
  --cert FILE / --key FILE   Asterisk ends TLS itself: certificate (full chain) and private key the browser trusts for your domain.
  --tls-port N               ...on this HTTPS/WSS port (default 8089).
  --behind-proxy             A CDN / reverse proxy ends TLS (wss://domain/ws -> proxy -> plain ws:// to this PBX). No certificate is
                             needed here. Asterisk's HTTP server stays on 127.0.0.1 and a proxy forwards only /ws (snippet printed).
  --bind-http-local          Bind Asterisk's HTTP server to 127.0.0.1 (and, with --behind-proxy, switch its HTTPS listener off) so /ari is
                             not reachable from the network. Written to http_custom.conf when http.conf includes it (FreePBX/Issabel
                             regenerate http_additional.conf, which a later include overrides), else to http.conf.
                             Skip it if IP phones fetch provisioning files from this PBX's HTTP port.
  --proxy-port N             Port the proxy listens on for the CDN (default 8080; Cloudflare reaches origins over HTTP on
                             80, 8080, 8880, 2052, 2082, 2086, 2095).
  --disable-chan-sip         Add "noload => chan_sip.so". Needed on Asterisk 16, where chan_sip blocks PJSIP's WebSocket
                             transport. THIS STOPS EVERY chan_sip EXTENSION AND TRUNK: use it on a test PBX or after moving them to PJSIP.
  --db-name / --db-user      Database and user for the PJSIP tables (default irnoti_pjsip).
  --db-pass PASS             Password for that user (letters, digits, _ . - only; generated when omitted).
  --mysql-root-pass PASS     MySQL root password (read from /etc/issabel.conf when present, else a socket login is tried).
  --skip-db                  Do not create the database or tables (they already exist); only wire ODBC and the config files.
  --schema-sql FILE          Create the ps_* tables from this SQL file. By default the script downloads the prebuilt, checksum-pinned
                             file for this Asterisk version (scripts/pjsip-schema/), so neither python3 nor Asterisk's source is needed.
  --no-prebuilt-schema       Do not use the prebuilt file; run Asterisk's own alembic migrations on this PBX (needs python3 + pip).
  --ast-source DIR           Use this Asterisk source tree for contrib/ast-db-manage instead of downloading it.
  --stun HOST:PORT           STUN server for ICE in rtp.conf (optional).
  --restart                  Restart Asterisk at the end (needed for noload and sorcery changes to take effect).
  --check                    Only report the state of this server; change nothing.
  --dry-run                  Show what would change; change nothing.
EOF
}

say()  { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!!\033[0m  %s\n' "$*" >&2; }
die()  { printf '\033[1;31mxx\033[0m  %s\n' "$*" >&2; exit 1; }
run()  { if [ "$DRY" = 1 ]; then echo "    + $*"; else "$@"; fi; }
ast()  { asterisk -rx "$1" 2>&1 || true; }
# "Not Running" also contains "Running", so the negative is tested first
# A missing prerequisite stops a real run before anything is changed; a dry run only reports it.
need() { if [ "$DRY" = 1 ]; then warn "$1 (a real run stops here)"; else die "$1"; fi; }
http_target() { # the file our HTTP settings go to: http_custom.conf when http.conf includes it (it is read last)
  if grep -Eq '^#include[[:space:]]+"?http_custom\.conf' "$ASTERISK_DIR/http.conf" 2>/dev/null; then echo "$ASTERISK_DIR/http_custom.conf"; else echo "$ASTERISK_DIR/http.conf"; fi
}
module_state() { local l; l="$(ast "module show like $1" | grep -F "$1.so" | head -1)"; case "$l" in "") echo "not available";; *"Not Running"*) echo "Not Running";; *) echo "Running";; esac; }

# ───────── config-file blocks ─────────
begin() { echo ";--- BEGIN $TAG:$1 (managed by pjsip-webrtc-setup.sh; delete the block to undo) ---"; }
end()   { echo ";--- END $TAG:$1 ---"; }

strip_block() { # file tag → file content without our block, on stdout
  awk -v b=";--- BEGIN $TAG:$2 " -v e=";--- END $TAG:$2 ---" '
    index($0, b) == 1 { skip = 1; next }
    skip && $0 == e   { skip = 0; next }
    !skip { print }' "$1"
}

backup() {
  [ -e "$1" ] || return 0
  local dest="$BACKUP_DIR$1"
  [ -e "$dest" ] && return 0
  run mkdir -p "$(dirname "$dest")"
  run cp -p "$1" "$dest"
}

# Puts BLOCK into FILE under HEADER (inserted right after the first "[header]" line, or appended with the header when absent).
set_block() { # file tag header content
  local file="$1" tag="$2" header="$3" content="$4" tmp
  backup "$file"
  if [ "$DRY" = 1 ]; then echo "    + write block '$tag' into $file under $header"; return 0; fi
  [ -e "$file" ] || : >"$file"
  tmp="$(mktemp)"
  local block; block="$(begin "$tag"; printf '%s\n' "$content"; end "$tag")"
  if strip_block "$file" "$tag" | grep -qx "\\[$header\\]"; then
    strip_block "$file" "$tag" | awk -v h="[$header]" -v blk="$block" '{ print } !done && $0 == h { print blk; done = 1 }' >"$tmp"
  else
    { strip_block "$file" "$tag"; [ -z "$header" ] || printf '\n[%s]\n' "$header"; printf '%s\n' "$block"; } >"$tmp"
  fi
  cat "$tmp" >"$file"; rm -f "$tmp"
}

# True when REGEX matches a line of FILE outside our own block for TAG.
elsewhere() { # file tag regex
  [ -e "$1" ] && strip_block "$1" "$2" | grep -Eq "$3"
}

# ───────── checks ─────────
diagnose() {
  say "Asterisk: $(asterisk -V 2>/dev/null || echo 'not found')"
  local m
  for m in chan_sip res_pjsip_transport_websocket res_http_websocket res_srtp res_odbc res_config_odbc; do
    printf '    %-34s %s\n' "$m" "$(module_state "$m")"
  done
  echo "    transports:"; ast "pjsip show transports" | sed 's/^/      /'
  echo "    ODBC:"; ast "odbc show" | sed 's/^/      /' | head -8
}

check_deps() {
  local hint=""
  grep -qs 'release 7' /etc/centos-release 2>/dev/null && hint=" (CentOS 7 is end-of-life: point /etc/yum.repos.d/CentOS-*.repo at vault.centos.org first, see the guide)"
  [ -n "$(odbcinst -q -d 2>/dev/null | grep -iE 'mysql|mariadb')" ] || need "no MySQL/MariaDB ODBC driver registered (odbcinst -q -d): install mysql-connector-odbc / odbc-mariadb$hint"
  [ "$SKIP_DB" = 1 ] && return 0
  command -v mysql >/dev/null || need "mysql client not found (install it, or use --skip-db)$hint"
  if [ -n "$SCHEMA_SQL" ] || [ -n "$SCHEMA_PLAN" ]; then return 0; fi # tables come from a SQL file: no python needed
  command -v python3 >/dev/null && python3 -c 'import venv, ensurepip' 2>/dev/null || need "python3 with venv/pip is required for Asterisk's database migrations: yum install python3 python3-pip  /  apt install python3-venv python3-pip$hint"
}

preflight() {
  [ "$(id -u)" = 0 ] || [ "$DRY" = 1 ] || [ "$CHECK_ONLY" = 1 ] || die "run as root"
  command -v asterisk >/dev/null || die "asterisk not found in PATH"
  [ -d "$ASTERISK_DIR" ] || die "$ASTERISK_DIR not found (set ASTERISK_DIR)"
  local major; major="$(asterisk -V | grep -oE '[0-9]+' | head -1)"
  [ "${major:-0}" -ge 16 ] || die "Asterisk 16 or newer is required (found: $(asterisk -V))"
  [ "$(module_state res_pjsip)" = "Running" ] || die "res_pjsip is not running on this Asterisk"
  [ "$CHECK_ONLY" = 1 ] && return 0
  resolve_schema
  check_deps
  prepare_migrations
  if [ "$BEHIND_PROXY" = 1 ]; then return 0; fi
  [ -r "$CERT" ] && [ -r "$KEY" ] || die "give --cert and --key (Asterisk ends TLS), or --behind-proxy (a CDN/proxy ends TLS)"
  openssl x509 -in "$CERT" -noout >/dev/null 2>&1 || die "$CERT is not a certificate in PEM format"
  # cert and key must be a pair, or every browser connection fails with an unhelpful TLS error
  local a b
  a="$(openssl x509 -in "$CERT" -noout -pubkey | openssl sha256)"; b="$(openssl pkey -in "$KEY" -pubout 2>/dev/null | openssl sha256)"
  [ "$a" = "$b" ] || die "the key does not belong to the certificate"
}

# ───────── prebuilt table schema ─────────
# sha256 of the files in scripts/pjsip-schema (a test keeps this list and the files in sync); a downloaded file must match.
schema_hash() {
  case "$1" in
    ps-schema-16.30.1.sql) echo 54f146e38441e92409863fd5627b1896a9b30cbb95e891f7430faffeb5424d5a;;
    ps-schema-16.7.0.sql) echo a50c449ca0a589df9a87b7a85d3c734292cf4b5f81d58e43fcd418574609c8af;;
    ps-schema-18.26.4.sql) echo c6a4ddcb9650fab42a4b1da8e3076cc4c20985db2fa851462983f80dafa4b511;;
    ps-schema-20.21.0.sql) echo f6461451a3fa06a0f81a839ac02863160dc87260ff773e4419251ddae319755f;;
    ps-schema-22.11.0.sql) echo 48b2279c5031dbfc15c5c9fafa993d510f65cfae9ff38fb7ff6d6de34eb236a8;;
  esac
}
# The newest shipped schema of each Asterisk major: a superset of every older release of that major (extra NULL columns are harmless).
schema_latest() { case "$1" in 16) echo 16.30.1;; 18) echo 18.26.4;; 20) echo 20.21.0;; 22) echo 22.11.0;; esac; }
version_gt() { [ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -1)" = "$1" ]; }

# The SQL runs as the database root, so it may only create tables and record the migration version.
schema_safe() {
  awk '
    /^[[:space:]]*$/ { next }
    /^--/ { next }
    /^[[:space:]]/ { next }
    /^\)/ { next }
    /^(CREATE TABLE|ALTER TABLE|CREATE INDEX|CREATE UNIQUE INDEX) / { next }
    /^(UPDATE|INSERT INTO) alembic_version / { next }
    { print "unexpected statement: " $0 > "/dev/stderr"; bad = 1 }
    END { exit bad }' "$1"
}

fetch() { if command -v curl >/dev/null; then curl -fsSL --max-time 90 -o "$2" "$1"; else wget -q -T 90 -O "$2" "$1"; fi; }

# Sets SCHEMA_SQL to a verified file (or SCHEMA_PLAN in a dry run). Leaves both empty to fall back to Asterisk's migrations.
resolve_schema() {
  [ "$SKIP_DB" = 1 ] && return 0
  if [ -n "$SCHEMA_SQL" ]; then
    [ -r "$SCHEMA_SQL" ] || need "--schema-sql $SCHEMA_SQL is not a readable file"
    schema_safe "$SCHEMA_SQL" || need "$SCHEMA_SQL contains statements other than table creation; refusing to run it as the database root"
    return 0
  fi
  [ "$PREBUILT" = 1 ] || return 0
  local ver major name latest dest
  ver="$(asterisk -V | grep -oE '[0-9]+(\.[0-9]+)+' | head -1)"; major="${ver%%.*}"
  name="ps-schema-$ver.sql"
  if [ -z "$(schema_hash "$name")" ]; then
    latest="$(schema_latest "$major")"
    if [ -z "$latest" ]; then warn "no prebuilt table schema for Asterisk $ver; Asterisk's migrations will be used (needs python3)"; return 0; fi
    name="ps-schema-$latest.sql"
    version_gt "$ver" "$latest" && warn "Asterisk $ver is newer than the newest prebuilt schema ($latest); a column added since then may be missing (re-run with --no-prebuilt-schema to use the migrations)"
  fi
  say "table schema: prebuilt $name"
  if [ "$DRY" = 1 ]; then SCHEMA_PLAN="$name"; return 0; fi
  mkdir -p "$STATE_DIR/pjsip-schema"; dest="$STATE_DIR/pjsip-schema/$name"
  if fetch "$SCHEMA_BASE/$name" "$dest" && [ "$(sha256sum "$dest" | cut -d' ' -f1)" = "$(schema_hash "$name")" ] && schema_safe "$dest"; then
    SCHEMA_SQL="$dest"
  else
    rm -f "$dest"
    warn "could not get a verified $name from $SCHEMA_BASE; falling back to Asterisk's migrations (needs python3)"
  fi
}

# ───────── steps ─────────
step_chan_sip() {
  [ "$(module_state chan_sip)" = "Running" ] || return 0
  if [ "$DISABLE_CHAN_SIP" = 1 ]; then
    say "chan_sip: adding noload (takes effect after a restart)"
    set_block "$ASTERISK_DIR/modules.conf" nosip modules 'noload => chan_sip.so'
  else
    warn "chan_sip is loaded. On Asterisk 16 it keeps res_pjsip_transport_websocket from loading, so the browser phone cannot connect."
    warn "Re-run with --disable-chan-sip (test PBX), or move your extensions to PJSIP first. See the guide, section 3."
    MANUAL+=("chan_sip is still loaded: the WebSocket transport will not start until it is disabled")
  fi
}

step_proxy() {
  say "WebSocket entry point behind a TLS-ending proxy"
  # Asterisk serves /ari and /ws from the same HTTP server, so it must not be opened to the network: only /ws may pass
  local bound hport status
  status="$(ast "http show status" | grep 'Bound to' | head -1)"
  bound="$(printf '%s' "$status" | sed -n 's/.*Bound to \([0-9.]*\):.*/\1/p')"
  hport="$(printf '%s' "$status" | sed -n 's/.*Bound to [0-9.]*:\([0-9]*\).*/\1/p')"; hport="${hport:-8088}"
  if [ -z "$bound" ]; then
    warn "Asterisk's HTTP server is not running (http show status). Enable it in http.conf (enabled=yes, bindaddr=127.0.0.1)."
    MANUAL+=("http.conf: enable Asterisk's HTTP server on 127.0.0.1:$hport")
  elif [ "$bound" != "127.0.0.1" ]; then
    warn "Asterisk's HTTP server listens on $bound, so ARI (/ari) is reachable from the network. Re-run with --bind-http-local (or set bindaddr=127.0.0.1 in http_custom.conf) and let the proxy forward only /ws."
    MANUAL+=("http.conf: bindaddr=$bound exposes ARI; bind to 127.0.0.1")
  else
    say "Asterisk HTTP is on 127.0.0.1 (good)"
  fi
  # the TLS listener serves the same /ari, so it must not face the network either
  local tls; tls="$(ast "http show status" | sed -n 's/.*HTTPS Server Enabled and Bound to \([0-9.]*\):\([0-9]*\).*/\1:\2/p' | head -1)"
  if [ -n "$tls" ] && [ "${tls%%:*}" != "127.0.0.1" ]; then
    warn "Asterisk's HTTPS server is enabled on $tls and serves /ari too. With a proxy in front it is not needed: --bind-http-local switches it off (or remove tlsenable from http.conf / http_additional.conf / http_custom.conf)."
    MANUAL+=("http.conf: HTTPS listener on $tls exposes ARI; disable it or bind to 127.0.0.1")
  fi
  cat <<EOT

    Put this in front of Asterisk (nginx; the CDN's origin is http://THIS-SERVER:$PROXY_PORT). It forwards ONLY /ws:

      server {
        listen $PROXY_PORT;
        location = /ws {
          proxy_pass http://127.0.0.1:$hport/ws;
          proxy_http_version 1.1;
          proxy_set_header Upgrade \$http_upgrade;
          proxy_set_header Connection "upgrade";
          proxy_set_header Host \$host;
          proxy_read_timeout 3600s;
        }
        location / { return 404; }
      }

    No nginx (e.g. CentOS 7 without EPEL)? Apache can do the same: put this in /etc/httpd/conf.d/irnoti-ws.conf, then
    "httpd -t && service httpd reload" (needs mod_proxy and mod_proxy_wstunnel: httpd -M | grep proxy):

      Listen $PROXY_PORT
      <VirtualHost *:$PROXY_PORT>
        ProxyRequests Off
        <Location "/">
          Require all denied
        </Location>
        <Location "/ws">
          Require all granted
          ProxyPass "ws://127.0.0.1:$hport/ws"
        </Location>
      </VirtualHost>

    Allow $PROXY_PORT/tcp only from the CDN's address ranges. The audio (DTLS-SRTP over UDP) does NOT go through the CDN:
    it goes straight to this PBX's public IP, so open the RTP range (rtp.conf) to the internet.

EOT
}

step_tls() {
  say "HTTPS listener on :$TLS_PORT"
  local dir="$ASTERISK_DIR/keys/irnoti" owner
  owner="$(stat -c '%U:%G' "$ASTERISK_DIR" 2>/dev/null || echo asterisk:asterisk)"
  run mkdir -p "$dir"
  run install -m 0644 "$CERT" "$dir/fullchain.pem"
  run install -m 0640 "$KEY" "$dir/privkey.pem"
  run chown -R "$owner" "$dir"
  local target hdr=""; target="$(http_target)"; [ "$target" = "$ASTERISK_DIR/http.conf" ] && hdr=general
  if elsewhere "$target" tls '^[[:space:]]*tlsenable'; then
    warn "$target already has a TLS listener; left alone. Check that it uses port $TLS_PORT and a certificate the browser trusts."
    MANUAL+=("$target already defines tlsenable: verify its port/certificate")
    return 0
  fi
  # included files are read after http.conf's own lines, so these win over FreePBX's generated http_additional.conf
  set_block "$target" tls "$hdr" "tlsenable=yes
tlsbindaddr=0.0.0.0:$TLS_PORT
tlscertfile=$dir/fullchain.pem
tlsprivatekey=$dir/privkey.pem"
}

step_lock_http() {
  [ "$BIND_LOCAL" = 1 ] || return 0
  say "Binding Asterisk's HTTP server to 127.0.0.1"
  local target hdr=""; target="$(http_target)"; [ "$target" = "$ASTERISK_DIR/http.conf" ] && hdr=general
  local body="bindaddr=127.0.0.1"
  [ "$BEHIND_PROXY" = 1 ] && body="$body
tlsenable=no"
  set_block "$target" httplocal "$hdr" "$body"
  [ "$DRY" = 1 ] || ast "module reload http" >/dev/null
}

step_transport() {
  local proto=wss
  [ "$BEHIND_PROXY" = 1 ] && proto=ws # the proxy hands Asterisk a plain WebSocket
  say "PJSIP transport-$proto"
  if elsewhere "$ASTERISK_DIR/pjsip.conf" transport "^[[:space:]]*\\[transport-$proto\\]" || elsewhere "$ASTERISK_DIR/pjsip_custom.conf" transport "^[[:space:]]*\\[transport-$proto\\]"; then
    warn "a [transport-$proto] already exists; left alone"; return 0
  fi
  local file="$ASTERISK_DIR/pjsip_irnoti_$proto.conf" host="$ASTERISK_DIR/pjsip.conf"
  [ -e "$ASTERISK_DIR/pjsip_custom.conf" ] && host="$ASTERISK_DIR/pjsip_custom.conf" # FreePBX/Issabel regenerate pjsip.conf
  if [ "$DRY" = 1 ]; then echo "    + write $file and include it from $host"; else
    printf '[transport-%s]\ntype=transport\nprotocol=%s\nbind=0.0.0.0\n' "$proto" "$proto" >"$file"
    chown "$(stat -c '%U:%G' "$ASTERISK_DIR")" "$file" 2>/dev/null || true
  fi
  set_block "$host" transport "" "#include \"pjsip_irnoti_$proto.conf\""
}

step_rtp() {
  say "rtp.conf (ICE for WebRTC)"
  if elsewhere "$ASTERISK_DIR/rtp.conf" rtp '^[[:space:]]*icesupport'; then return 0; fi
  set_block "$ASTERISK_DIR/rtp.conf" rtp general "icesupport=yes${STUN:+
stunaddr=$STUN}"
}

mysql_root() { # sql on stdin
  local pass="$MYSQL_ROOT_PASS"
  [ -n "$pass" ] || pass="$(sed -n 's/^mysqlrootpwd=//p' /etc/issabel.conf 2>/dev/null | head -1 || true)"
  MYSQL_PWD="$pass" mysql -uroot
}

step_db() {
  [ "$SKIP_DB" = 1 ] && { say "database: skipped"; return 0; }
  say "database $DB_NAME (user $DB_USER)"
  if [ "$DRY" = 1 ]; then
    if [ -n "$SCHEMA_SQL" ]; then echo "    + create database/user, then load $SCHEMA_SQL (ps_* tables)"; elif [ -n "$SCHEMA_PLAN" ]; then echo "    + create database/user, then download, verify and load the prebuilt $SCHEMA_PLAN"; else echo "    + create database/user, then run the Asterisk alembic migrations (ps_* tables)"; fi
    return 0
  fi
  command -v mysql >/dev/null || die "mysql client not found (install it or use --skip-db)"
  # utf8 (not utf8mb4): the longest alembic index would exceed InnoDB's key limit on MariaDB 5.5 / MySQL 5.5
  mysql_root <<<"CREATE DATABASE IF NOT EXISTS \`$DB_NAME\` CHARACTER SET utf8;" || die "cannot log in to MySQL as root (use --mysql-root-pass)"
  mysql_root <<<"GRANT ALL ON \`$DB_NAME\`.* TO '$DB_USER'@'localhost' IDENTIFIED BY '$DB_PASS';" 2>/dev/null \
    || { mysql_root <<<"CREATE USER IF NOT EXISTS '$DB_USER'@'localhost' IDENTIFIED BY '$DB_PASS'; GRANT ALL ON \`$DB_NAME\`.* TO '$DB_USER'@'localhost';"; }
  if mysql_root <<<"SHOW TABLES FROM \`$DB_NAME\` LIKE 'ps_endpoints';" | grep -q ps_endpoints; then
    say "tables already exist"; return 0
  fi
  if [ -n "$SCHEMA_SQL" ]; then
    say "creating the tables from $SCHEMA_SQL"
    { printf 'USE `%s`;\n' "$DB_NAME"; cat "$SCHEMA_SQL"; } | mysql_root || die "loading $SCHEMA_SQL failed"
  else
    migrate
  fi
}

# Fallback when no prebuilt schema is used: Asterisk's own migrations, prepared BEFORE any file is changed (source, venv, packages)
# so a missing network or package stops the run early instead of half-way.
prepare_migrations() {
  [ "$SKIP_DB" = 1 ] || [ -n "$SCHEMA_SQL" ] || [ -n "$SCHEMA_PLAN" ] || [ "$DRY" = 1 ] || {
    local work="$STATE_DIR/pjsip-schema" ver
    mkdir -p "$work"
    if [ -z "$AST_SOURCE" ]; then
      ver="$(asterisk -V | grep -oE '[0-9]+(\.[0-9]+)+' | head -1)"
      say "downloading Asterisk $ver migrations"
      fetch "https://downloads.asterisk.org/pub/telephony/asterisk/releases/asterisk-$ver.tar.gz" "$work/src.tgz" \
        || fetch "https://downloads.asterisk.org/pub/telephony/asterisk/asterisk-${ver%%.*}-current.tar.gz" "$work/src.tgz" \
        || die "download failed; use --schema-sql with a file made elsewhere, or pass the Asterisk source with --ast-source DIR"
      tar -xzf "$work/src.tgz" -C "$work" --wildcards '*/contrib/ast-db-manage' || die "cannot unpack the Asterisk source"
      AST_SOURCE="$(echo "$work"/asterisk-*)"
      rm -f "$work/src.tgz"
    fi
    [ -d "$AST_SOURCE/contrib/ast-db-manage" ] || die "$AST_SOURCE/contrib/ast-db-manage not found"
    python3 -m venv "$work/venv" && "$work/venv/bin/pip" -q install 'alembic<1.8' 'sqlalchemy<1.5' pymysql || die "cannot prepare the migration tools (python venv / pip install)"
  }
}

migrate() {
  local work="$STATE_DIR/pjsip-schema"
  cp "$AST_SOURCE/contrib/ast-db-manage/config.ini.sample" "$work/config.ini"
  sed -i "s#^sqlalchemy.url.*#sqlalchemy.url = mysql+pymysql://$DB_USER:$DB_PASS@localhost/$DB_NAME#" "$work/config.ini"
  chmod 600 "$work/config.ini"
  ( cd "$AST_SOURCE/contrib/ast-db-manage" && "$work/venv/bin/alembic" -c "$work/config.ini" upgrade head ) || { rm -f "$work/config.ini"; die "alembic failed"; }
  rm -f "$work/config.ini"
}

step_odbc() {
  say "ODBC data source $DSN"
  local driver
  driver="$(odbcinst -q -d 2>/dev/null | tr -d '[]' | grep -iE 'mysql|mariadb' | head -1 || true)"
  if [ -z "$driver" ]; then
    [ "$DRY" = 1 ] && { warn "no MySQL/MariaDB ODBC driver registered (a real run stops here)"; driver="MariaDB"; } \
      || die "no MySQL/MariaDB ODBC driver registered (yum install mysql-connector-odbc  /  apt install odbc-mariadb)"
  fi
  # MySQL's own ODBC driver (Connector/ODBC 5.x) ships its own client library whose default socket is /tmp/mysql.sock, so
  # without an explicit Socket= a "localhost" connection fails on distributions that keep the socket elsewhere.
  local sock=""
  if [ "$DRY" = 0 ] && command -v mysql >/dev/null; then
    sock="$(mysql_root <<<"SELECT @@socket;" 2>/dev/null | tail -1 || true)"
    case "$sock" in /*) ;; *) sock="";; esac
  fi
  set_block "$ODBC_INI" odbc "$DSN" "Description=IRNoti PJSIP realtime
Driver=$driver
Server=localhost
Port=3306
Database=$DB_NAME${sock:+
Socket=$sock}"
  set_block "$ASTERISK_DIR/res_odbc.conf" odbc "$DSN" "enabled => yes
dsn => $DSN
username => $DB_USER
password => $DB_PASS
pre-connect => yes"
  [ "$DRY" = 1 ] || chmod 640 "$ASTERISK_DIR/res_odbc.conf" || true
}

step_sorcery() {
  say "sorcery.conf / extconfig.conf (PJSIP objects live in the database)"
  if elsewhere "$ASTERISK_DIR/sorcery.conf" sorcery '^[[:space:]]*(endpoint|auth|aor)[[:space:]]*='; then
    warn "sorcery.conf already maps PJSIP objects; left alone."; MANUAL+=("sorcery.conf: add the realtime lines for endpoint/auth/aor by hand (see the guide)")
  else
    # keep objects defined in pjsip.conf working (trunks) next to the ones created by the platform
    set_block "$ASTERISK_DIR/sorcery.conf" sorcery res_pjsip "endpoint=config,pjsip.conf,criteria=type=endpoint
endpoint=realtime,ps_endpoints
auth=config,pjsip.conf,criteria=type=auth
auth=realtime,ps_auths
aor=config,pjsip.conf,criteria=type=aor
aor=realtime,ps_aors"
  fi
  if elsewhere "$ASTERISK_DIR/extconfig.conf" extconfig '^[[:space:]]*ps_(endpoints|auths|aors)[[:space:]]*=>'; then
    warn "extconfig.conf already maps ps_*; left alone."; MANUAL+=("extconfig.conf: point ps_endpoints/ps_auths/ps_aors at the $DSN DSN")
  else
    set_block "$ASTERISK_DIR/extconfig.conf" extconfig settings "ps_endpoints => odbc,$DSN
ps_auths => odbc,$DSN
ps_aors => odbc,$DSN"
  fi
}

step_finish() {
  if [ "$RESTART" = 1 ] && [ "$DRY" = 0 ]; then
    say "restarting Asterisk"
    if command -v systemctl >/dev/null && systemctl list-unit-files asterisk.service >/dev/null 2>&1; then systemctl restart asterisk
    else service asterisk restart; fi
    sleep 8
    diagnose
    ast "odbc show" | grep -A4 "$DSN" | grep -Eq 'active connections: [1-9]' \
      || { warn "the ODBC data source $DSN is not connected: check Socket= and Database= in $ODBC_INI, the user/password in res_odbc.conf and /var/log/asterisk/full"; MANUAL+=("ODBC data source $DSN is not connected"); }
  else
    say "Restart Asterisk (service asterisk restart), then check:"
    echo '    asterisk -rx "module show like pjsip_transport_websocket"   # Running'
    echo '    asterisk -rx "pjsip show transports"                        # transport-ws / transport-wss'
    echo '    asterisk -rx "odbc show"                                    # irnoti-pjsip connected'
  fi
  if [ ${#MANUAL[@]} -gt 0 ]; then
    echo; warn "Needs your attention:"; printf '    - %s\n' "${MANUAL[@]}"
  fi
  echo
  if [ "$BEHIND_PROXY" = 1 ]; then
    say "Next: in /admin/providers set on this server  sipDomain, sipWsUrl (wss://YOUR-DOMAIN/ws, the CDN's address) and sipTransport = transport-ws;"
    say "      start the proxy above, open the RTP range (rtp.conf) in the firewall, then create an account in /panel/voip/softphone."
  else
    say "Next: in /admin/providers set sipDomain and sipWsUrl (wss://YOUR-DOMAIN:$TLS_PORT/ws) on this server, open $TLS_PORT/tcp and the RTP range (rtp.conf) in the firewall, then create an account in /panel/voip/softphone."
  fi
}

main() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --cert) CERT="$2"; shift 2;; --key) KEY="$2"; shift 2;; --tls-port) TLS_PORT="$2"; shift 2;;
      --behind-proxy) BEHIND_PROXY=1; shift;; --bind-http-local) BIND_LOCAL=1; shift;; --proxy-port) PROXY_PORT="$2"; shift 2;;
      --db-name) DB_NAME="$2"; shift 2;; --db-user) DB_USER="$2"; shift 2;; --db-pass) DB_PASS="$2"; shift 2;;
      --mysql-root-pass) MYSQL_ROOT_PASS="$2"; shift 2;; --ast-source) AST_SOURCE="$2"; shift 2;; --schema-sql) SCHEMA_SQL="$2"; shift 2;; --no-prebuilt-schema) PREBUILT=0; shift;; --stun) STUN="$2"; shift 2;;
      --disable-chan-sip) DISABLE_CHAN_SIP=1; shift;; --restart) RESTART=1; shift;; --skip-db) SKIP_DB=1; shift;;
      --check) CHECK_ONLY=1; shift;; --dry-run) DRY=1; shift;; -h|--help) usage; exit 0;;
      *) usage; die "unknown option $1";;
    esac
  done
  [[ "$TLS_PORT" =~ ^[0-9]{2,5}$ ]] || die "--tls-port must be a number"
  [[ "$PROXY_PORT" =~ ^[0-9]{2,5}$ ]] || die "--proxy-port must be a number"
  if [ "$BEHIND_PROXY" = 1 ] && [ -n "$CERT$KEY" ]; then die "--behind-proxy and --cert/--key exclude each other (TLS is ended in one place)"; fi
  [[ "$DB_NAME$DB_USER" =~ ^[A-Za-z0-9_]+$ ]] || die "database and user names may only contain letters, digits and _"
  if [ -z "$DB_PASS" ]; then DB_PASS="$(openssl rand -hex 16)"; fi
  [[ "$DB_PASS" =~ ^[A-Za-z0-9_.-]+$ ]] || die "--db-pass may only contain letters, digits, _ . -"
  BACKUP_DIR="$STATE_DIR/pjsip-backup/$(date -u +%Y%m%dT%H%M%SZ)"

  preflight
  if [ "$CHECK_ONLY" = 1 ]; then diagnose; exit 0; fi
  [ "$DRY" = 1 ] && warn "dry run: nothing is changed"
  step_chan_sip; if [ "$BEHIND_PROXY" = 1 ]; then step_lock_http; step_proxy; else step_tls; step_lock_http; fi; step_transport; step_rtp; step_db; step_odbc; step_sorcery
  [ "$DRY" = 1 ] || say "backups of changed files: $BACKUP_DIR"
  step_finish
}

if [ "${IRNOTI_SOURCE_ONLY:-0}" != 1 ]; then main "$@"; fi
