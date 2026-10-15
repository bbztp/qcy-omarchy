#!/usr/bin/env bash
# Prepare qcyctl + qcyctld for the plugin: download the release matching the
# plugin version (checksum verified), or build from source with Go.
#
# Binaries go to ${XDG_DATA_HOME:-~/.local/share}/qcy-omarchy/bin, never into
# the plugin directory. The shell watches ~/.config/omarchy/plugins recursively
# and reloads every plugin on any write below it, so this script (like the
# plugins that ship binaries) prepares everything outside that tree and leaves
# the checkout untouched. The Go fallback keeps GOCACHE and GOTMPDIR in the
# staging directory; the module cache is the user's normal one (outside the tree).
#
# `bootstrap.sh --mark-plugin` only refreshes the plugin-dir record.
#
# Layout under <data>/bin:
#   <version>/{qcyctl,qcyctld,plugin-dir}   one directory per installed version
#   current -> <version>                    switched by one atomic rename
#   qcyctl, qcyctld -> current/<name>       stable paths (the daemon search order)
# plugin-dir records the plugin directory that installed this version; the
# daemon exits once that directory is gone (`omarchy plugin remove` has no
# hook). Download, checksum, extraction, build caches and the lock all live in
# <data>/.stage.XXXXXX, on the same filesystem. Old versions are removed except
# the previous one while a daemon still runs from it. A legacy <plugin>/.bin is
# ignored (deleting it at runtime would reload the shell; remove it by hand).
#
# Progress is one line per step on stdout:
#   STEP <key> <text>      keys: check, download, verify, install, build, done
#   BIN <path>             the qcyctl that is ready (before the final done)
#   ERROR <text>           final failure (exit status is non-zero)
#
# Overrides (tests, mirrors):
#   QCY_RELEASE_BASE_URL  directory URL holding the tarball and SHA256SUMS
#   QCY_DATA_DIR          replaces ${XDG_DATA_HOME}/qcy-omarchy
#   QCY_SRC_DIR           source tree for the build fallback (default: repo root)
#   QCY_BUILD=1           skip the download and build from source
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd "${script_dir}/.." && pwd)"
src_dir="${QCY_SRC_DIR:-${repo_dir}}"

step() { printf 'STEP %s %s\n' "$1" "$2"; }
fail() { printf 'ERROR %s\n' "$1"; exit 1; }

manifest="${repo_dir}/manifest.json"
[ -r "${manifest}" ] || fail "manifest.json not found next to the plugin"
version="$(sed -n 's/^[[:space:]]*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${manifest}" | head -n 1)"
case "${version}" in
  ""|-*|*..*|*[!0-9A-Za-z.+-]*) fail "could not read a valid version from manifest.json" ;;
esac

data_dir="${QCY_DATA_DIR:-${XDG_DATA_HOME:-${HOME}/.local/share}/qcy-omarchy}"
bin_root="${data_dir}/bin"
mkdir -p "${bin_root}" || fail "could not create ${bin_root}"
chmod 0700 "${data_dir}" 2>/dev/null || true
stage_root="${data_dir}"

case "$(uname -m)" in
  x86_64|amd64) arch=amd64; elf_machine="3e00" ;;
  aarch64|arm64) arch=arm64; elf_machine="b700" ;;
  *) fail "unsupported architecture: $(uname -m)" ;;
esac

# One bootstrap at a time (a second waits and then finds everything ready):
# the lock is on the staging parent directory itself, so no file is created.
if command -v flock >/dev/null 2>&1; then
  exec 9<"${stage_root}"
  flock -w 300 9 || fail "another preparation is still running"
fi

ready() {
  [ -x "${bin_root}/qcyctl" ] && [ -x "${bin_root}/qcyctld" ] || return 1
  [ "$("${bin_root}/qcyctl" version 2>/dev/null | head -n 1)" = "qcyctl ${version}" ]
}

# Record which plugin directory owns the current version (data dir only).
# The daemon reads plugin-dir verbatim (only the final newline is dropped), so a
# path with edge whitespace or a newline is not recorded: then nothing is checked.
plugin_dir_recordable() {
  case "${repo_dir}" in
    ""|" "*|*" "|*$'\t'|$'\t'*|*$'\n'*) return 1 ;;
  esac
}

