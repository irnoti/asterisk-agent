#!/usr/bin/env bash
# Installs the freshly built packages in clean containers and checks the binary, the install dry run and removal.
# Needs Docker and `make packages` run first. Usage: scripts/test-packages.sh
set -euo pipefail
cd "$(dirname "$0")/.."

case "$(uname -m)" in x86_64) arch=x86_64; deb=amd64 ;; aarch64) arch=aarch64; deb=arm64 ;; *) echo "unsupported host"; exit 1 ;; esac
rpm=$(ls dist/irnoti-asterisk-*."$arch".rpm | head -1)
dpkg=$(ls dist/irnoti-asterisk_*_"$deb".deb | head -1)

# `install` needs a live API, so only the parts that work offline are checked: the version, the install path and a clear error without a config.
check='irnoti-asterisk version | grep -q "^irnoti-asterisk v" && test -x /usr/bin/irnoti-asterisk
irnoti-asterisk status --config /nonexistent 2>&1 | grep -q "run .irnoti-asterisk install. first"'

for image in rockylinux:8 centos:7; do
  echo "== $image"
  # centos:7 is end-of-life: its mirrors moved to vault.centos.org
  docker run --rm -v "$PWD/dist:/dist:ro" "$image" bash -ec "
    if [ -f /etc/yum.repos.d/CentOS-Base.repo ]; then sed -i 's|^mirrorlist|#mirrorlist|; s|^#baseurl=http://mirror.centos.org|baseurl=http://vault.centos.org|' /etc/yum.repos.d/CentOS-Base.repo; fi
    rpm -ivh /dist/$(basename "$rpm")
    $check
    rpm -V irnoti-asterisk
    rpm -e irnoti-asterisk
    test ! -e /usr/bin/irnoti-asterisk"
done

echo "== debian:12"
docker run --rm -v "$PWD/dist:/dist:ro" debian:12 bash -ec "
  dpkg -i /dist/$(basename "$dpkg")
  $check
  dpkg -r irnoti-asterisk
  test ! -e /usr/bin/irnoti-asterisk"
echo "all package checks passed"
