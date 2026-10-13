#!/usr/bin/env bash
# Tests for scripts/bootstrap.sh against a local HTTP server and a fake
# release: wrong checksum is refused, a correct one installs.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
boot="${here}/bootstrap.sh"
work="$(mktemp -d)"
srv_pid=""
cleanup() { [ -z "${srv_pid}" ] || kill "${srv_pid}" 2>/dev/null || true; rm -rf "${work}"; }
trap cleanup EXIT

version="$(sed -n 's/^[[:space:]]*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${here}/../manifest.json" | head -n 1)"
case "$(uname -m)" in aarch64|arm64) arch=arm64; mach='\xb7\x00' ;; *) arch=amd64; mach='\x3e\x00' ;; esac
name="qcy-omarchy_${version}_linux_${arch}.tar.gz"

# A file with a valid ELF header for this machine (never executed).
fake_elf() { printf '\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00'"${mach}"'padding-padding' > "$1"; }

mkdir -p "${work}/pkg" "${work}/good" "${work}/bad" "${work}/nosrc"
fake_elf "${work}/pkg/qcyctl"; fake_elf "${work}/pkg/qcyctld"
tar -czf "${work}/good/${name}" -C "${work}/pkg" qcyctl qcyctld
sum="$(sha256sum "${work}/good/${name}" | awk '{print $1}')"
printf '%s  %s\n' "${sum}" "${name}" > "${work}/good/SHA256SUMS"
cp "${work}/good/${name}" "${work}/bad/${name}"
printf '%064d  %s\n' 0 "${name}" > "${work}/bad/SHA256SUMS"

port=$((20000 + RANDOM % 20000))
python3 -I -m http.server "${port}" --bind 127.0.0.1 --directory "${work}" >/dev/null 2>&1 &
srv_pid=$!
for _ in $(seq 1 50); do curl -fs "http://127.0.0.1:${port}/good/SHA256SUMS" >/dev/null 2>&1 && break; sleep 0.1; done

fails=0
check() { if [ "$2" = ok ]; then echo "PASS $1"; else echo "FAIL $1"; fails=$((fails + 1)); fi; }

run() { # data-dir base-url -> output in ${work}/out, status in $rc
  set +e
  QCY_DATA_DIR="$1" QCY_RELEASE_BASE_URL="$2" QCY_SRC_DIR="${work}/nosrc" \
    XDG_CACHE_HOME="${work}/cache" bash "${boot}" >"${work}/out" 2>&1
  rc=$?
  set -e
}

# 1. wrong checksum: nothing installed, final ERROR
run "${work}/d1" "http://127.0.0.1:${port}/bad"
if [ "${rc}" -ne 0 ] && grep -q '^ERROR' "${work}/out" && [ ! -e "${work}/d1/bin/qcyctl" ]; then check "wrong checksum refused" ok; else check "wrong checksum refused" no; cat "${work}/out"; fi

# 2. correct checksum: both binaries installed, .version written
run "${work}/d2" "http://127.0.0.1:${port}/good"
if [ "${rc}" -eq 0 ] && [ -x "${work}/d2/bin/qcyctl" ] && [ -x "${work}/d2/bin/qcyctld" ] \
   && [ "$(cat "${work}/d2/bin/.version")" = "${version}" ] && grep -q '^STEP done' "${work}/out"; then
  check "correct checksum installs" ok; else check "correct checksum installs" no; cat "${work}/out"; fi

# 3. not an ELF of this machine: refused even with a matching checksum
printf '#!/bin/sh\necho pwn\n' > "${work}/pkg/qcyctl"
tar -czf "${work}/good/${name}" -C "${work}/pkg" qcyctl qcyctld
printf '%s  %s\n' "$(sha256sum "${work}/good/${name}" | awk '{print $1}')" "${name}" > "${work}/good/SHA256SUMS"
run "${work}/d3" "http://127.0.0.1:${port}/good"
if [ "${rc}" -ne 0 ] && [ ! -e "${work}/d3/bin/qcyctl" ]; then check "non-ELF refused" ok; else check "non-ELF refused" no; cat "${work}/out"; fi

[ "${fails}" -eq 0 ] && echo "all passed" || { echo "${fails} failed"; exit 1; }