mark_plugin() { # dir
  local cur
  plugin_dir_recordable || return 0
  cur="$(cat "$1/plugin-dir" 2>/dev/null || true)"
  [ "${cur}" = "${repo_dir}" ] && return 0
  printf '%s\n' "${repo_dir}" > "$1/.plugin-dir.$$" && mv -f "$1/.plugin-dir.$$" "$1/plugin-dir"
}

# Light mode for the plugin when a matching binary was found elsewhere:
# refresh the plugin-dir record of the installed version, nothing else.
if [ "${1:-}" = "--mark-plugin" ]; then
  if [ -L "${bin_root}/current" ]; then
    mark_plugin "$(cd "${bin_root}/current" && pwd -P)" 2>/dev/null || true
  fi
  exit 0
fi

step check "Checking the installed version"
if [ "${QCY_BUILD:-0}" != 1 ] && ready; then
  mark_plugin "$(cd "${bin_root}/current" && pwd -P)" 2>/dev/null || true
  printf 'BIN %s\n' "${bin_root}/qcyctl"
  step done "Ready (${version})"
  exit 0
fi

# Staging directories left by an interrupted run (we hold the lock).
for d in "${stage_root}"/.stage.* "${stage_root}"/.qcy-stage.*; do
  [ -d "${d}" ] && [ ! -L "${d}" ] && rm -rf "${d}"
done
tmp="$(mktemp -d "${stage_root}/.stage.XXXXXX")"
chmod 0700 "${tmp}"
stage_bin="${tmp}/bin"
mkdir -p "${stage_bin}"
trap 'rm -rf "${tmp}"' EXIT

is_elf() { # file: ELF magic + the machine of this architecture
  local h
  h="$(od -An -tx1 -N20 "$1" 2>/dev/null | tr -d ' \n')" || return 1
  [ "${h:0:8}" = "7f454c46" ] && [ "${h:36:4}" = "${elf_machine}" ]
}

install_from() { # dir containing qcyctl and qcyctld
  local f
  step install "Installing"
  for f in qcyctl qcyctld; do
    [ -f "$1/${f}" ] && [ ! -L "$1/${f}" ] || return 1
    is_elf "$1/${f}" || return 1
  done
  for f in qcyctl qcyctld; do
    cp "$1/${f}" "${stage_bin}/${f}"
    chmod 0755 "${stage_bin}/${f}"
  done
  if plugin_dir_recordable; then printf '%s\n' "${repo_dir}" > "${stage_bin}/plugin-dir"; fi
}

# Point a stable name at a target with one rename (atomic replace).
relink() { # target name
  rm -f "${bin_root}/.link.$$"
  ln -s "$1" "${bin_root}/.link.$$" && mv -T "${bin_root}/.link.$$" "${bin_root}/$2"
}

# True when a running process executes a binary inside directory $1.
in_use() {
  local e
  for e in /proc/[0-9]*/exe; do
    case "$(readlink "${e}" 2>/dev/null)" in
      "$1"/*) return 0 ;;
    esac
  done
  return 1
}

# stage_bin becomes <data>/bin/<dir>; `current` then moves with one rename.
# A reinstall of the same version gets a fresh directory name, so `current`
# never points at a missing or half-replaced directory and a failure at any
# step leaves the previous install untouched.
commit() {
  local prev="" dir="${version}"
  if [ -L "${bin_root}/current" ]; then
    prev="$(readlink "${bin_root}/current")"
  fi
  if [ -e "${bin_root}/${dir}" ]; then
    dir="${version}+r$$"
    rm -rf "${bin_root}/${dir}"
  fi
  mv "${stage_bin}" "${bin_root}/${dir}" || return 1
  relink "${dir}" current || { rm -rf "${bin_root}/${dir}"; return 1; }
  [ -L "${bin_root}/qcyctl" ] && [ "$(readlink "${bin_root}/qcyctl")" = "current/qcyctl" ] \
    || relink current/qcyctl qcyctl || return 1
  [ -L "${bin_root}/qcyctld" ] && [ "$(readlink "${bin_root}/qcyctld")" = "current/qcyctld" ] \
    || relink current/qcyctld qcyctld || return 1
  # Old versions go; the previous one stays while a daemon still runs from it.
  local d name
  for d in "${bin_root}"/*/; do
    d="${d%/}"; name="${d##*/}"
    [ -d "${d}" ] && [ ! -L "${d}" ] || continue
    [ "${name}" != "${dir}" ] || continue
    if [ "${name}" = "${prev}" ] && in_use "${d}"; then continue; fi
    rm -rf "${d}"
  done
  rm -f "${bin_root}/.version" "${bin_root}"/.link.* 2>/dev/null || true
}

