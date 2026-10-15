# qcyctl and qcyctld reference

`qcyctl` is the command-line client and `qcyctld` the user daemon that holds the
Bluetooth Low Energy connection. The plugin uses both; you can use them on their own.

Run `qcyctl` with no arguments for the full command list.

## Commands

```sh
qcyctl status --json          # full machine-readable snapshot
qcyctl status                 # human-readable snapshot
qcyctl anc noisy              # set ANC mode
qcyctl anc --raw 1 5 0        # the three bytes a model's panel lists (status --json: model.ancModes)
qcyctl game on                # toggles: game|sleep|multipoint|wind (wind and wear "off" write 0x00 where the model's panel says so)
qcyctl ldac on --yes          # restarts the earbuds; only where the model's panel declares it; tested on the N70
qcyctl codec --address AA:BB:CC:DD:EE:FF [--json]            # the computer's Bluetooth audio codec for the earbuds (see Computer codec below)
qcyctl codec set LDAC --address AA:BB:CC:DD:EE:FF [--json]   # switch the computer to another music codec
qcyctl find                   # play a beeping tone on the earbuds' audio sink to find them: 100% sink volume, tone at -3 dBFS
                              # (--start-volume/--max-volume/--tone-level), stops after --timeout (default 60 s); asks you to take the
                              # earbuds out of your ears first (--yes skips the question; --events = JSON lines for the plugin)
qcyctl prompt-vol 8           # prompt volume: one of the model's steps (status --json: model.promptVolumeSteps) or low|medium|high|max
qcyctl wear-sensitivity high  # one of the model's items (name or value; N70: medium|normal|high|1|2|3); needs wear detection on
qcyctl fit start              # ear-tip fit test (N70): plays a sound, waits and prints left/right (stop|begin also exist)
qcyctl i18n list|bundle|template|set   # languages, offline (see README, Languages)
qcyctl model image            # device pictures, downloaded on demand (see Device images below)
qcyctl library seed           # the 8 factory effects (embedded) + Hearing experience, offline, idempotent
qcyctl library list [--all]   # saved EQ curves (offline): factory in the app's order, Hearing experience, then yours; --all also shows older seeds
qcyctl library import-device <name> --expect-slot 11 [--replace [--backup]] [--user] [--require-ears]   # save the active EQ read from the earbuds (never writes to them); --replace overwrites the entry of that name (needs --expect-slot), --backup copies the old entry to ~/.local/share/qcy-omarchy/backups/curve-<id>-<UTC>.json first, --user stores it as your own curve; an entry with per-ear curves (hearing-test result) can only be replaced with --backup; the "Experiência auditiva" card imported from slot 11 gets BOTH ears when the earbuds report them (`status --json` `eq.ears`), in the same shape as a card made by `hearing run`, so `library apply` writes 0x46/0x47 from it; --require-ears saves nothing when the ears are not reported (older daemon, read failed)
qcyctl library matches <id>|--any-user|--any --master -10 --bands '[...]' [--slot N] [--ears '{"left":[...],"right":[...]}']   # offline: does the EQ the earbuds report (status eq.masterGain, eq.bands) match the entry? prints {"match":bool}; --any-user tries all your own curves and adds "id"; --any tries your curves first, then the factory cards (a card another machine wrote to slot 255), and also adds "id"; hearing-test entries (with per-ear curves) match only slot 11 (--slot gives the active slot; without it the slot is not checked): the last 8 reported bands must be the left ear (the prefix before them is ignored) and, with --ears (status `eq.ears`), the right ear too; --any-user and --any never name a hearing-test entry; --any cannot be combined with --any-user or an id
qcyctl library apply <id>|--file <curve.json> --yes [--slot 255|0]   # --file applies a curve JSON (for example a restore point `curve-*.json` from ~/.local/share/qcy-omarchy/backups/) without adding it to the library; write a saved curve to slot 255 (default: the app's "Custom" buffer, the only safe target while a factory effect 6/11 is active) or 0
qcyctl library compose --base predefinido --gains 0,0,3,0,0,-2,0,0,0,4.5 --yes   # your bars on top of a factory effect, in the app's layout, into slot 255
qcyctl library compose --base none --gains 1,0,0,0,0,0,0,0,0,0 --dry-run --json  # only print the payload and the resulting bands
```

### Computer codec

`qcyctl codec` reads the earbuds' Bluetooth device from PipeWire (`pw-dump`) and
`qcyctl codec set <codec>` switches it with `wpctl set-profile`. It needs no sudo,
no Bluetooth LE and no daemon, and works for any Bluetooth earbuds. `--address` is
the classic Bluetooth address (the one `bluetoothctl devices` shows), required. The
codec name is matched ignoring case, spaces and dashes (`sbc-xq`, `SBC_XQ`).

```json
{"device": 92,
 "active": {"codec": "LDAC", "kind": "music"},
 "music": [{"codec": "SBC", "profile": "a2dp-sink-sbc", "index": 131073, "active": false},
           {"codec": "LDAC", "profile": "a2dp-sink", "index": 131080, "active": true}]}
```

`kind` is `music` (an A2DP profile), `call` (HSP/HFP; `music` can still be switched
to) or `off`. `music` lists the offered codecs in PipeWire's order. `set` re-reads
PipeWire every 0.4 s, for about 3 s in all (the `wpctl` call excluded), to confirm the switch, and does nothing when the codec is
already in use. The plugin runs `codec` when the Settings tab opens and, after you
turn LDAC on, polls it for at most 40 s to switch to LDAC once it is offered.
Whether the choice survives a reconnect has not been measured.

