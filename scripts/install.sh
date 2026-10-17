#!/usr/bin/env bash
# Build qcyctl + qcyctld, install them, enable the systemd user socket unit and
# install the Omarchy bar widget.
#
#   ./scripts/install.sh [--bin-dir DIR] [--plugin-dir DIR] [--no-build] [--no-service]
#
# Defaults:
#   binaries   -> ~/.local/bin/{qcyctl,qcyctld}
#   units      -> ~/.config/systemd/user/qcyctld.{socket,service} (socket enabled,
#                 written by `qcyctl daemon install-service`)
#   plugin     -> ~/.config/omarchy/plugins/io.github.bbztp.qcy
#
#   --no-build    reuse the binaries already in ./build (make build)
#   --no-service  skip the systemd user units
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
plugin_id="io.github.bbztp.qcy"
bin_dir="${HOME}/.local/bin"
plugin_dir="${HOME}/.config/omarchy/plugins/${plugin_id}"
build_dir="${repo_dir}/build"
build=1
service=1

while [ $# -gt 0 ]; do
  case "$1" in
    --bin-dir) bin_dir="${2:?--bin-dir needs a value}"; shift 2 ;;
    --plugin-dir) plugin_dir="${2:?--plugin-dir needs a value}"; shift 2 ;;
    --no-build) build=0; shift ;;
    --no-service) service=0; shift ;;
    -h|--help) sed -n '2,14p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

if [ "$build" = 1 ]; then
  command -v go >/dev/null 2>&1 || { echo "error: go is required to build (or use --no-build after 'make build')" >&2; exit 1; }
  mkdir -p "${build_dir}"
  # Version from manifest.json (no jq), injected into both binaries.
  ver="$(sed -n 's/^[[:space:]]*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${repo_dir}/manifest.json" | head -n 1)"
  ldflags="-X main.version=${ver:-dev}"
  echo "building qcyctl..."
  ( cd "${repo_dir}" && go build -ldflags "${ldflags}" -o "${build_dir}/qcyctl" ./cmd/qcyctl )
  echo "building qcyctld..."
  ( cd "${repo_dir}" && go build -ldflags "${ldflags}" -o "${build_dir}/qcyctld" ./cmd/qcyctld )
fi

for b in qcyctl qcyctld; do
  [ -x "${build_dir}/${b}" ] || { echo "error: ${build_dir}/${b} not found (run 'make build' or omit --no-build)" >&2; exit 1; }
done

# Stop a running daemon first so the binary can be replaced cleanly.
if [ "$service" = 1 ] && command -v systemctl >/dev/null 2>&1; then
  systemctl --user stop qcyctld.service 2>/dev/null || true
fi

mkdir -p "${bin_dir}"
for b in qcyctl qcyctld; do
  echo "installing ${b} -> ${bin_dir}/${b}"
  install -m 0755 "${build_dir}/${b}" "${bin_dir}/${b}"
done

if [ "$service" = 1 ]; then
  # One source of unit text: the freshly installed qcyctl writes qcyctld.socket
  # and qcyctld.service (no [Install] section; the socket activates the daemon).
  echo "enabling the daemon socket..."
  if ! "${bin_dir}/qcyctl" daemon install-service; then
    echo "warning: no usable 'systemctl --user' session (e.g. over SSH); the daemon starts on demand without it." >&2
    echo "         enable the socket later from a desktop session with:" >&2
    echo "           ${bin_dir}/qcyctl daemon install-service" >&2
  fi
else
  echo "skipping systemd unit (--no-service); run ${bin_dir}/qcyctld yourself"
fi

echo "installing plugin -> ${plugin_dir}"
# Update in place and only what changed: the running shell watches this
# directory and hot-reloads the plugin on every write, and removing then
# recreating the whole tree made it reload against a half-copied plugin (the
# shell has crashed during such reloads). Nothing changed -> nothing written.
# The plugin lives at the repository root; only its own files are installed
# (plus scripts/bootstrap.sh, which prepares the binaries, and SHA256SUMS, the
# checksums it pins; never the Go
# sources, docs or .git).
plugin_files() {
  find "${repo_dir}" -maxdepth 1 -type f \( -name manifest.json -o -name qmldir -o -name '*.qml' \) -print0
  find "${repo_dir}/assets" -type f -print0
  printf '%s\0' "${repo_dir}/scripts/bootstrap.sh"
  # The pinned release checksums bootstrap.sh verifies the download against.
  [ ! -f "${repo_dir}/SHA256SUMS" ] || printf '%s\0' "${repo_dir}/SHA256SUMS"
}
fresh=0
[ -d "${plugin_dir}" ] || fresh=1
mkdir -p "${plugin_dir}"
changed=0
while IFS= read -r -d '' f; do
  rel="${f#"${repo_dir}/"}"
  if ! cmp -s "$f" "${plugin_dir}/${rel}"; then
    mkdir -p "$(dirname "${plugin_dir}/${rel}")"
    cp "$f" "${plugin_dir}/${rel}.tmp.$$" && mv -f "${plugin_dir}/${rel}.tmp.$$" "${plugin_dir}/${rel}"
    changed=1
  fi
done < <(plugin_files)
# files that no longer exist in the repo
while IFS= read -r -d '' f; do
  rel="${f#"${plugin_dir}/"}"
  if [ ! -e "${repo_dir}/${rel}" ]; then
    rm -f "$f"
    changed=1
  fi
done < <(find "${plugin_dir}" -type f -print0)
[ "$changed" = 1 ] || echo "plugin already up to date"

if [ "$fresh" = 1 ] && command -v omarchy-shell >/dev/null 2>&1; then
  omarchy-shell -q shell rescanPlugins || true
fi

cat <<MSG

Done. Add the widget to your bar by placing this entry in ~/.config/omarchy/shell.json
inside bar.layout (for example the "right" array):

  { "id": "${plugin_id}" }

shell.json hot-reloads on save.

After updating the plugin, run: omarchy restart shell
(the shell's hot-reload can keep serving the previous BarWidget.qml.)
MSG
