#!/usr/bin/env bash
# Tests for scripts/bootstrap.sh against a local HTTP server and a fake
# release. The binaries must land in the data directory (<data>/bin/<version>
# plus stable links) and nothing may be written, created or removed under the
# plugin directory (the shell reloads every plugin on any write there). Also:
# a failed download/checksum keeps the previous install and leaves no litter,
# an upgrade swaps versions and removes the old one, and the Go build fallback
# keeps its caches in the staging directory.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
boot="${here}/bootstrap.sh"
work="$(mktemp -d)"
srv_pid=""
cleanup() { [ -z "${srv_pid}" ] || kill "${srv_pid}" 2>/dev/null || true; rm -rf "${work}"; }
trap cleanup EXIT

# Airtight against the real session: PATH starts with stubs for systemctl and
# the daemon binaries (they record the call and fail), and every run below gets
# a throwaway HOME and XDG_* tree including XDG_RUNTIME_DIR. The bootstrap calls
# ${data}/bin/qcyctl by absolute path, which is a fake in every case.
mkdir -p "${work}/stubbin"
for s in systemctl qcyctl qcyctld; do
  printf '#!/bin/sh\necho "%s $*" >> "%s/stub.called"\nexit 1\n' "${s}" "${work}" > "${work}/stubbin/${s}"
  chmod +x "${work}/stubbin/${s}"
done
export PATH="${work}/stubbin:${PATH}"
export XDG_RUNTIME_DIR="${work}/xdg-run" XDG_CONFIG_HOME="${work}/xdg-config" XDG_STATE_HOME="${work}/xdg-state"
mkdir -p "${XDG_RUNTIME_DIR}" "${XDG_CONFIG_HOME}" "${XDG_STATE_HOME}"

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

# A fresh copy of the repo (without .git) acts as the plugin directory.
new_plugin() { # name [version] -> plugin dir path
  mkdir -p "${work}/$1"
  ( cd "${here}/.." && tar -cf - --exclude=.git --exclude=build --exclude=dist . ) | tar -xf - -C "${work}/$1"
  [ -z "${2:-}" ] || sed -i "s/\"version\"[[:space:]]*:[[:space:]]*\"[^\"]*\"/\"version\": \"$2\"/" "${work}/$1/manifest.json"
  printf '%s\n' "${work}/$1"
}

home="${work}/home"
data="${home}/.local/share/qcy-omarchy"
run() { # plugin-dir base-url [env...] -> output in ${work}/out, status in $rc
  local p="$1" url="$2"; shift 2
  mkdir -p "${home}"
  set +e
  env HOME="${home}" XDG_DATA_HOME="${home}/.local/share" XDG_CACHE_HOME="${home}/.cache" \
    QCY_RELEASE_BASE_URL="${url}" QCY_SRC_DIR="${work}/nosrc" "$@" bash "${p}/scripts/bootstrap.sh" >"${work}/out" 2>&1
  rc=$?
  set -e
}
tree() { find "$1" -printf '%p %y %T@ %s %l\n' | sort; }
stages() { find "${data}" -maxdepth 1 \( -name '.stage.*' -o -name '.qcy-stage.*' \) 2>/dev/null | head -n 1; }
set_release() { # dir name  (rebuild the archive from ${work}/pkg and its sums)
  tar -czf "$1/$2" -C "${work}/pkg" qcyctl qcyctld
  printf '%s  %s\n' "$(sha256sum "$1/$2" | awk '{print $1}')" "$2" > "$1/SHA256SUMS"
}

# 1. wrong checksum: nothing installed, final ERROR, plugin dir untouched
p1="$(new_plugin p1)"; mkdir -p "${p1}/.bin"; echo legacy > "${p1}/.bin/qcyctl"
tree "${p1}" > "${work}/before1"
run "${p1}" "http://127.0.0.1:${port}/bad"
if [ "${rc}" -ne 0 ] && grep -q '^ERROR' "${work}/out" && [ ! -e "${data}/bin/qcyctl" ] \
   && [ "$(tree "${p1}")" = "$(cat "${work}/before1")" ]; then check "wrong checksum refused" ok; else check "wrong checksum refused" no; cat "${work}/out"; fi

