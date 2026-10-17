# Publishing guide (maintainer)

Nothing here is automated except the release build and its checks. Do the steps in order.

## 1. Make the repository public

Scan the whole history first; once public, every old commit is visible.

```sh
cd qcy-omarchy
# Secrets and tokens
git log -p --all | grep -niE 'password|passwd|secret|token|api[_-]?key|BEGIN [A-Z ]*PRIVATE KEY|ghp_|github_pat_' | head -50
# Bluetooth MAC addresses (real device addresses must not be published)
git log -p --all | grep -noE '([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}' | sort | uniq -c | sort -rn | head -50
# Local paths and the author's machine
git log -p --all | grep -niE '/home/[a-z]+|/Users/[a-z]+|\.local/share/[a-z]+-ref' | grep -v 'qcy-omarchy' | head -50
# Terms that must not appear (bracketed so this file does not match itself)
git log -p --all | grep -niE 'a[p]k|decompi[l]|jad[x]|CMDID[_]|com\.qcym[a]ll|\.jav[a]' | head -50
# The same on the current tree
git grep -niE 'a[p]k|decompi[l]|jad[x]|CMDID[_]|com\.qcym[a]ll|\.jav[a]|([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}' -- . ':!internal/qcycore/models/models.json'
# Large or odd files that ever existed
git rev-list --objects --all | git cat-file --batch-check='%(objecttype) %(objectname) %(objectsize) %(rest)' | awk '$1=="blob" && $3>1000000'
```

Review every hit by hand (`img.qcymall.com` image URLs are expected). If
anything real shows up, rewrite history (`git filter-repo`) before publishing,
or publish from a fresh repository with a squashed history.

Checklist:

- [ ] Scans above are clean (or every hit explained)
- [ ] `LICENSE` present, `manifest.json` `license` matches
- [ ] `go test -count=1 ./...` and `bash scripts/bootstrap_test.sh` pass locally; CI is green on `main`
- [ ] `CHANGELOG.md` has a section for the version in `manifest.json`
- [ ] Capture `preview.png` (panel only, earbuds connected) and commit it at the repository root
- [ ] GitHub: Settings > General > Change visibility > Public

## 2. Tag and release

The downloaded binaries are bound to the reviewed source: `SHA256SUMS` at the
repository root pins the SHA-256 of the two tarballs of the `manifest.json`
version, and `scripts/bootstrap.sh` verifies against that file only (never
against the `SHA256SUMS` published with the release). It is generated, not
written by hand, and committed with the version bump.

The release workflow (`.github/workflows/release.yml`) runs on a `v*.*.*` tag:
it checks that the tag equals `v` + `manifest.json` version and that the
changelog has that version's section, runs the tests, builds static
`linux/amd64` and `linux/arm64` binaries with `scripts/package-release.sh`
(`qcyctl` and `qcyctld` at the root of
`qcy-omarchy_<version>_linux_<arch>.tar.gz`), and fails unless
`sha256sum *.tar.gz` of the result equals the committed `SHA256SUMS` exactly.
Only then does it attach a provenance attestation, create the release as a
draft with the tarballs and the committed `SHA256SUMS`, and make it public as
the last step (immutable releases lock the assets at that moment).

Steps for a new version:

1. Bump `version` in `manifest.json` and add the `## [x.y.z] - date` section to
   `CHANGELOG.md`.
2. Use exactly the Go version of the `go` line in `go.mod`, then run
   `scripts/release-sums.sh` (or `make release-sums`). It prints the toolchain,
   builds both tarballs and rewrites `SHA256SUMS`. Run `scripts/repro-check.sh`
   too: the tarball must be identical built from the working tree, from a copy
   without `.git` and from a dirty copy.
3. Commit the bump, the changelog and `SHA256SUMS` together; push `main`; CI must
   be green (it checks that `SHA256SUMS` names the manifest version's tarballs).
4. Dry run: `gh workflow run release.yml --ref main`, then `gh run watch`. It
   builds and compares against `SHA256SUMS` but never attests or publishes.
5. Releases must be immutable (once, repository setting):
   `gh api -X PUT repos/bbztp/qcy-omarchy/immutable-releases`. Check with
   `gh api repos/bbztp/qcy-omarchy/immutable-releases`.
6. Tag and push:

```sh
git switch main && git pull
version="$(python3 -c 'import json;print(json.load(open("manifest.json"))["version"])')"
git tag -a "v${version}" -m "v${version}"
git push origin "v${version}"
gh run watch                       # wait for the Release workflow
gh release view "v${version}"      # 2 tarballs + SHA256SUMS
```

Verify a published tarball's provenance:
`gh attestation verify qcy-omarchy_<version>_linux_amd64.tar.gz -R bbztp/qcy-omarchy`.

If the workflow reports a mismatch (the `diff` of `SHA256SUMS` fails), nothing
was published. Typical causes: the sums were generated on another Go version,
or a runner image update changed `tar`/`gzip` output. Regenerate the sums on a
matching toolchain (`go version` in the workflow log shows the one it used),
commit, and repeat from step 3; if the source changed since the sums were made,
regenerate them and review the diff. A tag that already failed is deleted and
recreated only on a commit that has not been released (never after a published
release).

v0.1.11 and older releases were published before immutable releases and before
the pinned checksums, so they stay mutable; only old commits refer to them.

Local dry run of the packaging: `scripts/package-release.sh 0.1.0 amd64 /tmp/out`.

## 3. Test the install on a clean account

On a fresh user (or VM) with Omarchy:

```sh
omarchy plugin add https://github.com/bbztp/qcy-omarchy --enable
```

Open the panel: it shows "Preparing QCY..." while the bootstrap downloads and
verifies the release, then the earbuds' state. Check
`~/.local/share/qcy-omarchy/bin/qcyctl version` prints
`qcyctl <version>`.

```sh
omarchy plugin update io.github.bbztp.qcy     # after a new release
omarchy restart shell                         # the reload alone keeps the old QML
omarchy plugin remove io.github.bbztp.qcy
```

The binaries live in `~/.local/share/qcy-omarchy/bin`, outside the plugin
directory (the shell reloads all plugins on any write under
`~/.config/omarchy/plugins`, so the plugin never writes there). `plugin remove`
does not delete them; a running `qcyctld` stays up until stopped
(the unit has a ConditionPathExists on the binary). The daemon socket is enabled by default; turn it off
first (or run `qcyctl daemon uninstall-service`). Settings > Daemon >
"Remove QCY data..." deletes the data, config and cache directories.

## 4. Submit to the marketplace

Marketplace: <https://github.com/omacom/omarchy-plugin-marketplace> (rules in
its `SUBMISSION.md`). Requirements: public repository, `manifest.json`,
README with install and removal, `LICENSE` at the root, a unique plugin ID
outside `omarchy.*` (check `registry.json`); `preview.png` is optional.
Open <https://github.com/omacom/omarchy-plugin-marketplace/issues/new?template=submit-plugin.yml>
and fill the form. Title: `[Plugin]: QCY Earbuds`.

