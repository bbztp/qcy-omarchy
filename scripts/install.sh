#!/usr/bin/env bash
# Build the qcyctl backend and install the Omarchy bar widget.
#
#   ./scripts/install.sh [--bin-dir DIR] [--plugin-dir DIR] [--no-build]
#
# Defaults:
#   binary     -> ~/.local/bin/qcyctl
#   plugin     -> ~/.config/omarchy/plugins/io.github.bbztp.qcy
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
plugin_id="io.github.bbztp.qcy"
bin_dir="${HOME}/.local/bin"
plugin_dir="${HOME}/.config/omarchy/plugins/${plugin_id}"
build=1

while [ $# -gt 0 ]; do
  case "$1" in
    --bin-dir) bin_dir="$2"; shift 2 ;;
    --plugin-dir) plugin_dir="$2"; shift 2 ;;
    --no-build) build=0; shift ;;
    -h|--help) sed -n '2,9p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

if [ "$build" = 1 ]; then
  command -v go >/dev/null 2>&1 || { echo "error: go is required to build qcyctl" >&2; exit 1; }
  echo "building qcyctl…"
  ( cd "${repo_dir}/backend" && go build -o qcyctl . )
fi

[ -x "${repo_dir}/backend/qcyctl" ] || { echo "error: ${repo_dir}/backend/qcyctl not found (run without --no-build)" >&2; exit 1; }

echo "installing qcyctl -> ${bin_dir}/qcyctl"
mkdir -p "${bin_dir}"
install -m 0755 "${repo_dir}/backend/qcyctl" "${bin_dir}/qcyctl"

echo "installing plugin -> ${plugin_dir}"
rm -rf "${plugin_dir}"
mkdir -p "${plugin_dir}"
cp -r "${repo_dir}/plugin/${plugin_id}/." "${plugin_dir}/"

if command -v omarchy-shell >/dev/null 2>&1; then
  omarchy-shell -q shell rescanPlugins || true
fi

cat <<EOF

Done. Add the widget to your bar by placing this entry in ~/.config/omarchy/shell.json
inside bar.layout (for example the "right" array):

  { "id": "${plugin_id}" }

shell.json hot-reloads on save; otherwise run: omarchy restart shell
EOF
