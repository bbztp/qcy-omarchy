#!/usr/bin/env bash
# Tests for scripts/bootstrap.sh against a local HTTP server and a fake
# release: wrong checksum is refused, a correct one installs into the plugin
# directory and writes nothing elsewhere, plus the read-only and build fallbacks.
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

# A fresh copy of the repo (without .bin/.git) acts as the plugin directory.
new_plugin() { # name -> plugin dir path
  mkdir -p "${work}/$1"
  ( cd "${here}/.." && tar -cf - --exclude=.git --exclude=.bin --exclude=build --exclude=dist . ) | tar -xf - -C "${work}/$1"
  printf '%s\n' "${work}/$1"
}

run() { # plugin-dir base-url [env...] -> output in ${work}/out, status in $rc
  local p="$1" url="$2"; shift 2
  rm -rf "${work}/home"; mkdir -p "${work}/home"
  set +e
  env HOME="${work}/home" XDG_DATA_HOME="${work}/home/.local/share" XDG_CACHE_HOME="${work}/home/.cache" \
    QCY_RELEASE_BASE_URL="${url}" QCY_SRC_DIR="${work}/nosrc" "$@" bash "${p}/scripts/bootstrap.sh" >"${work}/out" 2>&1
  rc=$?
  set -e
}

# 1. wrong checksum: nothing installed, final ERROR
p1="$(new_plugin p1)"
run "${p1}" "http://127.0.0.1:${port}/bad"
if [ "${rc}" -ne 0 ] && grep -q '^ERROR' "${work}/out" && [ ! -e "${p1}/.bin/qcyctl" ]; then check "wrong checksum refused" ok; else check "wrong checksum refused" no; cat "${work}/out"; fi

# 2. correct checksum: installed inside the plugin dir, nothing outside it
p2="$(new_plugin p2)"
run "${p2}" "http://127.0.0.1:${port}/good"
if [ "${rc}" -eq 0 ] && [ -x "${p2}/.bin/qcyctl" ] && [ -x "${p2}/.bin/qcyctld" ] \
   && [ "$(cat "${p2}/.bin/.version")" = "${version}" ] && grep -q '^STEP done' "${work}/out" \
   && grep -qx "BIN ${p2}/.bin/qcyctl" "${work}/out"; then
  check "correct checksum installs into the plugin dir" ok; else check "correct checksum installs into the plugin dir" no; cat "${work}/out"; fi
if [ -z "$(find "${work}/home" -mindepth 1 2>/dev/null)" ] && [ -z "$(find "${p2}/.bin" -mindepth 1 -type d -name '.bootstrap.*' 2>/dev/null)" ]; then
  check "nothing written outside the plugin dir; no staging left" ok; else check "nothing written outside the plugin dir; no staging left" no; find "${work}/home" "${p2}/.bin"; fi

# 3. not an ELF of this machine: refused even with a matching checksum
printf '#!/bin/sh\necho pwn\n' > "${work}/pkg/qcyctl"
tar -czf "${work}/good/${name}" -C "${work}/pkg" qcyctl qcyctld
printf '%s  %s\n' "$(sha256sum "${work}/good/${name}" | awk '{print $1}')" "${name}" > "${work}/good/SHA256SUMS"
p3="$(new_plugin p3)"
run "${p3}" "http://127.0.0.1:${port}/good"
if [ "${rc}" -ne 0 ] && [ ! -e "${p3}/.bin/qcyctl" ]; then check "non-ELF refused" ok; else check "non-ELF refused" no; cat "${work}/out"; fi

# 4. plugin dir not writable: falls back to the data directory
if [ "$(id -u)" -ne 0 ]; then
  fake_elf "${work}/pkg/qcyctl"
  tar -czf "${work}/good/${name}" -C "${work}/pkg" qcyctl qcyctld
  printf '%s  %s\n' "$(sha256sum "${work}/good/${name}" | awk '{print $1}')" "${name}" > "${work}/good/SHA256SUMS"
  p4="$(new_plugin p4)"; chmod a-w "${p4}"
  run "${p4}" "http://127.0.0.1:${port}/good"
  chmod u+w "${p4}"
  if [ "${rc}" -eq 0 ] && [ -x "${work}/home/.local/share/qcy-omarchy/bin/qcyctl" ] && [ ! -e "${p4}/.bin" ]; then
    check "read-only plugin dir falls back to the data dir" ok; else check "read-only plugin dir falls back to the data dir" no; cat "${work}/out"; fi
fi

# 5. build fallback: a fake `go` shows where the build cache goes; nothing
# stays under ~/.cache and the cache is removed with the staging directory
mkdir -p "${work}/fakebin" "${work}/src/cmd/qcyctl"
cat > "${work}/fakebin/go" <<ENDGO
#!/usr/bin/env bash
printf '%s\n' "\${GOCACHE}" > "${work}/gocache.path"
mkdir -p "\${GOCACHE}"; : > "\${GOCACHE}/entry"
out=""
while [ \$# -gt 0 ]; do [ "\$1" = "-o" ] && out="\$2"; shift; done
printf '\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00${mach}padding-padding' > "\${out}qcyctl"
cp "\${out}qcyctl" "\${out}qcyctld"
ENDGO
chmod +x "${work}/fakebin/go"
p5="$(new_plugin p5)"
run "${p5}" "http://127.0.0.1:${port}/none" QCY_BUILD=1 QCY_SRC_DIR="${work}/src" PATH="${work}/fakebin:${PATH}"
gc="$(cat "${work}/gocache.path" 2>/dev/null || true)"
case "${gc}" in "${p5}/.bin/"*) in_plugin=ok ;; *) in_plugin=no ;; esac
if [ "${rc}" -eq 0 ] && [ -x "${p5}/.bin/qcyctld" ] && [ "${in_plugin}" = ok ] && [ ! -e "${gc}" ] \
   && [ -z "$(find "${work}/home" -mindepth 1 2>/dev/null)" ]; then
  check "build fallback keeps its cache inside the plugin dir and removes it" ok
else check "build fallback keeps its cache inside the plugin dir and removes it" no; cat "${work}/out"; echo "gocache=${gc}"; fi

[ "${fails}" -eq 0 ] && echo "all passed" || { echo "${fails} failed"; exit 1; }
