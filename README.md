# QCY Earbuds for Omarchy

A bar widget for the [Omarchy](https://omarchy.org) shell that controls QCY
earbuds (developed against the **MeloBuds N70 / HT18**, but written to the
vendor's low-energy GATT protocol) and shows their state:

- **Battery** in the panel: the earbuds' pictures (left, right, case) with a
  level bar under each (red below 20%, amber to 49%, green from 50%) and
  charging flags. The bar button shows the QCY wordmark.
- **Active noise control** — Off, Adaptive, Indoor, Commuting, Noisy,
  Anti-wind, Transparency.
- **Toggles** — LDAC (hi-res audio), Multipoint, Game mode (low latency),
  Sleep mode, Wind noise reduction.
- **Readout** — firmware, EQ preset + curve, balance, auto power-off, prompt
  volume.
- **Settings (MeloBuds N70)** — prompt volume (4/6/8/10), wear-sensor
  sensitivity (medium/normal/high), ear-tip fit test with a result per ear.

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
- In the panel — **Esc** closes, **r** refreshes, **1** / **2** / **3** switch
  tabs.

The full state is read on connect and when the panel opens; the panel shows
`Reading…` while it does.

The panel has three tabs (named in the active language; the pt-BR names are
Status / Som / Ajustes):

| Tab | Content |
| --- | --- |
| **Status** | Earbuds picture and connection state, battery (left, right, case) as pictures with a level bar, noise mode. |
| **Sound** | The effect library (click a card to apply it) and the 10-bar equalizer (31 Hz to 16 kHz, ±12 dB). The bars show the curve of the chosen effect; drag a bar and release to apply (the sound changes right away), double-click to zero it. **Start from scratch** drops the base effect so the bars are the whole curve; **Save as…** stores what you see in the curve library. Bars are written on top of the effect as a curve in slot 255. |
| **Settings** | Noise scenes, toggles, balance, auto power-off, prompt volume, sensor sensitivity, fit test, touch controls, device info, language. |

On a model that is not a verified MeloBuds N70 the Sound tab only says that
the equalizer is unavailable, and the Settings tab offers only what the
product declares (see [Supported models](#supported-models)).

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

Slot 0 is the old test curve of this tool. Details in `docs/PROTOCOL.md`.

## Supported models

The daemon identifies the model by the 16-bit `vendorId` in the earbuds'
advertisement, not by the Bluetooth name.

| vendorId | Model | Profile |
| --- | --- | --- |
| 23872, 23877 | MeloBuds N70 | **verified**: everything above, including EQ and touch keys |
| any other id in the embedded QCY product list | `qcy-<vendorId>` | conservative: only the common controls the product declares (noise control, balance, find, auto power-off, wear detection, game/sleep/LDAC/multipoint/wind toggles); **EQ, touch keys, fit test, prompt volume and sensor sensitivity are blocked**; the panel says "Unverified model" |
| unknown | name, then firmware string, then `generic` | |

Only the N70 was tried on real hardware. Where the product data comes from
and how the profile is derived: `docs/WEB_API.md`.

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

## Device images

The panel can show your earbuds (left, right, case, icon) in your colour. The
images are not part of this repository: `qcyctl model image` downloads them on
demand, the first time they are needed, from QCY's image server
(`img.qcymall.com`, the same files the QCY app uses; the URLs come from the
product list embedded in the backend). Only that host is ever contacted, files
are limited to 2 MiB and checked to be WebP/PNG before they are saved.

They are kept in `~/.cache/qcy-omarchy/images/<vendorId>/<colour>/`
(`$XDG_CACHE_HOME` is honoured) and are never fetched twice; `--refresh`
downloads them again. The colour comes from `--color`, or from the file
`~/.config/qcy-omarchy/color` (a colour name or index, e.g. `Purple` or `2`),
otherwise it is the product's default colour; the colour the earbuds advertise
is not used because the scanner does not report it reliably.

Print the paths without the panel: `qcyctl model image [--vendor N]
[--color <name|index>] [--refresh] [--no-network] [--json]`.

To stop all downloads run `qcyctl model images off` (`on` re-enables,
`status` shows); the panel then uses only what is already cached, or its drawn
icons. Delete the cache directory to remove the files.

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
qcyctl game on                # toggles: game|sleep|multipoint|wind (wind and wear "off" write 0x00 on verified models, as the app does)
qcyctl ldac on --yes          # restarts the earbuds; verified models only; not yet verified on hardware
qcyctl find                   # play a beeping tone on the earbuds' audio sink to find them: 20% rising +5 points/2 s up to
                              # --max-volume (default 70%), stops after --timeout (default 60 s); asks you to take the
                              # earbuds out of your ears first (--yes skips the question; --events = JSON lines for the plugin)
qcyctl prompt-vol 8           # prompt volume: 4|6|8|10|low|medium|high|max (N70)
qcyctl wear-sensitivity high  # medium|normal|high|1|2|3 (N70; needs wear detection on)
qcyctl fit start              # ear-tip fit test (N70): plays a sound, waits and prints left/right (stop|begin also exist)
qcyctl i18n list|bundle|template|set   # languages, offline (see Languages)
qcyctl model image            # device pictures, downloaded on demand (see Device images)
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
gain (or `--master`, default -12). See `docs/PROTOCOL.md`.

Run `qcyctl` with no arguments for the full command list. Set `QCY_DEBUG=1` to
trace the GATT traffic of the CLI on stderr; for the daemon set `QCYCTL_DEBUG=1`
(e.g. `systemctl --user edit qcyctld.service`, then `journalctl --user -u qcyctld`).
The same drop-in can set `Environment=QCYCTL_IDLE_SECONDS=<n>` to change the idle
window.

## Layout

```
cmd/qcyctl/                 Go source for qcyctl
cmd/qcyctld/                Go source for the qcyctld daemon
internal/                   Go packages shared by both (qcycore, hearing)
docs/                       PROTOCOL.md, WEB_API.md
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
