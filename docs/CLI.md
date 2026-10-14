# qcyctl and qcyctld reference

`qcyctl` is the command-line client and `qcyctld` the user daemon that holds the
Bluetooth Low Energy connection. The plugin uses both; you can use them on their own.

Run `qcyctl` with no arguments for the full command list.

## Commands

```sh
qcyctl status --json          # full machine-readable snapshot
qcyctl status                 # human-readable snapshot
qcyctl anc noisy              # set ANC mode
qcyctl game on                # toggles: game|sleep|multipoint|wind (wind and wear "off" write 0x00 on verified models)
qcyctl ldac on --yes          # restarts the earbuds; verified models only; not yet verified on hardware
qcyctl find                   # play a beeping tone on the earbuds' audio sink to find them: 100% sink volume, tone at -3 dBFS
                              # (--start-volume/--max-volume/--tone-level), stops after --timeout (default 60 s); asks you to take the
                              # earbuds out of your ears first (--yes skips the question; --events = JSON lines for the plugin)
qcyctl prompt-vol 8           # prompt volume: 4|6|8|10|low|medium|high|max (N70)
qcyctl wear-sensitivity high  # medium|normal|high|1|2|3 (N70; needs wear detection on)
qcyctl fit start              # ear-tip fit test (N70): plays a sound, waits and prints left/right (stop|begin also exist)
qcyctl i18n list|bundle|template|set   # languages, offline (see README, Languages)
qcyctl model image            # device pictures, downloaded on demand (see Device images below)
qcyctl library seed           # the 8 factory effects (embedded) + Hearing experience, offline, idempotent
qcyctl library list [--all]   # saved EQ curves (offline): factory in the app's order, Hearing experience, then yours; --all also shows older seeds
qcyctl library apply <id> --yes [--slot 255|0]   # write a saved curve to slot 255 (default: the app's "Custom" buffer, the only safe target while a factory effect 6/11 is active) or 0
qcyctl library compose --base predefinido --gains 0,0,3,0,0,-2,0,0,0,4.5 --yes   # your bars on top of a factory effect, in the app's layout, into slot 255
qcyctl library compose --base none --gains 1,0,0,0,0,0,0,0,0,0 --dry-run --json  # only print the payload and the resulting bands
```

### Curve library

`qcyctl library` keeps named EQ curves under `~/.local/share/qcy-omarchy/library`.
`list`, `show`, `save`, `delete`, `export`, `import`, `seed` and
`compose --dry-run` need no Bluetooth.

The library applies and composes any curve to slot 255: every seeded curve is
applyable (band types 0, 2, 3 and 255 are accepted, positions 0-19). `compose`
puts the non-zero bars of `--gains` (ISO 31..16000 Hz, Q 100, type 255) first and
the base effect's bands after them, up to 20 bands, with the base's master gain
(or `--master`, default -12). See [PROTOCOL.md](PROTOCOL.md).

Library ids of the factory effects are kept from the original data (for example
`predefinido` is Default); `qcyctl library list` prints them.

## EQ slot to effect map

MeloBuds N70, read from the device. The phone app's own curves are always slot 255.

| Slot | Effect | Status |
| --- | --- | --- |
| 1 / 2 / 3 / 4 / 5 / 6 | Default / Popular / Heavy bass / Rock / Soft / Classical | measured |
| 10 | Spatial sound (same payload as slot 1) | measured |
| 11 | Hearing experience (your hearing-test profile) | measured |
| 14 | Soft treble | measured |
| 255 | Custom (any custom curve made in the app) | measured |
| 7, 8, 9, 12, 13 | unknown | not observed |

Slot 0 is the old test curve of this tool. Details in [PROTOCOL.md](PROTOCOL.md).

## A note on safety

The widget writes the EQ **master gain**. Before any EQ write, the raw EQ
payload of the currently **active** slot, as read from the earbuds, is saved to
`~/.local/share/qcy-omarchy/backups/`.

- This is not a backup of whatever the slot you overwrite held before: a slot's
  contents cannot be read while it is inactive.