**Repository URL** (repository root, no trailing slash)

```
https://github.com/bbztp/qcy-omarchy
```

**Category** (one of: Appearance, Desktop, Developer Tools, Hardware, Kids,
Productivity, System, Widgets, Other)

```
Hardware
```

**Tags** (one to three of: AI, Bar, Education, Games, Hyprland, Kids,
Launcher, Media, Power management, Quickshell, Security, System, VPN,
Workspaces)

```
Bar, Media, Quickshell
```

**Suggest a missing tag**

```
bluetooth
```

**Maintainer notes**

```
QCY Earbuds is a bar widget (Quickshell) that controls QCY earbuds over
Bluetooth Low Energy: battery, noise control, EQ, touch controls, LDAC,
the computer's Bluetooth codec, find my earbuds. It needs two small Go
programs (qcyctl CLI and qcyctld user daemon), so a scanner will flag the
following. All of it is deliberate:

- Downloads and runs a binary. On first open, scripts/bootstrap.sh downloads
  qcy-omarchy_<version>_linux_<arch>.tar.gz from this repository's own
  GitHub release (https only, host restricted to github.com and
  *.githubusercontent.com), verifies the SHA-256 against the SHA256SUMS
  pinned in this repository at the reviewed commit (the release's own
  checksum file is not trusted), checks the files are ELF binaries for this
  architecture, and installs them to
  ~/.local/share/qcy-omarchy/bin/<version> (staged, moved in with one
  rename; nothing is written under the plugin directory). Nothing is
  executed before the checksum matches. The builds are reproducible
  (static, -trimpath, no VCS stamp); .github/workflows/release.yml rebuilds
  them from the tagged source and refuses to publish unless they equal the
  pinned SHA256SUMS. Releases are immutable and carry a build provenance
  attestation (gh attestation verify).
- Fallback: if no release can be downloaded, it runs `go build` on the
  cloned source (needs Go).
- systemd --user units (qcyctld.socket + qcyctld.service) in
  ~/.config/systemd/user; the socket can be turned off in Settings > Daemon.
- Runs pw-dump and wpctl (PipeWire) to show and switch the computer's
  Bluetooth codec for the earbuds. No sudo.
- No sudo, no root, no system paths. No `curl | sh`. Writes only under
  ~/.local/share, ~/.config and ~/.cache in a qcy-omarchy directory;
  "Remove QCY data..." deletes them.
- Network: only the release download above and, on demand, product pictures
  from img.qcymall.com (size-limited, type-checked, can be turned off with
  `qcyctl model images off`).
- Hardware: tested on the MeloBuds N70; every other QCY model shows what its
  own control panel declares. Firmware is never written.
- After `omarchy plugin update`, run `omarchy restart shell` (omarchy#10729);
  the panel says so when needed.
```

**Submission checklist**: tick every box in the form only after checking each
statement is true (repository public with install and removal instructions;
license and dependencies documented; you own the plugin and preview; no user
configuration overwritten without consent; listing is not a security review).

After submitting, the marketplace bot posts a validation comment and an
Automated Security Baseline result on the issue. The release download makes a
`review-required` result likely; a marketplace maintainer then reviews it and
applies `approved-and-verified`, which publishes the listing. To fix a
reported problem, change the repository and edit the same issue (that reruns
validation); do not open a duplicate.

### Updating the listing later

The listing is bound to the approved commit; newer commits show as "Update
unverified" until verified. After releasing a new version, open
<https://github.com/omacom/omarchy-plugin-marketplace/issues/new?template=verify-plugin.yml>,
choose "Verify and publish a newer upstream commit", and give the plugin ID
(`io.github.bbztp.qcy`), the repository URL and the full 40-character SHA of
`HEAD` (`git rev-parse HEAD`).
