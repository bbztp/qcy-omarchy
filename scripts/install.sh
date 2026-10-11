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
  echo "building qcyctl..."
  ( cd "${repo_dir}/backend" && go build -o "${build_dir}/qcyctl" . )
  echo "building qcyctld..."
  ( cd "${repo_dir}/backend" && go build -o "${build_dir}/qcyctld" ./cmd/qcyctld )
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
rm -rf "${plugin_dir}"
mkdir -p "${plugin_dir}"
cp -r "${repo_dir}/plugin/${plugin_id}/." "${plugin_dir}/"

if command -v omarchy-shell >/dev/null 2>&1; then
  omarchy-shell -q shell rescanPlugins || true
fi

cat <<MSG

Done. Add the widget to your bar by placing this entry in ~/.config/omarchy/shell.json
inside bar.layout (for example the "right" array):

  { "id": "${plugin_id}" }

shell.json hot-reloads on save; otherwise run: omarchy restart shell
MSG