- `library apply` / `library compose` into slot 255 (the app's "Custom") back up
  only the active payload. If slot 255 is not the active one, its previous
  content is replaced with no copy (the phone app does the same when you pick a
  curve).
- `qcyctl eq set` overwrites the target EQ slot and refuses to run without
  `--yes`. It changes only band positions 0-9 and leaves positions 10+ (custom
  curves may use up to 20 bands) as read.
- The master gain range is -12..+12 dB.
- When a curve uses positions 10+, the write sends the full 143-byte payload
  (`fullLength: true` in `--json` output, plus a warning). The 73-byte write is
  proven on the earbuds; the 143-byte one is not proven for the factory slots and
  needs `--full-length` there. Slot 255 (the app's "Custom", where the phone app
  itself writes up to 143 bytes) takes it without the flag.

## Daemon lifecycle

Commands that talk to the earbuds start `qcyctld` on demand when its socket does
not answer: through `qcyctld.service` if that user unit is enabled, otherwise by
launching `qcyctld` detached (looked up next to `qcyctl`, then in `PATH`, then
`~/.local/share/qcy-omarchy/bin` and `~/.local/bin`). Its output goes to
`${XDG_STATE_HOME:-~/.local/state}/qcy-omarchy/qcyctld.log` (truncated above
1 MiB); concurrent starters are serialised with a lock. Offline commands (`i18n`,
`library list/show/...`, `model`, `hearing plan|result`, `version`) never start
it.

```sh
qcyctl daemon start              # idempotent
qcyctl daemon stop               # SIGTERM to the socket owner, or systemctl stop
qcyctl daemon status [--json]    # running, pid, managedBySystemd, serviceInstalled,
                                 # serviceEnabled, binary, log
qcyctl daemon install-service    # writes ~/.config/systemd/user/qcyctld.service with
                                 # the resolved qcyctld path, then enable --now
qcyctl daemon uninstall-service  # disable --now, removes the unit
qcyctl version [--json]          # also: qcyctld --version
```

[`packaging/qcyctld.service`](../packaging/qcyctld.service) is a static example;
`install-service` writes the same unit with the real `ExecStart=` path.

### One client at a time

The earbuds accept **one BLE client at a time**, so the daemon does not hold them
permanently. It connects on the first request, keeps the link for an idle window
and then disconnects, which frees the earbuds for the phone app. If another app
holds them, requests fail with a `busy` error ("earbuds are in use by another
app"), the panel shows the last known state marked stale, and it recovers on the
next refresh once the other app lets go.

### Environment variables

| Variable | Applies to | Effect |
| --- | --- | --- |
| `QCYCTL_IDLE_SECONDS` | daemon | Idle window before releasing the earbuds. Default 6 s; `0` = never release. Same as `qcyctld -idle N`. |
| `QCYCTL_NO_AUTOSTART` | `qcyctl` | Set to `1` to disable on-demand daemon start. |
| `QCY_DEBUG` | `qcyctl` | Set to `1` to trace the GATT traffic of the CLI on stderr. |
| `QCYCTL_DEBUG` | `qcyctld` | Set to `1` to trace the daemon's GATT traffic. |
| `QCYCTL` | plugin | Path to a `qcyctl` binary to use instead of the default lookup. |

To set variables for the daemon, use a systemd drop-in:

```sh
systemctl --user edit qcyctld.service     # add Environment=QCYCTL_DEBUG=1
journalctl --user -u qcyctld              # read the output
```

## Device images

The panel can show your earbuds (left, right, case, icon) in your colour. The
images are not part of this repository: `qcyctl model image` downloads them on
demand, the first time they are needed, from QCY's image server
(`img.qcymall.com`, the same files the QCY phone app uses; the URLs come from the
product list embedded in the backend). Only that host is ever contacted, files
are limited to 2 MiB and checked to be WebP/PNG before they are saved.

They are kept in `~/.cache/qcy-omarchy/images/<vendorId>/<colour>/`
(`$XDG_CACHE_HOME` is honoured) and are never fetched twice; `--refresh`
downloads them again. The colour comes from `--color`, or from the file
`~/.config/qcy-omarchy/color` (a colour name or index, e.g. `Purple` or `2`),
otherwise it is the product's default colour; the colour the earbuds advertise
is not used because the scanner does not report it reliably.

```sh
qcyctl model image [--vendor N] [--color <name|index>] [--refresh] [--no-network] [--json]
qcyctl model images off      # stop all downloads (on re-enables, status shows)
```

With downloads off, the panel uses only what is already cached, or its drawn
icons. Delete the cache directory to remove the files.
