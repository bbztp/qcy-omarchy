#!/usr/bin/env bash
# Build qcyctl + qcyctld, install them, the systemd user unit and the Omarchy
# bar widget.
#
#   ./scripts/install.sh [--bin-dir DIR] [--plugin-dir DIR] [--no-build] [--no-service]
#
# Defaults:
#   binaries   -> ~/.local/bin/{qcyctl,qcyctld}
#   unit       -> ~/.config/systemd/user/qcyctld.service (enabled and started)
#   plugin     -> ~/.config/omarchy/plugins/io.github.bbztp.qcy
#
#   --no-build    reuse the binaries already in ./build (make build)
#   --no-service  skip installing/enabling the systemd user unit
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
plugin_id="io.github.bbztp.qcy"
bin_dir="${HOME}/.local/bin"
plugin_dir="${HOME}/.config/omarchy/plugins/${plugin_id}"
unit_dir="${HOME}/.config/systemd/user"
build_dir="${repo_dir}/build"
build=1
service=1

while [ $# -gt 0 ]; do
  case "$1" in
    --bin-dir) bin_dir="${2:?--bin-dir needs a value}"; shift 2 ;;
    --plugin-dir) plugin_dir="${2:?--plugin-dir needs a value}"; shift 2 ;;
    --no-build) build=0; shift ;;
    --no-service) service=0; shift ;;
    -h|--help) sed -n '2,13p' "$0"; exit 0 ;;
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
  command -v systemctl >/dev/null 2>&1 || { echo "error: systemctl not found (use --no-service)" >&2; exit 1; }
  echo "installing unit -> ${unit_dir}/qcyctld.service"
  mkdir -p "${unit_dir}"
  # Quote the path for systemd: wrap in double quotes, escape \ and ", and
  # double % (specifier) and $ (variable expansion). No sed: --bin-dir may hold
  # any character.
  exec_path="${bin_dir}/qcyctld"
  exec_path="${exec_path//\\/\\\\}"
  exec_path="${exec_path//\"/\\\"}"
  exec_path="${exec_path//%/%%}"
  exec_path="${exec_path//\$/\$\$}"
  # Copy the template line by line, replacing only ExecStart.
  : > "${unit_dir}/qcyctld.service"
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      ExecStart=*) printf 'ExecStart="%s"\n' "$exec_path" ;;
      *) printf '%s\n' "$line" ;;
    esac
  done < "${repo_dir}/packaging/qcyctld.service" >> "${unit_dir}/qcyctld.service"
  if systemctl --user daemon-reload 2>/dev/null \
     && systemctl --user enable --now qcyctld.service 2>/dev/null; then
    echo "qcyctld.service enabled and started"
  else
    echo "warning: no usable 'systemctl --user' session (e.g. over SSH); unit installed but not started." >&2
    echo "         activate it later from a desktop session with:" >&2
    echo "           systemctl --user daemon-reload && systemctl --user enable --now qcyctld.service" >&2
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
# (plus scripts/bootstrap.sh, which prepares the binaries; never the Go
# sources, docs or .git).
plugin_files() {
  find "${repo_dir}" -maxdepth 1 -type f \( -name manifest.json -o -name qmldir -o -name '*.qml' \) -print0
  find "${repo_dir}/assets" -type f -print0
  printf '%s\0' "${repo_dir}/scripts/bootstrap.sh"
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