# 2. correct checksum: installed in the data dir, plugin dir byte-for-byte
# untouched (a legacy .bin is ignored, not deleted), no staging left
p2="$(new_plugin p2)"; mkdir -p "${p2}/.bin"; echo legacy > "${p2}/.bin/qcyctl"
tree "${p2}" > "${work}/before2"
run "${p2}" "http://127.0.0.1:${port}/good"
if [ "${rc}" -eq 0 ] && [ -x "${data}/bin/${version}/qcyctl" ] && [ -x "${data}/bin/${version}/qcyctld" ] \
   && [ "$(cat "${data}/bin/${version}/plugin-dir")" = "${p2}" ] \
   && [ -L "${data}/bin/qcyctl" ] && [ -x "${data}/bin/qcyctl" ] && [ -x "${data}/bin/qcyctld" ] \
   && [ "$(readlink "${data}/bin/current")" = "${version}" ] && grep -q '^STEP done' "${work}/out" \
   && grep -qx "BIN ${data}/bin/qcyctl" "${work}/out"; then
  check "correct checksum installs into the data dir" ok; else check "correct checksum installs into the data dir" no; cat "${work}/out"; find "${data}"; fi
if [ "$(tree "${p2}")" = "$(cat "${work}/before2")" ] && [ -z "$(stages)" ] \
   && [ -z "$(find "${home}" -mindepth 1 -not -path "${data}" -not -path "${data}/*" -not -path "${home}/.local" -not -path "${home}/.local/share" 2>/dev/null)" ]; then
  check "nothing created, written or removed under the plugin dir; no staging left" ok
else check "nothing created, written or removed under the plugin dir; no staging left" no; diff <(cat "${work}/before2") <(tree "${p2}") || true; stages; fi

# 2b. a second run finds the install ready (no download, same files)
before_bin="$(tree "${data}/bin")"
run "${p2}" "http://127.0.0.1:${port}/bad"
# fake binaries cannot report a version, so this run re-installs from the bad
# release and must fail while keeping the previous install intact
if [ "${rc}" -ne 0 ] && grep -q '^ERROR' "${work}/out" && [ "$(tree "${data}/bin")" = "${before_bin}" ] && [ -z "$(stages)" ]; then
  check "failed checksum keeps the installed version and leaves no staging" ok
else check "failed checksum keeps the installed version and leaves no staging" no; cat "${work}/out"; fi

# 2c. an orphan staging directory from a killed run is cleaned up
mkdir -p "${data}/.stage.ORPHAN/bin"; : > "${data}/.stage.ORPHAN/bin/x"
run "${p2}" "http://127.0.0.1:${port}/bad"
if [ -z "$(stages)" ]; then check "orphan staging removed" ok; else check "orphan staging removed" no; stages; fi

# 2d. upgrade: a new plugin version installs next to the old one, `current`
# switches, the old directory is removed, the plugin dir stays untouched
up="9.9.9"
upname="qcy-omarchy_${up}_linux_${arch}.tar.gz"
mkdir -p "${work}/up"; printf 'more' >> "${work}/pkg/qcyctl"
set_release "${work}/up" "${upname}"
fake_elf "${work}/pkg/qcyctl"
pu="$(new_plugin pu "${up}")"
tree "${pu}" > "${work}/beforeu"
run "${pu}" "http://127.0.0.1:${port}/up"
if [ "${rc}" -eq 0 ] && [ "$(readlink "${data}/bin/current")" = "${up}" ] && [ -x "${data}/bin/${up}/qcyctl" ] \
   && [ ! -e "${data}/bin/${version}" ] && [ -z "$(stages)" ] \
   && [ "$(ls -A "${data}/bin" | LC_ALL=C sort | tr '\n' ' ')" = "${up} current qcyctl qcyctld " ] \
   && [ "$(tree "${pu}")" = "$(cat "${work}/beforeu")" ] && [ "$(cat "${data}/bin/${up}/plugin-dir")" = "${pu}" ]; then
  check "upgrade switches version and removes the old one" ok
else check "upgrade switches version and removes the old one" no; cat "${work}/out"; ls -la "${data}/bin"; fi

# 3. not an ELF of this machine: refused even with a matching checksum
printf '#!/bin/sh\necho pwn\n' > "${work}/pkg/qcyctl"
set_release "${work}/good" "${name}"
fake_elf "${work}/pkg/qcyctl"
p3="$(new_plugin p3)"
rm -rf "${home}"
run "${p3}" "http://127.0.0.1:${port}/good"
if [ "${rc}" -ne 0 ] && [ ! -e "${data}/bin/qcyctl" ] && [ -z "$(stages)" ]; then check "non-ELF refused" ok; else check "non-ELF refused" no; cat "${work}/out"; fi

# 4. read-only plugin dir still installs (nothing is written there anyway)
if [ "$(id -u)" -ne 0 ]; then
  set_release "${work}/good" "${name}"
  p4="$(new_plugin p4)"; chmod -R a-w "${p4}"
  rm -rf "${home}"
  run "${p4}" "http://127.0.0.1:${port}/good"
  chmod -R u+w "${p4}"
  if [ "${rc}" -eq 0 ] && [ -x "${data}/bin/qcyctl" ] && [ ! -e "${p4}/.bin" ]; then
    check "read-only plugin dir installs into the data dir" ok; else check "read-only plugin dir installs into the data dir" no; cat "${work}/out"; fi
fi

# 4b. a matching installed binary: ready path, and the plugin-dir record follows
# a plugin that moved (stale record would make the daemon exit every 30 s)
cur="$(readlink "${data}/bin/current")"
printf '#!/bin/sh\necho "qcyctl %s"\n' "${version}" > "${data}/bin/${cur}/qcyctl"; chmod +x "${data}/bin/${cur}/qcyctl"
p4b="$(new_plugin p4b)"
run "${p4b}" "http://127.0.0.1:${port}/bad"
if [ "${rc}" -eq 0 ] && grep -qx "BIN ${data}/bin/qcyctl" "${work}/out" && [ "$(cat "${data}/bin/${cur}/plugin-dir")" = "${p4b}" ]; then
  check "ready path rewrites plugin-dir for the new plugin path" ok
else check "ready path rewrites plugin-dir for the new plugin path" no; cat "${work}/out"; fi
p4c="$(new_plugin p4c)"
( cd "${p4c}" && env HOME="${home}" XDG_DATA_HOME="${home}/.local/share" bash scripts/bootstrap.sh --mark-plugin ) >"${work}/out" 2>&1
if [ "$(cat "${data}/bin/${cur}/plugin-dir")" = "${p4c}" ] && [ ! -s "${work}/out" ]; then
  check "--mark-plugin refreshes plugin-dir silently" ok; else check "--mark-plugin refreshes plugin-dir silently" no; cat "${work}/out"; fi

mkdir -p "${work}/mvbin"
real_mv="$(command -v mv)"
# 4c. same-version reinstall: a failing step keeps the previous install intact
rm -f "${data}/bin/${cur}/qcyctl"; fake_elf "${data}/bin/${cur}/qcyctl"; chmod +x "${data}/bin/${cur}/qcyctl"
cat > "${work}/mvbin/mv" <<ENDMV
#!/usr/bin/env bash
for a; do last="\$a"; done
case "\${last}" in *+r*) exit 1 ;; esac
exec "${real_mv}" "\$@"
ENDMV
chmod +x "${work}/mvbin/mv"
before_bin="$(tree "${data}/bin")"
run "${p4}" "http://127.0.0.1:${port}/good" PATH="${work}/mvbin:${PATH}"
if [ "${rc}" -ne 0 ] && [ "$(readlink "${data}/bin/current")" = "${cur}" ] && [ "$(tree "${data}/bin")" = "${before_bin}" ] && [ -z "$(stages)" ]; then
  check "failed commit on the same version leaves the install intact" ok
else check "failed commit on the same version leaves the install intact" no; cat "${work}/out"; fi
run "${p4}" "http://127.0.0.1:${port}/good"
nd="$(find "${data}/bin" -mindepth 1 -maxdepth 1 -type d | wc -l)"
if [ "${rc}" -eq 0 ] && [ "$(readlink "${data}/bin/current")" != "${cur}" ] && [ "${nd}" = 1 ] && [ -x "${data}/bin/qcyctl" ]; then
  check "same-version reinstall switches current and drops the old directory" ok
else check "same-version reinstall switches current and drops the old directory" no; cat "${work}/out"; ls -la "${data}/bin"; fi

# 4d. the previous version stays while a daemon still runs from it
cur="$(readlink "${data}/bin/current")"
cp "$(command -v sleep)" "${data}/bin/${cur}/qcyctld"
"${data}/bin/${cur}/qcyctld" 60 & sleeper=$!
sleep 0.3
run "${pu}" "http://127.0.0.1:${port}/up"
kill "${sleeper}" 2>/dev/null || true; wait "${sleeper}" 2>/dev/null || true
if [ "${rc}" -eq 0 ] && [ "$(readlink "${data}/bin/current")" = "${up}" ] && [ -d "${data}/bin/${cur}" ]; then
  check "previous version kept while a daemon runs from it" ok
else check "previous version kept while a daemon runs from it" no; cat "${work}/out"; ls -la "${data}/bin"; fi

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

# 4e. a plugin path with edge whitespace is not recorded (the daemon then never checks)
set_release "${work}/good" "${name}"
p6="$(new_plugin "p6 ")"
rm -rf "${home}"
run "${p6}" "http://127.0.0.1:${port}/good"
if [ "${rc}" -eq 0 ] && [ -x "${data}/bin/qcyctl" ] && [ ! -e "${data}/bin/$(readlink "${data}/bin/current")/plugin-dir" ]; then
  check "path with edge space: installed, plugin-dir not recorded" ok
else check "path with edge space: installed, plugin-dir not recorded" no; cat "${work}/out"; fi

# 4f. download failure announces the build fallback
run "${p6}" "http://127.0.0.1:${port}/none" PATH="${work}/fakebin:${PATH}" QCY_SRC_DIR="${work}/src"
if grep -q '^STEP build Download or checksum check failed' "${work}/out"; then
  check "fallback to build is announced" ok; else check "fallback to build is announced" no; cat "${work}/out"; fi

# 5. build fallback: a fake `go` shows where its caches go; they live in the
# staging directory (never the plugin dir or ~/.cache) and go with it
p5="$(new_plugin p5)"
rm -rf "${home}"
tree "${p5}" > "${work}/before5"
run "${p5}" "http://127.0.0.1:${port}/none" QCY_BUILD=1 QCY_SRC_DIR="${work}/src" PATH="${work}/fakebin:${PATH}"
gc="$(cat "${work}/gocache.path" 2>/dev/null || true)"

case "${gc}" in "${data}/.stage."*) in_stage=ok ;; *) in_stage=no ;; esac
if [ "${rc}" -eq 0 ] && [ -x "${data}/bin/qcyctld" ] && [ "${in_stage}" = ok ] && [ ! -e "${gc}" ] \
   && [ -z "$(stages)" ] && [ ! -e "${home}/.cache" ] && [ "$(tree "${p5}")" = "$(cat "${work}/before5")" ]; then
  check "build fallback keeps its build cache in the staging dir and removes it" ok
else check "build fallback keeps its build cache in the staging dir and removes it" no; cat "${work}/out"; echo "gocache=${gc}"; fi

# 6. the daemon hand-over: after a fresh install the new qcyctl is asked to
# `daemon refresh-service --quiet` (a real tiny stand-in records its arguments);
# when that fails it falls back to `daemon stop --quiet`; a failing qcyctl never
# fails the install and nothing it prints leaks into the STEP/BIN output.
if command -v go >/dev/null 2>&1; then
  mkdir -p "${work}/fakectl" "${work}/recpkg" "${work}/rec"
  cat > "${work}/fakectl/main.go" <<'ENDGO'
package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	if f := os.Getenv("QCY_FAKE_LOG"); f != "" {
		if fh, err := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			fmt.Fprintln(fh, strings.Join(os.Args[1:], " "))
			fh.Close()
		}
	}
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("qcyctl " + os.Getenv("QCY_FAKE_VERSION"))
		return
	}
	fmt.Println("STEP leaked from qcyctl")
	if os.Getenv("QCY_FAKE_FAIL") == "1" && len(os.Args) > 2 && os.Args[2] == "refresh-service" {
		os.Exit(1)
	}
	if os.Getenv("QCY_FAKE_FAIL") == "all" {
		os.Exit(1)
	}
}
ENDGO
  ( cd "${work}/fakectl" && printf 'module fakectl\n\ngo 1.21\n' > go.mod && env -u GOFLAGS GOWORK=off GOTOOLCHAIN=local GOCACHE="${work}/fakectl/cache" go build -o "${work}/recpkg/qcyctl" . ) >"${work}/fakectl.log" 2>&1 \
    && cp "${work}/recpkg/qcyctl" "${work}/recpkg/qcyctld" && set_release_from() { tar -czf "$1/$2" -C "${work}/recpkg" qcyctl qcyctld; printf '%s  %s\n' "$(sha256sum "$1/$2" | awk '{print $1}')" "$2" > "$1/SHA256SUMS"; } \
    && set_release_from "${work}/rec" "${name}" || { echo "could not build the stand-in qcyctl"; cat "${work}/fakectl.log"; fails=$((fails + 1)); }
  p7="$(new_plugin p7)"
  rm -rf "${home}"; : > "${work}/fake.log"
  run "${p7}" "http://127.0.0.1:${port}/rec" QCY_FAKE_LOG="${work}/fake.log" QCY_FAKE_VERSION="${version}"
  if [ "${rc}" -eq 0 ] && [ "$(grep -c . "${work}/fake.log")" = 1 ] && grep -qx 'daemon refresh-service --quiet' "${work}/fake.log" \
     && ! grep -v '^STEP \(check\|download\|verify\|install\|build\|done\) \|^BIN ' "${work}/out" | grep -q .; then
    check "hand-over runs refresh-service --quiet and prints nothing extra" ok
  else check "hand-over runs refresh-service --quiet and prints nothing extra" no; cat "${work}/out" "${work}/fake.log"; fi

  rm -rf "${home}"; : > "${work}/fake.log"
  run "${p7}" "http://127.0.0.1:${port}/rec" QCY_FAKE_LOG="${work}/fake.log" QCY_FAKE_VERSION="${version}" QCY_FAKE_FAIL=1
  if [ "${rc}" -eq 0 ] && [ -x "${data}/bin/qcyctl" ] && [ "$(grep 'refresh-service\|stop' "${work}/fake.log" | tr '\n' ';')" = "daemon refresh-service --quiet;daemon stop --quiet;" ]; then
    check "failed refresh-service falls back to daemon stop and does not fail the install" ok
  else check "failed refresh-service falls back to daemon stop and does not fail the install" no; cat "${work}/out" "${work}/fake.log"; fi

  rm -rf "${home}"; : > "${work}/fake.log"
  run "${p7}" "http://127.0.0.1:${port}/rec" QCY_FAKE_LOG="${work}/fake.log" QCY_FAKE_VERSION="${version}" QCY_FAKE_FAIL=all
  if [ "${rc}" -eq 0 ] && grep -qx "BIN ${data}/bin/qcyctl" "${work}/out"; then
    check "a qcyctl that fails every daemon command still leaves a successful install" ok
  else check "a qcyctl that fails every daemon command still leaves a successful install" no; cat "${work}/out"; fi
else
  echo "SKIP hand-over tests (no go to build the stand-in qcyctl)"
fi


if [ -e "${work}/stub.called" ]; then check "nothing reached the real systemctl or qcyctl" no; cat "${work}/stub.called"; else check "nothing reached the real systemctl or qcyctl" ok; fi

[ "${fails}" -eq 0 ] && echo "all passed" || { echo "${fails} failed"; exit 1; }
