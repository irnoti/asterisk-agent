#!/usr/bin/env bash
# Prepares an Asterisk (16+) server for the IRNoti browser phone: PJSIP over WSS, PJSIP realtime in MySQL/MariaDB (ODBC),
# and the HTTPS listener the WebSocket rides on. Guide: docs/VOIP-PJSIP-WEBRTC.md in the IRNoti platform repository (dadehnamair/irnoti)
#
# Every file change sits in a marked block, is backed up first, and can be removed by deleting the block. Re-running is safe.
# Nothing is restarted unless you pass --restart.
set -euo pipefail

ASTERISK_DIR="${ASTERISK_DIR:-/etc/asterisk}"
STATE_DIR="${STATE_DIR:-/var/lib/irnoti-asterisk}"
ODBC_INI="${ODBC_INI:-/etc/odbc.ini}"
TAG="irnoti-pjsip"
DSN="irnoti-pjsip"

CERT="" KEY="" TLS_PORT=8089 DB_NAME="irnoti_pjsip" DB_USER="irnoti_pjsip" DB_PASS="" MYSQL_ROOT_PASS=""
STUN="" DISABLE_CHAN_SIP=0 RESTART=0 DRY=0 SKIP_DB=0 CHECK_ONLY=0 AST_SOURCE=""
MANUAL=()

