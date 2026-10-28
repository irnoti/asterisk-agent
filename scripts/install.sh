#!/usr/bin/env bash
# Installs irnoti-asterisk: downloads the binary for this CPU, checks its SHA-256, then starts it.
#   curl -fsSL .../install.sh | sudo bash                      guided setup (asks everything; installs Asterisk too if you have none)
#   curl -fsSL .../install.sh | sudo bash -s -- setup --yes ...  guided setup, answers from flags
#   curl -fsSL .../install.sh | sudo bash -s -- --api https://api.example.com --token irna_xxx   plain `install` for an Asterisk you already run
# Overrides: IRNOTI_AGENT_VERSION (default: latest), IRNOTI_AGENT_BASE_URL (mirror of the release files).
set -euo pipefail

[ "$(id -u)" -eq 0 ] || { echo "run as root (sudo)" >&2; exit 1; }
[ "$(uname -s)" = "Linux" ] || { echo "Linux only" >&2; exit 1; }

case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported CPU: $(uname -m)" >&2; exit 1 ;;
esac

repo="irnoti/asterisk-agent"
version="${IRNOTI_AGENT_VERSION:-latest}"
if [ -n "${IRNOTI_AGENT_BASE_URL:-}" ]; then
  base="${IRNOTI_AGENT_BASE_URL%/}"
elif [ "$version" = "latest" ]; then
  base="https://github.com/${repo}/releases/latest/download"
else
  base="https://github.com/${repo}/releases/download/${version}"
fi

name="irnoti-asterisk-linux-${arch}"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT

echo "downloading ${name} …"
curl -fsSL "${base}/${name}" -o "${tmp}/${name}"
curl -fsSL "${base}/SHA256SUMS" -o "${tmp}/SHA256SUMS"
( cd "$tmp" && grep " ${name}\$" SHA256SUMS | sha256sum -c - ) || { echo "checksum mismatch: not installing" >&2; exit 1; }

install -m 0755 "${tmp}/${name}" /usr/local/bin/irnoti-asterisk
echo "installed $(/usr/local/bin/irnoti-asterisk version)"
# no arguments, or "setup ...": the guided wizard (its questions are read from the terminal even when this script came through a pipe)
if [ "$#" -eq 0 ] || [ "$1" = "setup" ]; then
  [ "$#" -gt 0 ] && shift
  if [ -r /dev/tty ] && [[ " $* " != *" --yes "* ]]; then exec /usr/local/bin/irnoti-asterisk setup "$@" </dev/tty; fi
  exec /usr/local/bin/irnoti-asterisk setup "$@"
fi
exec /usr/local/bin/irnoti-asterisk install "$@"