| Exit | Meaning |
|---|---|
| 0 | done (`set`: the new codec is in use) |
| 1 | other error |
| 2 | usage error (missing or invalid `--address`, unknown flag or argument) |
| 3 | unavailable: `pw-dump` or `wpctl` missing, PipeWire not running, or the earbuds are not in PipeWire; the plugin hides the row |
| 4 | failed: the codec is not offered, `wpctl` failed, or the switch was not confirmed; the plugin shows an error |

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
- `hearing run --apply` / `hearing apply` / `library apply` of a hearing-test card
  save the restore points (the active EQ and slot 11) as `curve-*.json` files in
  the backup directory, not as library entries; the commands they print
  (`qcyctl library apply --file <file> --yes`) restore them.
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

Commands that talk to the earbuds need `qcyctld`. With the socket unit enabled
(the plugin does this when it installs or updates, and Settings > Daemon >
"Start with the system" toggles it) systemd holds the daemon socket and starts
`qcyctld.service` on the first connection. Without it, a command whose socket
does not answer launches `qcyctld` detached (looked up next to `qcyctl`, then in
`~/.local/share/qcy-omarchy/bin`, `~/.local/bin` and finally `PATH`). Its output
goes to `${XDG_STATE_HOME:-~/.local/state}/qcy-omarchy/qcyctld.log` (truncated
above 1 MiB); concurrent starters are serialised with a lock, and a second
detached daemon exits quietly because the first holds a lifetime lock on
`$XDG_RUNTIME_DIR/qcyctld.lock`. Offline commands (`i18n`, `library list/show/...`,
`model`, `hearing plan|result`, `version`) never start it.

```sh
qcyctl daemon start              # idempotent
qcyctl daemon stop               # systemctl stop of the service when socket-managed
                                 # (the socket stays and reactivates it on demand),
                                 # else SIGTERM and wait for the process to exit
qcyctl daemon status [--json]    # running, pid, managedBySystemd, serviceInstalled,
                                 # serviceEnabled, socketActive, version, cliVersion,
                                 # binary, log
qcyctl daemon install-service [--json] [--quiet]
                                 # writes qcyctld.socket + qcyctld.service under
                                 # ~/.config/systemd/user, enable --now the socket and
                                 # try-restart the service (picks up a new binary);
                                 # safe to repeat; fails if no systemd user session
qcyctl daemon disable-service   # the Settings toggle OFF: disable --now the socket,
                                 # stop the service, keep the unit files (opt-out)
qcyctl daemon refresh-service   # run by the bootstrap after an update: no socket unit
                                 # file or socket enabled -> install-service; socket
                                 # disabled (opt-out) -> rewrite changed units, never
                                 # enable, stop the daemon so the next client starts
                                 # the new binary
qcyctl daemon uninstall-service  # disable --now the socket, stop the service,
                                 # remove both units (also "Remove QCY data…")
qcyctl version [--json]          # also: qcyctld --version
```

The service unit has no `[Install]` section (the socket activates it) and runs
the stable `~/.local/share/qcy-omarchy/bin/qcyctld` link, with a
`ConditionPathExists=` on it so a removed install does not leave a unit failing
in a restart loop, and `TimeoutStopSec=20`. A 0.1.5 unit that was enabled on its
own is disabled by `install-service`, and a daemon started the old way is stopped
(and awaited) before systemd binds the socket. The version shown by `status` is
informational: a daemon older than `qcyctl` is replaced by the next
`install-service` (the plugin runs it after each update) or by toggling the
setting off and on.

On SIGTERM the daemon stops accepting, lets the requests in flight finish (up to
15 s, so a backup/write/readback EQ sequence is not cut) and exits.

### One client at a time

The earbuds accept **one BLE client at a time**, so the daemon does not hold them
permanently. It connects on the first request, keeps the link for an idle window
and then disconnects, which frees the earbuds for the phone app. If another app
holds them, requests fail with a `busy` error ("earbuds are in use by another
app", on this or another device), the panel shows the last known state marked stale, and it recovers on the
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
downloads them again. The colour is, in order: `--color` (a colour name or
index, e.g. `Purple` or `2`); the colour the earbuds themselves advertise (the
daemon records it when it scans, since a connected unit stops advertising, so it
is known after the first scan-based connection and reported as `colorIndex` /
`colorSource: "device"` in the status `model` object); the product's default
colour. An advertised colour the product list does not contain falls back to the
default. The old `~/.config/qcy-omarchy/color` file is no longer read.

```sh
qcyctl model image [--vendor N] [--color <name|index>] [--refresh] [--no-network] [--json]
qcyctl model images off      # stop all downloads (on re-enables, status shows)
```

With downloads off, the panel uses only what is already cached, or its drawn
icons. Delete the cache directory to remove the files.

## Status: `eq.ears`

`status --json` adds `eq.ears` only while the earbuds report slot 11 (hearing
experience), the model's panel declares the hearing test and the daemon read both
ears (see PROTOCOL.md, "Reading both ears"); otherwise the key is absent:

```json
"eq": { "preset": 11, "masterGain": -10, "bands": [ ... ],
        "ears": { "left":  [ {"index":0,"freq":63,"gain":2,"q":70,"type":2}, ... 8 bands ],
                  "right": [ {"index":0,"freq":63,"gain":0,"q":70,"type":2}, ... 8 bands ] } }
```

`index` counts the band within the ear (0-7); `gain` is in dB. `hearing apply` and
`library apply` of a hearing-test card verify both ears with it ("both ears
confirmed by readback"); without it they keep the older check (left confirmed,
right "not independently verifiable").
