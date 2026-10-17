#!/usr/bin/env bash
# Reproducibility check (maintainer, cheap): the release tarball must not depend
# on git state. Builds the amd64 tarball from (a) this working tree, (b) a copy
# without .git and (c) a copy of the repository with a modified tracked file and
# an untracked file, and requires the same sha256 for all three. Part of what lets
# a SHA256SUMS committed with the source describe binaries built from that source.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="$(sed -n 's/^[[:space:]]*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${repo}/manifest.json" | head -n 1)"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

copy() { # dest [--keep-git]
  mkdir -p "$1"
  if [ "${2:-}" = --keep-git ]; then
    ( cd "${repo}" && tar -cf - --exclude=build --exclude=dist . ) | tar -xf - -C "$1"
  else
    ( cd "${repo}" && tar -cf - --exclude=.git --exclude=build --exclude=dist . ) | tar -xf - -C "$1"
  fi
}
sum_of() { # tree
  local t
  t="$(bash "$1/scripts/package-release.sh" "${version}" amd64 "${work}/out-$(basename "$1")")"
  sha256sum "${t}" | awk '{print $1}'
}

copy "${work}/nogit"
copy "${work}/dirty" --keep-git
printf '\n<!-- dirty -->\n' >> "${work}/dirty/README.md"
printf 'scratch\n' > "${work}/dirty/untracked-file"

a="$(sum_of "${repo}")"; b="$(sum_of "${work}/nogit")"; c="$(sum_of "${work}/dirty")"
printf 'working tree  %s\nno .git copy  %s\ndirty copy    %s\n' "${a}" "${b}" "${c}"
if [ "${a}" = "${b}" ] && [ "${a}" = "${c}" ]; then echo "reproducible: identical"; else echo "NOT reproducible" >&2; exit 1; fi
