# QCY Earbuds for Omarchy

A bar widget for the [Omarchy](https://omarchy.org) shell that controls QCY
earbuds (developed against the **MeloBuds N70 / HT18**, but written to the
vendor's low-energy GATT protocol) and shows their state:

- **Battery** in the bar (glyph; optional percentage), with per-ear and case
  levels plus charging flags in the panel.
- **Active noise control** — Off, Adaptive, Indoor, Commuting, Noisy,
  Anti-wind, Transparency.
- **Toggles** — LDAC (hi-res audio), Multipoint, Game mode (low latency),
  Sleep mode, Wind noise reduction.
- **Readout** — firmware, EQ preset + curve, balance, auto power-off, prompt
  volume.
- **Find my earbuds.**

It talks to the earbuds directly over Bluetooth Low Energy through a small Go
CLI, `qcyctl` — no phone, no vendor app, no background daemon.

## Requirements

- Omarchy (Quickshell-based shell).
- Go (to build `qcyctl`), or a prebuilt binary you drop somewhere on `PATH`.
- The earbuds paired with the machine.

## Install

```sh
git clone https://github.com/bbztp/qcy-omarchy
cd qcy-omarchy
./scripts/install.sh
```

This builds `qcyctl` into `~/.local/bin`, copies the plugin to
`~/.config/omarchy/plugins/io.github.bbztp.qcy`, and rescans plugins.

Then add the widget to your bar — put this in `~/.config/omarchy/shell.json`
under `bar.layout` (e.g. inside the `right` array), which hot-reloads on save:

```json
{ "id": "io.github.bbztp.qcy" }
```

or move it into place with `omarchy bar move io.github.bbztp.qcy --section right`.

## Usage

- **Left click** the bar button — open the panel.
- **Right click** — toggle the battery percentage in the bar.
- **Middle click** — refresh.
- In the panel — **Esc** closes, **r** refreshes, **f** flashes the earbuds.

The full state is read on connect and when the panel opens (each read connects
to the earbuds' low-energy service, so it takes a short scan — the panel shows
`Reading…` while it does).

## Settings

Configurable per widget in `shell.json` (or via the shell's settings UI):

| Key | Default | Meaning |
| --- | --- | --- |
| `showPercentage` | `false` | Show the numeric battery percentage in the bar. |
| `scanSeconds` | `20` | How long to scan for the earbuds' advertisement. |
| `deviceMatch` | `"qcy"` | Case-insensitive substring to recognise your earbuds. |
| `refreshOnOpen` | `true` | Read the state when the panel opens. |

Set `QCYCTL` in the environment to point the widget at a `qcyctl` binary that
is not in `~/.local/bin`.

## A note on safety

The EQ-write path (`qcyctl eq set`) overwrites the earbuds' single custom EQ
preset. That is exactly how a hand-tuned curve gets lost, so the CLI refuses to
write unless you pass `--yes`, and the widget itself only **reads** EQ. Nothing
in this project writes EQ automatically.

## `qcyctl`

The backend is also usable on its own:

```sh
qcyctl status --json          # full machine-readable snapshot
qcyctl status                 # human-readable snapshot
qcyctl anc noisy              # set ANC mode
qcyctl ldac on                # toggles: game|sleep|ldac|multipoint|wind
qcyctl find                   # flash the earbuds
```

Run `qcyctl` with no arguments for the full command list. Set `QCY_DEBUG=1` to
trace the GATT traffic on stderr.

## Layout

```
backend/                    Go source for qcyctl
plugin/io.github.bbztp.qcy/ the Omarchy bar widget (QML)
scripts/install.sh          build + install
```

## Credits

Protocol work builds on the public work in
[Quicky](https://github.com/hui1601/Quicky) and
[OpenQCY-Desktop](https://github.com/GiorgioRafael/OpenQCY-Desktop).

## License

MIT — see [LICENSE](LICENSE).