usage() {
  cat <<'EOF'
Usage: pjsip-webrtc-setup.sh --cert FILE --key FILE [options]

  --cert FILE / --key FILE   Certificate (full chain) and private key the browser will trust for your PBX domain.
  --tls-port N               HTTPS/WSS port of Asterisk (default 8089; use 8443 behind a Cloudflare-style proxy).
  --disable-chan-sip         Add "noload => chan_sip.so". Needed on Asterisk 16, where chan_sip blocks PJSIP's WebSocket
                             transport. THIS STOPS EVERY chan_sip EXTENSION AND TRUNK: use it on a test PBX or after moving them to PJSIP.
  --db-name / --db-user      Database and user for the PJSIP tables (default irnoti_pjsip).
  --db-pass PASS             Password for that user (letters, digits, _ . - only; generated when omitted).
  --mysql-root-pass PASS     MySQL root password (read from /etc/issabel.conf when present, else a socket login is tried).
  --skip-db                  Do not create the database or tables (they already exist); only wire ODBC and the config files.
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

preflight() {
  [ "$(id -u)" = 0 ] || [ "$DRY" = 1 ] || [ "$CHECK_ONLY" = 1 ] || die "run as root"
  command -v asterisk >/dev/null || die "asterisk not found in PATH"
  [ -d "$ASTERISK_DIR" ] || die "$ASTERISK_DIR not found (set ASTERISK_DIR)"
  local major; major="$(asterisk -V | grep -oE '[0-9]+' | head -1)"
  [ "${major:-0}" -ge 16 ] || die "Asterisk 16 or newer is required (found: $(asterisk -V))"
  [ "$(module_state res_pjsip)" = "Running" ] || die "res_pjsip is not running on this Asterisk"
  [ "$CHECK_ONLY" = 1 ] && return 0
  [ -r "$CERT" ] && [ -r "$KEY" ] || die "--cert and --key must point to readable files"
  openssl x509 -in "$CERT" -noout >/dev/null 2>&1 || die "$CERT is not a certificate in PEM format"
  # cert and key must be a pair, or every browser connection fails with an unhelpful TLS error
  local a b
  a="$(openssl x509 -in "$CERT" -noout -pubkey | openssl sha256)"; b="$(openssl pkey -in "$KEY" -pubout 2>/dev/null | openssl sha256)"
  [ "$a" = "$b" ] || die "the key does not belong to the certificate"
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

step_tls() {
  say "HTTPS listener on :$TLS_PORT"
  local dir="$ASTERISK_DIR/keys/irnoti" owner
  owner="$(stat -c '%U:%G' "$ASTERISK_DIR" 2>/dev/null || echo asterisk:asterisk)"
  run mkdir -p "$dir"
  run install -m 0644 "$CERT" "$dir/fullchain.pem"
  run install -m 0640 "$KEY" "$dir/privkey.pem"
  run chown -R "$owner" "$dir"
  if elsewhere "$ASTERISK_DIR/http.conf" tls '^[[:space:]]*tlsenable'; then
    warn "http.conf already has a TLS listener; left alone. Check that it uses port $TLS_PORT and a certificate the browser trusts."
    MANUAL+=("http.conf already defines tlsenable: verify its port/certificate")
    return 0
  fi
  set_block "$ASTERISK_DIR/http.conf" tls general "tlsenable=yes
tlsbindaddr=0.0.0.0:$TLS_PORT
tlscertfile=$dir/fullchain.pem
tlsprivatekey=$dir/privkey.pem"
}

step_transport() {
  say "PJSIP transport-wss"
  if elsewhere "$ASTERISK_DIR/pjsip.conf" transport '^[[:space:]]*\[transport-wss\]' || elsewhere "$ASTERISK_DIR/pjsip_custom.conf" transport '^[[:space:]]*\[transport-wss\]'; then
    warn "a [transport-wss] already exists; left alone"; return 0
  fi
  local file="$ASTERISK_DIR/pjsip_irnoti_wss.conf" host="$ASTERISK_DIR/pjsip.conf"
  [ -e "$ASTERISK_DIR/pjsip_custom.conf" ] && host="$ASTERISK_DIR/pjsip_custom.conf" # FreePBX/Issabel regenerate pjsip.conf
  if [ "$DRY" = 1 ]; then echo "    + write $file and include it from $host"; else
    printf '[transport-wss]\ntype=transport\nprotocol=wss\nbind=0.0.0.0\n' >"$file"
    chown "$(stat -c '%U:%G' "$ASTERISK_DIR")" "$file" 2>/dev/null || true
  fi
  set_block "$host" transport "" '#include "pjsip_irnoti_wss.conf"'
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
  if [ "$DRY" = 1 ]; then echo "    + create database/user, then run Asterisk's alembic migrations (ps_* tables)"; return 0; fi
  command -v mysql >/dev/null || die "mysql client not found (install it or use --skip-db)"
  # utf8 (not utf8mb4): the longest alembic index would exceed InnoDB's key limit on MariaDB 5.5 / MySQL 5.5
  mysql_root <<<"CREATE DATABASE IF NOT EXISTS \`$DB_NAME\` CHARACTER SET utf8;" || die "cannot log in to MySQL as root (use --mysql-root-pass)"
  mysql_root <<<"GRANT ALL ON \`$DB_NAME\`.* TO '$DB_USER'@'localhost' IDENTIFIED BY '$DB_PASS';" 2>/dev/null \
    || { mysql_root <<<"CREATE USER IF NOT EXISTS '$DB_USER'@'localhost' IDENTIFIED BY '$DB_PASS'; GRANT ALL ON \`$DB_NAME\`.* TO '$DB_USER'@'localhost';"; }
  if mysql_root <<<"SHOW TABLES FROM \`$DB_NAME\` LIKE 'ps_endpoints';" | grep -q ps_endpoints; then
    say "tables already exist"; return 0
  fi
  migrate
}

migrate() {
  # Asterisk's own migrations are the only schema that is guaranteed to match what it reads and writes
  command -v python3 >/dev/null || { command -v yum >/dev/null && yum -y install python3 || die "python3 is required"; }
  local work="$STATE_DIR/pjsip-schema" src="$AST_SOURCE" ver
  mkdir -p "$work"
  if [ -z "$src" ]; then
    ver="$(asterisk -V | grep -oE '[0-9]+(\.[0-9]+)+' | head -1)"
    say "downloading Asterisk $ver migrations"
    curl -fsSL "https://downloads.asterisk.org/pub/telephony/asterisk/releases/asterisk-$ver.tar.gz" -o "$work/src.tgz" \
      || curl -fsSL "https://downloads.asterisk.org/pub/telephony/asterisk/asterisk-${ver%%.*}-current.tar.gz" -o "$work/src.tgz" \
      || die "download failed; fetch the Asterisk source yourself and pass --ast-source DIR"
    tar -xzf "$work/src.tgz" -C "$work" --wildcards '*/contrib/ast-db-manage'
    src="$(echo "$work"/asterisk-*)"
  fi
  [ -d "$src/contrib/ast-db-manage" ] || die "$src/contrib/ast-db-manage not found"
  python3 -m venv "$work/venv"
  "$work/venv/bin/pip" -q install 'alembic<1.8' 'sqlalchemy<1.5' pymysql
  cp "$src/contrib/ast-db-manage/config.ini.sample" "$work/config.ini"
  sed -i "s#^sqlalchemy.url.*#sqlalchemy.url = mysql+pymysql://$DB_USER:$DB_PASS@localhost/$DB_NAME#" "$work/config.ini"
  chmod 600 "$work/config.ini"
  ( cd "$src/contrib/ast-db-manage" && "$work/venv/bin/alembic" -c "$work/config.ini" upgrade head ) || die "alembic failed"
  rm -f "$work/config.ini" "$work/src.tgz"
}

step_odbc() {
  say "ODBC data source $DSN"
  local driver
  driver="$(odbcinst -q -d 2>/dev/null | tr -d '[]' | grep -iE 'mysql|mariadb' | head -1 || true)"
  if [ -z "$driver" ]; then
    [ "$DRY" = 1 ] && { warn "no MySQL/MariaDB ODBC driver registered (a real run stops here)"; driver="MariaDB"; } \
      || die "no MySQL/MariaDB ODBC driver registered (yum install mysql-connector-odbc  /  apt install odbc-mariadb)"
  fi
  set_block "$ODBC_INI" odbc "$DSN" "Description=IRNoti PJSIP realtime
Driver=$driver
Server=localhost
Port=3306
Database=$DB_NAME"
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
  else
    say "Restart Asterisk (service asterisk restart), then check:"
    echo '    asterisk -rx "module show like pjsip_transport_websocket"   # Running'
    echo '    asterisk -rx "pjsip show transports"                        # transport-wss'
    echo '    asterisk -rx "odbc show"                                    # irnoti-pjsip connected'
  fi
  if [ ${#MANUAL[@]} -gt 0 ]; then
    echo; warn "Needs your attention:"; printf '    - %s\n' "${MANUAL[@]}"
  fi
  echo
  say "Next: in /admin/providers set sipDomain and sipWsUrl (wss://YOUR-DOMAIN:$TLS_PORT/ws) on this server, open $TLS_PORT/tcp and the RTP range (rtp.conf) in the firewall, then create an account in /panel/voip/softphone."
}

main() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --cert) CERT="$2"; shift 2;; --key) KEY="$2"; shift 2;; --tls-port) TLS_PORT="$2"; shift 2;;
      --db-name) DB_NAME="$2"; shift 2;; --db-user) DB_USER="$2"; shift 2;; --db-pass) DB_PASS="$2"; shift 2;;
      --mysql-root-pass) MYSQL_ROOT_PASS="$2"; shift 2;; --ast-source) AST_SOURCE="$2"; shift 2;; --stun) STUN="$2"; shift 2;;
      --disable-chan-sip) DISABLE_CHAN_SIP=1; shift;; --restart) RESTART=1; shift;; --skip-db) SKIP_DB=1; shift;;
      --check) CHECK_ONLY=1; shift;; --dry-run) DRY=1; shift;; -h|--help) usage; exit 0;;
      *) usage; die "unknown option $1";;
    esac
  done
  [[ "$TLS_PORT" =~ ^[0-9]{2,5}$ ]] || die "--tls-port must be a number"
  [[ "$DB_NAME$DB_USER" =~ ^[A-Za-z0-9_]+$ ]] || die "database and user names may only contain letters, digits and _"
  if [ -z "$DB_PASS" ]; then DB_PASS="$(openssl rand -hex 16)"; fi
  [[ "$DB_PASS" =~ ^[A-Za-z0-9_.-]+$ ]] || die "--db-pass may only contain letters, digits, _ . -"
  BACKUP_DIR="$STATE_DIR/pjsip-backup/$(date -u +%Y%m%dT%H%M%SZ)"

  preflight
  if [ "$CHECK_ONLY" = 1 ]; then diagnose; exit 0; fi
  [ "$DRY" = 1 ] && warn "dry run: nothing is changed"
  step_chan_sip; step_tls; step_transport; step_rtp; step_db; step_odbc; step_sorcery
  [ "$DRY" = 1 ] || say "backups of changed files: $BACKUP_DIR"
  step_finish
}

if [ "${IRNOTI_SOURCE_ONLY:-0}" != 1 ]; then main "$@"; fi