download() {
  local base="${QCY_RELEASE_BASE_URL:-https://github.com/bbztp/qcy-omarchy/releases/download/v${version}}"
  local name="qcy-omarchy_${version}_linux_${arch}.tar.gz"
  local proto="=https" eff host want got dl="${tmp}/dl" ex="${tmp}/ex"
  command -v curl >/dev/null 2>&1 || return 1
  [ -z "${QCY_RELEASE_BASE_URL:-}" ] || proto="=http,https"
  mkdir -p "${dl}" "${ex}"
  step download "Downloading ${name}"
  eff="$(curl -fsSL --proto "${proto}" --proto-redir "${proto}" --connect-timeout 10 --max-time 180 \
      -o "${dl}/${name}" -w '%{url_effective}' "${base}/${name}")" || return 1
  curl -fsSL --proto "${proto}" --proto-redir "${proto}" --connect-timeout 10 --max-time 60 \
      -o "${dl}/SHA256SUMS" "${base}/SHA256SUMS" || return 1
  if [ -z "${QCY_RELEASE_BASE_URL:-}" ]; then
    host="${eff#https://}"; host="${host%%/*}"
    case "${host}" in
      github.com|*.githubusercontent.com) ;;
      *) return 1 ;;
    esac
  fi
  step verify "Verifying the checksum"
  want="$(awk -v n="${name}" '$2 == n || $2 == "*" n { print $1; exit }' "${dl}/SHA256SUMS")"
  [ -n "${want}" ] || return 1
  got="$(sha256sum "${dl}/${name}" | awk '{print $1}')"
  [ "${want}" = "${got}" ] || return 1
  # Only the two expected names, as plain relative entries.
  local entry n=0
  while IFS= read -r entry; do
    case "${entry#./}" in
      qcyctl|qcyctld) n=$((n + 1)) ;;
    esac
  done < <(tar -tzf "${dl}/${name}")
  [ "${n}" -ge 2 ] || return 1
  tar -xzf "${dl}/${name}" -C "${ex}" --no-same-owner --no-same-permissions qcyctl qcyctld 2>/dev/null \
    || tar -xzf "${dl}/${name}" -C "${ex}" --no-same-owner --no-same-permissions ./qcyctl ./qcyctld || return 1
  install_from "${ex}"
}

build() {
  local out="${tmp}/build" cache="${tmp}/go-build"
  [ -d "${src_dir}/cmd/qcyctl" ] || fail "no release available and no source here; install Go and clone the repository to build"
  command -v go >/dev/null 2>&1 || fail "no release available; install Go (pacman -S go) and try again"
  mkdir -p "${out}" "${cache}"
  step build "Building from source (this takes a minute)"
  ( cd "${src_dir}" && env -u GOFLAGS GOWORK=off GOTOOLCHAIN=local GOCACHE="${cache}" GOTMPDIR="${tmp}" go build -buildvcs=false -trimpath -ldflags "-X main.version=${version}" \
      -o "${out}/" ./cmd/qcyctl ./cmd/qcyctld ) >"${tmp}/build.log" 2>&1 \
    || fail "build failed: $(tail -n 1 "${tmp}/build.log")"
  install_from "${out}" || fail "the built binaries were rejected"
}

done_ok=0
if [ "${QCY_BUILD:-0}" != 1 ]; then
  if download; then done_ok=1; fi
fi
if [ "${done_ok}" = 0 ]; then
  if [ "${QCY_BUILD:-0}" != 1 ]; then
    step build "Download or checksum check failed; falling back to a local build"
  fi
  build
fi
commit || fail "could not install into ${bin_root}"

# Hand the daemon over to the new binaries, after the old-version cleanup above.
# `daemon refresh-service` decides: first install or enabled socket -> install
# and restart; socket disabled by the user -> keep it off, refresh the units and
# stop the daemon so the next client starts the new binary. If it fails, the
# running daemon is stopped. Best effort: it never fails the install, and nothing
# it prints reaches the STEP/BIN protocol.
hand_over_daemon() {
  local q="${bin_root}/qcyctl" t=""
  command -v timeout >/dev/null 2>&1 && t="timeout 60"
  { ${t} "${q}" daemon refresh-service --quiet || ${t} "${q}" daemon stop --quiet; } >/dev/null 2>&1 || true
}
hand_over_daemon
printf 'BIN %s\n' "${bin_root}/qcyctl"
step done "Ready (${version})"
