# Publishing guide (maintainer)

Nothing here is automated except the release build. Do the steps in order.

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

The release workflow (`.github/workflows/release.yml`) runs on a `v*.*.*` tag:
it checks that the tag equals `v` + `manifest.json` version and that the
changelog has that version's section, builds static `linux/amd64` and
`linux/arm64` binaries, packs `qcy-omarchy_<version>_linux_<arch>.tar.gz`
(`qcyctl` and `qcyctld` at the root), writes `SHA256SUMS`, and publishes the
release with the changelog section as notes. The plugin's bootstrap downloads
exactly these files and refuses anything whose SHA-256 does not match.

```sh
git switch main && git pull
version="$(python3 -c 'import json;print(json.load(open("manifest.json"))["version"])')"
git tag -a "v${version}" -m "v${version}"
git push origin "v${version}"
gh run watch                       # wait for the Release workflow
gh release view "v${version}"      # 2 tarballs + SHA256SUMS
```

Local dry run of the packaging: `scripts/package-release.sh 0.1.0 amd64 /tmp/out`.

To release a new version: bump `version` in `manifest.json`, add a
`## [x.y.z] - date` section to `CHANGELOG.md`, commit, tag `vx.y.z`.

## 3. Test the install on a clean account

On a fresh user (or VM) with Omarchy:

```sh
omarchy plugin add https://github.com/bbztp/qcy-omarchy --enable
```

Open the panel: it shows "Preparing QCY..." while the bootstrap downloads and
verifies the release, then the earbuds' state. Check
`~/.config/omarchy/plugins/io.github.bbztp.qcy/.bin/qcyctl version` prints
`qcyctl <version>`.

```sh
omarchy plugin update io.github.bbztp.qcy     # after a new release
omarchy plugin remove io.github.bbztp.qcy
```

The binaries live inside the plugin directory (`.bin/`, git-ignored), so
`plugin remove` deletes them with the plugin; a running `qcyctld` notices that
its executable is gone and exits. If "Start with the system" is on, turn it off
first (or run `qcyctl daemon uninstall-service`). Settings > Daemon >
"Remove QCY data..." deletes the data, config and cache directories.

## 4. Submit to the marketplace

Marketplace: <https://github.com/omacom/omarchy-plugin-marketplace>.
Open <https://github.com/omacom/omarchy-plugin-marketplace/issues/new?template=submit-plugin.yml>
and fill the form. Title: `[Plugin]: QCY Earbuds`. Available tags: ai, bar,
education, games, hyprland, kids, launcher, media, power-management,
quickshell, security, system, vpn, workspaces (pick 1 to 3).

Paste, per heading, in this order:

**Repository URL**

```
https://github.com/bbztp/qcy-omarchy
```

**Category**

```
Hardware
```

(If "Hardware" is not offered, pick the closest available option, e.g. Audio.)

**Tags**

```
bar, media, quickshell
```

**Suggest a missing tag**

```
bluetooth
```

**Maintainer notes**

```
QCY Earbuds is a bar widget (Quickshell) that controls QCY earbuds over
Bluetooth Low Energy: battery, noise control, EQ, touch controls, find my
earbuds. It needs two small Go programs (qcyctl CLI and qcyctld user daemon),
so a scanner will flag the following. All of it is deliberate:

- Downloads and runs a binary. On first open, scripts/bootstrap.sh downloads
  qcy-omarchy_<version>_linux_<arch>.tar.gz and SHA256SUMS from this
  repository's own GitHub release (https only, host restricted to github.com
  and *.githubusercontent.com), verifies the SHA-256, checks the files are
  ELF binaries for this architecture, and installs them to the plugin's own
  .bin directory (~/.local/share/qcy-omarchy/bin if that is not writable). Nothing is executed before the checksum
  matches. The release is built by .github/workflows/release.yml from the
  tagged source (static, -trimpath).
- Fallback: if no release can be downloaded, it runs `go build` on the cloned
  source (needs Go; no network beyond Go's module cache).
- Optional systemd --user unit: the daemon can be enabled at login from
  Settings > Daemon; it is off until the user turns it on. The unit lives in
  ~/.config/systemd/user.
- No sudo, no root, no system paths. No `curl | sh`. Writes only inside the
  plugin directory and under ~/.local/share, ~/.config and ~/.cache in a
  qcy-omarchy directory; "Remove QCY data..." deletes the latter.
- Network: only the release download above and, on demand, product pictures
  from img.qcymall.com (size-limited, type-checked, can be turned off with
  `qcyctl model images off`).
- Hardware: only one model (MeloBuds N70) was tested; other QCY models get a
  conservative profile with risky features blocked. Firmware is never written.
```

**Submission checklist**: tick every box in the form (only after you have
actually done each item: tested on a clean account, preview.png committed,
license present, README documents install and removal).

Attach nothing else; the form reads `preview.png` from the repository.

### Updating the listing later

After releasing a new version, open the update form
(<https://github.com/omacom/omarchy-plugin-marketplace/issues/new?template=verify-plugin.yml>)
and give it the SHA of the new `HEAD` (`git rev-parse HEAD`) so the scanner
re-verifies the plugin.
