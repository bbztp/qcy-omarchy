#!/usr/bin/env bash
# Build one release tarball: scripts/package-release.sh <version> <amd64|arm64> [out-dir]
#
# Produces <out-dir>/qcy-omarchy_<version>_linux_<arch>.tar.gz holding exactly
# `qcyctl` and `qcyctld` at the root (mode 0755, no directories) -- the layout
# scripts/bootstrap.sh downloads and verifies. Binaries are static
# (CGO_ENABLED=0), built with -trimpath and the version baked in.
# SHA256SUMS is not written here: scripts/release-sums.sh generates the committed
# one, and the release workflow compares its own build against it.
set -euo pipefail

version="${1:-}"; arch="${2:-}"; out="${3:-dist}"
case "${arch}" in amd64|arm64) ;; *) echo "usage: $0 <version> <amd64|arm64> [out-dir]" >&2; exit 2 ;; esac
case "${version}" in
  ""|*[!0-9A-Za-z.+-]*) echo "invalid version: '${version}'" >&2; exit 2 ;;
esac

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mkdir -p "${out}"
out="$(cd "${out}" && pwd)"
stage="$(mktemp -d)"
trap 'rm -rf "${stage}"' EXIT

# Reproducible: the binaries depend only on the source files, go.mod/go.sum, the
# toolchain and the version. Ambient Go settings are cleared, the instruction-set
# levels are pinned, and -buildvcs=false keeps the git revision and dirty flag
# out of the binaries (they would make the committed SHA256SUMS circular).
( cd "${repo}" && env -u GOFLAGS -u GOEXPERIMENT -u GOFIPS140 \
    GOWORK=off GOAMD64=v1 GOARM64=v8.0 GOTOOLCHAIN=local \
    CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" \
    go build -buildvcs=false -trimpath -ldflags "-s -w -X main.version=${version}" \
    -o "${stage}/" ./cmd/qcyctl ./cmd/qcyctld )
chmod 0755 "${stage}/qcyctl" "${stage}/qcyctld"

name="qcy-omarchy_${version}_linux_${arch}.tar.gz"
tar --sort=name --owner=0 --group=0 --numeric-owner --mtime='2020-01-01 00:00:00 UTC' \
    -cf - -C "${stage}" qcyctl qcyctld | gzip -n -9 > "${out}/${name}"
echo "${out}/${name}"
