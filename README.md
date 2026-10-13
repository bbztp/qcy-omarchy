# QCY Earbuds for Omarchy

A bar widget for the [Omarchy](https://omarchy.org) shell that controls QCY
earbuds (developed against the **MeloBuds N70 / HT18**, but written to the
vendor's low-energy GATT protocol) and shows their state:

- **Battery** in the bar (glyph), with per-ear and case
  levels plus charging flags in the panel.
- **Active noise control** — Off, Adaptive, Indoor, Commuting, Noisy,
  Anti-wind, Transparency.
- **Toggles** — LDAC (hi-res audio), Multipoint, Game mode (low latency),
  Sleep mode, Wind noise reduction.
- **Readout** — firmware, EQ preset + curve, balance, auto power-off, prompt
  volume.

It talks to the earbuds directly over Bluetooth Low Energy — no phone, no
vendor app. A small user daemon, `qcyctld`, holds the BLE connection so
the widget's calls are fast; `qcyctl` is the matching Go CLI.

The earbuds accept **one BLE client at a time**, so the daemon does not hold
them permanently: it connects on the first request, keeps the link for an idle
window (6 s by default, `QCYCTL_IDLE_SECONDS` or `qcyctld -idle N`, `0` = never
release) and then disconnects, which frees the earbuds for the phone app. If
the phone app (or anything else) holds them, requests fail with a `busy` error
("earbuds are in use by another app"), the panel shows the last known state
marked stale, and it recovers on the next refresh once the other app lets go.

## Requirements

- Omarchy (Quickshell-based shell).
- Go (to build `qcyctl` and `qcyctld`).
- systemd (user session) to run the `qcyctld` daemon.
- The earbuds paired with the machine.

## Install

```sh
git clone https://github.com/bbztp/qcy-omarchy
cd qcy-omarchy
./scripts/install.sh
```

This builds `qcyctl` and `qcyctld`, installs them to `~/.local/bin`, installs
and starts the `qcyctld.service` systemd user unit
(`~/.config/systemd/user/qcyctld.service`), copies the plugin to
`~/.config/omarchy/plugins/io.github.bbztp.qcy`, and rescans plugins. Pass
`--no-service` to skip the unit, `--no-build` to reuse `make build` output.
`make install` does the same.

Then add the widget to your bar — put this in `~/.config/omarchy/shell.json`
under `bar.layout` (e.g. inside the `right` array), which hot-reloads on save:

```json
{ "id": "io.github.bbztp.qcy" }
```

or move it into place with `omarchy bar move io.github.bbztp.qcy --section right`.

## Usage

- **Left click** the bar button — open the panel.
- **Right click** — refresh.
- In the panel — **Esc** closes, **r** refreshes.

The full state is read on connect and when the panel opens; the panel shows
`Reading…` while it does.

The panel has three tabs: **Status** (battery, connection state, noise mode),
**Som** (live EQ curve and the curve library: clicking a card runs
`qcyctl library apply <id> --yes`; it is disabled while slot 6 or 11 is active; the panel names the active
effect, e.g. `Experiência auditiva (slot 11)`)
and **Ajustes** (noise scenes, toggles, balance, touch controls, device info).

EQ slot -> effect map (MeloBuds N70, read from the device; the phone app's own
curves are always slot 255):

| Slot | Effect | Status |
|---|---|---|
| 1 / 2 / 3 / 4 / 5 / 6 | Predefinido / Popular / Bass pesados / Rock / Suave / Clássico | medido |
| 10 | Som espacial (same payload as slot 1) | medido |
| 11 | Experiência auditiva (your hearing-test profile) | medido |
| 14 | Agudos suaves | medido |
| 255 | Personalizado (any custom curve made in the app) | medido |
| 7, 8, 9, 12, 13 | ? | não observado |

Slot 0 is the old test curve of this tool. Details in `backend/PROTOCOL.md`.

## Languages

The panel is in English by default and ships with Português (Brasil). It follows
your system language (`LC_ALL`, `LC_MESSAGES`, `LANG`); to override it, pick a
language in the panel under Settings → Language (Automatic restores the
system's), or run `qcyctl i18n set <code|auto>`.

To add a language, no code changes are needed:

```sh
mkdir -p ~/.config/qcy-omarchy/i18n
qcyctl i18n template > ~/.config/qcy-omarchy/i18n/es.json
```

Edit `es.json`: set `_name` to the language's own name (shown in the picker),
`_decimal` to its decimal separator, and translate the values (the keys are the
English texts; keep `{n}`, `{name}`... markers as they are). Untranslated or
empty entries fall back to English. Then choose the language in the panel.
`qcyctl i18n list` shows what was found; an invalid file is skipped with a
warning on stderr. A file named `pt-BR.json` overrides entries of the shipped
one; `pt.json` is the fallback for any `pt-*`. User files live outside the
plugin directory, so reinstalling the plugin keeps them.

## Settings

Configurable per widget in `shell.json` (or via the shell's settings UI):

| Key | Default | Meaning |
| --- | --- | --- |
| `scanSeconds` | `10` | How long to listen for the earbuds' advertisement (5–60 in the settings UI; values below 5 are raised to 5). |
| `deviceMatch` | `"qcy"` | Case-insensitive substring to recognise your earbuds. |

Set `QCYCTL` in the environment to point the widget at a `qcyctl` binary that
is not in `~/.local/bin`.

## A note on safety

The widget writes the EQ **master gain**. Before any EQ write, the raw EQ
payload of the currently **active** slot, as read from the earbuds, is saved to
`~/.local/share/qcy-omarchy/backups/`. This is not a backup of whatever the
slot you overwrite held before: a slot's contents cannot be read while it is
inactive. In particular, `library apply` / `library compose` into slot 255 (the
app's "Personalizado") back up only the active payload: if slot 255 is not the
active one, its previous content is replaced with no copy (the phone app does
the same when you pick a curve). `qcyctl eq set` overwrites the target EQ slot and refuses to run
without `--yes`; it changes only band positions 0-9 and leaves positions 10+
(custom curves may use up to 20 bands) as read. The master gain range is
-12..+12 dB. When a curve uses positions 10+, the write sends the full
143-byte payload (`fullLength: true` in `--json` output, plus a warning): the
73-byte write is proven on the earbuds; the 143-byte one is not proven for the
factory slots and needs `--full-length` there, but slot 255 (the app's
"Personalizado", where the phone app itself writes up to 143 bytes) takes it
without the flag.

## `qcyctl`

The backend is also usable on its own:

```sh
qcyctl status --json          # full machine-readable snapshot
qcyctl status                 # human-readable snapshot
qcyctl anc noisy              # set ANC mode
qcyctl ldac on                # toggles: game|sleep|ldac|multipoint|wind
qcyctl find                   # flash the earbuds
qcyctl library seed           # the 8 factory effects (embedded) + Experiência auditiva, offline, idempotent
qcyctl library list [--all]   # saved EQ curves (offline): factory in the app's order, Experiência auditiva, then yours; --all also shows older seeds
qcyctl library apply <id> --yes [--slot 255|0]   # write a saved curve to slot 255 (default: the app's "Personalizado" buffer, the only safe target while a factory effect 6/11 is active) or 0
qcyctl library compose --base predefinido --gains 0,0,3,0,0,-2,0,0,0,4.5 --yes   # your bars on top of a factory effect, in the app's layout, into slot 255
qcyctl library compose --base none --gains 1,0,0,0,0,0,0,0,0,0 --dry-run --json  # only print the payload and the resulting bands
```

The curve library (`qcyctl library`) keeps named EQ curves under
`~/.local/share/qcy-omarchy/library`; `list`, `show`, `save`, `delete`,
`export`, `import`, `seed` and `compose --dry-run` need no Bluetooth. The
library applies and composes any curve to slot 255: every seeded curve is
applyable (band types 0, 2, 3 and 255 are accepted, positions 0-19). `compose`
puts the non-zero bars of `--gains` (ISO 31..16000 Hz, Q 100, type 255) first and
the base effect's bands after them, up to 20 bands, with the base's master
gain (or `--master`, default -12). See `backend/PROTOCOL.md`.

Run `qcyctl` with no arguments for the full command list. Set `QCY_DEBUG=1` to
trace the GATT traffic of the CLI on stderr; for the daemon set `QCYCTL_DEBUG=1`
(e.g. `systemctl --user edit qcyctld.service`, then `journalctl --user -u qcyctld`).
The same drop-in can set `Environment=QCYCTL_IDLE_SECONDS=<n>` to change the idle
window.

## Layout

```
backend/                    Go source for qcyctl
backend/cmd/qcyctld/        Go source for the qcyctld daemon
packaging/qcyctld.service   systemd user unit
plugin/io.github.bbztp.qcy/ the Omarchy bar widget (QML)
scripts/install.sh          build + install
```

## Uninstall

```sh
make uninstall
```

This disables and removes the `qcyctld.service` user unit, removes
`~/.local/bin/qcyctl` and `qcyctld`, removes the plugin directory, and rescans
plugins. Remove the `{ "id": "io.github.bbztp.qcy" }` entry from
`shell.json` yourself. Backups in `~/.local/share/qcy-omarchy/backups/` are
left in place.

## Credits

Protocol work builds on the public work in
[Quicky](https://github.com/hui1601/Quicky) and
[OpenQCY-Desktop](https://github.com/GiorgioRafael/OpenQCY-Desktop).

## License

MIT — see [LICENSE](LICENSE).
