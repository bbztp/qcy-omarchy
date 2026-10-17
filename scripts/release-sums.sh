#!/usr/bin/env bash
# Regenerate the root SHA256SUMS for the version in manifest.json (maintainer,
# run before tagging; `make release-sums` is a thin wrapper).
#
# scripts/bootstrap.sh verifies the downloaded binaries against this file, which
# is committed with the reviewed source, and the release workflow refuses to
# publish binaries that do not reproduce it. The tarballs are built by
# scripts/package-release.sh with the exact toolchain named by the `go` line of
# go.mod, so the hashes match what the workflow builds. Commit SHA256SUMS together
# with the version bump and the CHANGELOG section.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo}"

version="$(sed -n 's/^[[:space:]]*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' manifest.json | head -n 1)"
case "${version}" in
  ""|-*|*..*|*[!0-9A-Za-z.+-]*) echo "could not read a valid version from manifest.json" >&2; exit 1 ;;
esac

if ! grep -q "^## \[${version}\]" CHANGELOG.md; then
  echo "CHANGELOG.md has no section for [${version}]; write it first" >&2
  exit 1
fi

want_go="$(awk '$1 == "go" { print $2; exit }' go.mod)"
[ -n "${want_go}" ] || { echo "no go line in go.mod" >&2; exit 1; }
have_go="$(env -u GOFLAGS GOTOOLCHAIN=local go env GOVERSION 2>/dev/null || true)"
echo "toolchain: $(env -u GOFLAGS GOTOOLCHAIN=local go version 2>&1 || true)"
if [ "${have_go}" != "go${want_go}" ]; then
  echo "toolchain mismatch: go.mod wants go${want_go}, found '${have_go}'; install exactly go${want_go}" >&2
  exit 1
fi

out="$(mktemp -d)"
trap 'rm -rf "${out}"' EXIT
for arch in amd64 arm64; do
  bash scripts/package-release.sh "${version}" "${arch}" "${out}" >/dev/null
done
( cd "${out}" && sha256sum *.tar.gz ) > "${repo}/SHA256SUMS.new"
mv -f "${repo}/SHA256SUMS.new" "${repo}/SHA256SUMS"
echo "wrote SHA256SUMS for ${version}:"
cat "${repo}/SHA256SUMS"
