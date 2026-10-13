#!/usr/bin/env bash
# Prepare qcyctl + qcyctld for the plugin: download the release matching the
# plugin version (checksum verified), or build from source with Go.
#
# Binaries go to ${XDG_DATA_HOME:-~/.local/share}/qcy-omarchy/bin, never into
# the plugin directory (the shell reloads the plugin on every write there).
#
# Progress is one line per step on stdout:
#   STEP <key> <text>      keys: check, download, verify, install, build, done
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
  ""|*[!0-9A-Za-z.+-]*) fail "could not read a valid version from manifest.json" ;;
esac

data_dir="${QCY_DATA_DIR:-${XDG_DATA_HOME:-${HOME}/.local/share}/qcy-omarchy}"
bin_dir="${data_dir}/bin"
cache_home="${XDG_CACHE_HOME:-${HOME}/.cache}/qcy-omarchy"

case "$(uname -m)" in
  x86_64|amd64) arch=amd64; elf_machine="3e00" ;;
  aarch64|arm64) arch=arm64; elf_machine="b700" ;;
  *) fail "unsupported architecture: $(uname -m)" ;;
esac

mkdir -p "${bin_dir}"
chmod 0700 "${data_dir}" 2>/dev/null || true

# One bootstrap at a time; a second waits and then finds everything ready.
if command -v flock >/dev/null 2>&1; then
  exec 9>"${data_dir}/.bootstrap.lock"
  flock -w 300 9 || fail "another preparation is still running"
fi

ready() {
  [ -x "${bin_dir}/qcyctl" ] && [ -x "${bin_dir}/qcyctld" ] || return 1
  [ "$("${bin_dir}/qcyctl" version 2>/dev/null | head -n 1)" = "qcyctl ${version}" ]
}

step check "Checking the installed version"
if [ "${QCY_BUILD:-0}" != 1 ] && ready; then
  step done "Ready (${version})"
  exit 0
fi

tmp="$(mktemp -d "${data_dir}/.bootstrap.XXXXXX")"
chmod 0700 "${tmp}"
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
    cp "$1/${f}" "${bin_dir}/.${f}.new"
    chmod 0755 "${bin_dir}/.${f}.new"
    mv -f "${bin_dir}/.${f}.new" "${bin_dir}/${f}"
  done
  printf '%s\n' "${version}" > "${bin_dir}/.version.new"
  mv -f "${bin_dir}/.version.new" "${bin_dir}/.version"
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
  local out="${tmp}/build" cache="${cache_home}/go-build"
  [ -d "${src_dir}/cmd/qcyctl" ] || fail "no release available and no source here; install Go and clone the repository to build"
  command -v go >/dev/null 2>&1 || fail "no release available; install Go (pacman -S go) and try again"
  mkdir -p "${out}" "${cache}"
  step build "Building from source (this takes a minute)"
  ( cd "${src_dir}" && GOCACHE="${cache}" go build -trimpath -ldflags "-X main.version=${version}" \
      -o "${out}/" ./cmd/qcyctl ./cmd/qcyctld ) >"${tmp}/build.log" 2>&1 \
    || fail "build failed: $(tail -n 1 "${tmp}/build.log")"
  install_from "${out}" || fail "the built binaries were rejected"
}

done_ok=0
if [ "${QCY_BUILD:-0}" != 1 ]; then
  if download; then done_ok=1; fi
fi
if [ "${done_ok}" = 0 ]; then
  build
fi
step done "Ready (${version})"
