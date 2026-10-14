# qcy-omarchy

Omarchy shell bar plugin (QML) plus a Go CLI (`qcyctl`) and daemon (`qcyctld`)
that control QCY earbuds over Bluetooth Low Energy. Plugin id
`io.github.bbztp.qcy`; Go module `github.com/bbztp/qcy-omarchy`.

## Product rules (do not re-litigate)

- **Every model follows its panel.** The embedded server panel of each model
  (`internal/qcycore/models/models.json`, schema in `docs/reference/panels-schema.md`)
  declares what it offers. Anything general in the protocol must work for every
  model; model-specific features appear when that model's panel declares them.
  Never gate a feature on "verified on the N70" or on a hard-coded N70 constant.
- Legitimate gates are only: the panel does not declare it, or its encoding is
  not understood (then the feature is absent and `model.notes` says why).
  "Not tested on this model" is informational (README, panel note), never a block.
- **Firmware/OTA writing is forbidden.** Configuration writes (EQ, keys, toggles,
  prompt volume, ...) are allowed, with backup before EQ writes, readback after,
  and `--yes` for destructive actions. Reset/rename sit behind confirmation.
- Keep the N70's tested extensions (EQ +-12 dB, 20 EQ positions, slot 255 compose).
- Earbuds accept one BLE client at a time: the daemon releases them after an
  idle window; a `busy` error is expected and must degrade gracefully.

## Hard rule: no source references

Nothing in the repo (code, comments, docs, tests, commit messages) may reference
the vendor app's source, its reverse-engineering, or the material kept outside the
repo; say "vendor app" and describe behaviour, never origin. The exact forbidden
terms and the check command are in `CLAUDE.local.md` (gitignored, not published):
run it before every commit; it must print nothing. Never commit real MAC addresses
(use `AA:BB:CC:DD:EE:FF`), home paths, tokens or personal data.

## Architecture

```
cmd/qcyctl/        CLI; talks to the daemon over a unix socket, autostarts it
cmd/qcyctld/       daemon, sole owner of BLE; handlers per model (N70 handler,
                   profileHandler derived from the panel), setters, fn.go (generic
                   panel-declared commands; firmware ops forbidden)
internal/protocol  frame codec, opcode registry (generated from table.json)
internal/qcycore   EQ, presets, backups, socket protocol, models (embedded panels)
internal/hearing   hearing-test algorithm
BarWidget.qml, Qcy.qml, qmldir, manifest.json, assets/   the plugin (repo root)
scripts/           install, bootstrap (release download + go build fallback), packaging
docs/              PROTOCOL.md, WEB_API.md, CLI.md, PUBLISHING.md, reference/
```

- The plugin never talks BLE: only `qcyctl`. The daemon is the only BLE owner.
- Capabilities flow daemon -> status `model` object -> QML (`has()`); the QML must
  not hard-code model ids or vendor ids.
- Generated files: edit `docs/reference/protocol.json` / `scripts/gen_models.py`
  inputs and regenerate (`make protocol`, `go generate ./internal/protocol`);
  never hand-edit `internal/protocol/table.json` or `models.json`.
- Dependencies: keep the Go module dependency list minimal; a new dependency needs
  a reason in the commit message.

## Commands

```sh
make build            # qcyctl + qcyctld into build/
make vet              # go vet ./...
gofmt -l .            # must print nothing (CI fails otherwise)
go test -count=1 ./...
bash scripts/bootstrap_test.sh   # bootstrap against a local fake release
```

CI (`.github/workflows/ci.yml`) runs: static build (`CGO_ENABLED=0`), `go vet`,
`gofmt -l`, `go test -count=1`, shell syntax + bootstrap test, manifest semver.
Redirect test output to a file and check the exit code; do not pipe to `tail`.

## Code and test conventions

- Go: standard `gofmt`; errors wrapped with context; no global mutable state
  beyond what already exists; user-facing strings go through the i18n bundle
  (`cmd/qcyctl/i18n/pt-BR.json`, English is the key).
- Tests must never touch hardware or the user's HOME: each package's `TestMain`
  disables BLE and redirects `HOME`/`XDG_*`. Use the fake transport.
- A bug fix comes with a test that fails without the fix. Capability changes get a
  table test over all `models.json` vendorIds (no panics, no feature declared
  without its parameters).
- QML: user-facing text goes through `Qcy.tr`/the i18n helpers, no raw Portuguese or
  English literals in new code; text must not clip (use elide + tooltip);
  hot-reload serves stale code, so `omarchy restart shell` after installing QML.

## Documentation and language

- All repo text (README, docs, comments, commit messages) is in English.
  Portuguese appears only in `cmd/qcyctl/i18n/pt-BR.json` and verbatim device strings.
- README stays short and links to `docs/CLI.md`; protocol facts go to
  `docs/PROTOCOL.md`; mark what is measured on hardware vs inferred.
- Update `CHANGELOG.md` (Keep a Changelog) for user-visible changes; the version
  lives only in `manifest.json` and is injected into the binaries.

## Operational cautions

- Do not inject mouse/keyboard input into the user's session; drive the panel via
  `qs ipc call io.github.bbztp.qcy open` and take screenshots only of the panel region.
- Warn before sound tests; use the user's explicit sink and `--yes` flags.
- Never `pkill -f` / `pgrep -f` with a pattern that matches its own command.
- Never rewrite published history or force-push without explicit authorization.
