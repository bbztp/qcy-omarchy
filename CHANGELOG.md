# Changelog

All notable changes to this project are documented in this file.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
the project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.8] - 2026-10-15

### Fixed

- A factory card (for example Rock) written to the custom slot by another computer or the phone app is now marked as playing. Before, only your own curves were recognised on that slot, so the panel kept marking a stale card or none. `qcyctl library matches --any` tries your curves first, then the factory cards, and never the hearing-experience card.
- The panel no longer marks any card when the earbuds' last report is stale (the earbuds were busy with another client), so an old hearing-experience report is not shown as live.
- Clicking a card that is already being applied is ignored. Before, repeated clicks queued the same write several times, and the hearing card could stay on "applying".
- An apply whose queued request is dropped (for example when the earbuds disconnect) now ends with a "not sent" error instead of staying on "applying".

## [0.1.7] - 2026-10-15

### Fixed

- The hearing-experience card is marked as playing only when the earbuds report the hearing-test slot (11). Before, a custom curve on slot 255 that happened to equal the average of both ears marked the card as playing, although the earbuds were playing a custom curve.
- After a hearing-test curve is written, both ears are confirmed by readback. Before, only the left ear was confirmed.

### Added

- The earbuds report both hearing-test ears while the hearing experience is active (slot 11). This was measured on a MeloBuds N70 with firmware 3.0.13: a query for 0x46 or 0x47 is answered with a 0x22 frame that carries that ear. The daemon reads both ears and exposes them as `eq.ears` (`left`/`right`) in `qcyctl status --json`, only for models whose panel declares the hearing test.
- When the hearing experience is active on the earbuds (set from the phone app or another computer) and there is no saved hearing card, the panel imports both ears into the "Hearing experience" card on its own, as if the test had been done here. A card left by 0.1.4–0.1.6 without the ears is replaced once, after a backup.
- `qcyctl library import-device --require-ears` saves nothing unless both ears were read. `qcyctl library matches --slot N --ears json` compares both ears.

## [0.1.6] - 2026-10-14

### Changed

- The daemon now runs under systemd socket activation by default: the setup installs `qcyctld.socket` and `qcyctld.service` in `~/.config/systemd/user/` and enables the socket. The daemon starts on the first request and owns no socket file of its own. After an update the setup runs `qcyctl daemon refresh-service`, which restarts a running daemon on the new binary (`try-restart`). Requests sent during that restart wait in the socket queue instead of failing.
- The Settings toggle "Start with the system" now enables or disables the socket (`qcyctl daemon install-service` / `disable-service`). Turning it off keeps the unit files, and later updates respect the choice. `qcyctl daemon uninstall-service` (and "Remove QCY data…") removes both units.
- Upgrading from 0.1.5: a daemon left by 0.1.5 is stopped (waiting for the process to exit) and an enabled 0.1.5 `qcyctld.service` is disabled before the socket is enabled. 0.1.5 removed the unit file when its toggle was turned off, so 0.1.6 cannot see that choice and enables the socket; turn the toggle off again if you prefer to start the daemon yourself.
- Without a systemd user session the daemon keeps working as before (started by qcyctl). A lock in `$XDG_RUNTIME_DIR` keeps a second daemon from starting, and only the lock holder removes the socket file.
- `scripts/install.sh` installs the units through `qcyctl daemon install-service`. `packaging/qcyctld.service` is gone.
- The panel tabs (Status, Sound, Settings) stay fixed at the top while the content scrolls, and switching tabs starts the new tab at its top.

### Fixed

- The widget no longer appears and disappears with the daemon's BLE sessions. Only a paired, bonded or trusted BlueZ device counts as the connected earbuds.
- On SIGTERM the daemon stops accepting requests and waits up to 15 s for the ones in progress (such as an EQ backup, write and readback) before it exits. A request it refuses while stopping is retried by qcyctl on the new daemon. A request that was already sent is never retried.
- The earbuds cache (`~/.cache/qcyctl/le-address`) is written atomically under a lock and is never rewritten with fewer fields. A cache without the model is repaired with a short scan, so the earbuds no longer fall back to a generic model.
- Settings: the daemon state is read back after you toggle the service, instead of showing a stale value.

### Added

- The Settings footer shows the plugin, daemon and qcyctl versions, plus a hint when the running daemon is older than the plugin. `qcyctl daemon status --json` reports `version` and `cliVersion`.

## [0.1.5] - 2026-10-14

### Fixed

- After `omarchy plugin update` the plugin prepares the new binaries without `omarchy restart shell`: the loaded `Qcy` singleton watches `manifest.json` (read-only) and repeats the setup when its version changes or the setup had failed.

## [0.1.4] - 2026-10-14

### Added

- `qcyctl library apply --file <curve.json>` applies a curve file (such as a restore point) without adding it to the library.
- The bar widget stays visible while the setup is running or has failed (tooltip shows the error), and hides again once ready and the earbuds are away.
- `qcyctl library import-device --expect-slot N` saves nothing unless slot N is the one active on the earbuds.
- The Sound tab always shows the "Hearing experience" card when the model declares the hearing test; with no saved curve it has a "How to get it" button (use the phone app curve and refresh, or run the hearing test here); when the earbuds report that effect active and the entry is missing, the panel imports it once per session (read-only, never writes to the earbuds).
- When the earbuds report slot 11 active but its bands or master gain differ (hearing-test cards compare the last 8 reported bands with the left ear, ignoring the prefix the previous effect leaves; `qcyctl library matches <id> --master dB --bands json` exposes the rule) from the saved hearing card (test redone on the phone), the card is no longer marked as playing, the chart shows the earbuds' curve, and a notice offers to overwrite the saved card (optionally backing it up first) or save the curve as a new custom one. The notice is not repeated for the same payload in a session.
- `qcyctl library import-device` gains `--replace` (overwrite the existing entry, requires `--expect-slot`; an entry with per-ear curves also requires `--backup`), `--backup` (with `--replace`: copy the old entry to the backups directory first) and `--user` (store as a custom curve). Still read-only on the earbuds.
- With Personalizado (slot 255) active, the earbuds' curve is compared with all your own curves (`qcyctl library matches --any-user`): the matching card is marked; when none matches, a notice offers to save it as a new custom curve (`import-device --expect-slot 255 --user`). Not raised while the panel is writing the EQ or composing on a base.

### Changed

- The restore points of `library apply` (hearing-test card), `hearing apply` and `hearing run --apply` (the active EQ and slot 11 before the write) are now `curve-*.json` files in `~/.local/share/qcy-omarchy/backups/` instead of library entries, so they no longer fill the Sound tab with cards. The slot 11 backup requirement (and `--no-slot11-backup`) is unchanged.
- Device images follow the colour the earbuds advertise (`--color` still wins, then the product default). The daemon records the colour during its scan and reports it as `model.colorIndex` / `model.colorSource`; the scan reads the raw advertisement itself. The `~/.config/qcy-omarchy/color` file is no longer read.

### Fixed

- The footer no longer shows "Updated N min ago" for old data: the age is cleared on disconnect and on a failed read, and a cached reading is reported with its age in the error line.
- A failed read is always reported in the panel, even while the earbuds are not connected; Refresh never does nothing silently (a stuck "Reading…" is released after 25 s).

## [0.1.3] - 2026-10-14

### Changed

- `qcyctl` and `qcyctld` now live in `~/.local/share/qcy-omarchy/bin/<version>` (stable `qcyctl`/`qcyctld` links beside it), not in the plugin folder. `omarchy plugin remove` does not delete them; the daemon exits by itself within about 30 s once the plugin folder is gone, and "Remove QCY data…" deletes them. A leftover `<plugin dir>/.bin` from an earlier build is ignored; remove it by hand.
- "Remove QCY data…" stops the daemon before deleting, and its confirmation mentions the downloaded programs.
- A systemd unit written by `qcyctl daemon install-service` points at the stable `bin/qcyctld`, so it survives upgrades.

### Fixed

- First-time setup no longer makes the shell reload every plugin (and leave the widget missing until `omarchy restart shell`): the shell reloads on any write under `~/.config/omarchy/plugins`, so the bootstrap now downloads, builds and installs entirely in `~/.local/share/qcy-omarchy` and never writes under the plugin directory; interrupted runs leave nothing behind and a failed update keeps the installed binaries.
- The equalizer graph now follows the bands the earbuds report when the library has no entry for the effect in use (for example the hearing experience on a computer without the local backup), instead of drawing a flat line.

## [0.1.2] - 2026-10-14

### Fixed

- A status read that gets no answer at all (no battery and no ANC, typically because another app took the earbuds and left a dead session) now fails with `not_found` (or `busy`) and drops the session so the next request reconnects, instead of reporting an empty success; the sweep also aborts after the first two queries instead of waiting out every toggle.
- The panel now says so when the earbuds cannot be read because another app (on this or another device) holds them, instead of silently showing the last reading; a manual refresh always runs, and a failed change is reverted with the reason shown.
- While the panel is open it keeps retrying (about every 20 s) when the earbuds are held by another app or unreadable, so it reconnects by itself once the other app is closed; a read or change that never answers is cut off with a visible error instead of leaving the panel unresponsive.

## [0.1.1] - 2026-10-14

### Changed

- The busy and not-found errors now ask you to check whether another app, on this or another device (phone, tablet, computer), is open and holding the earbuds.

## [0.1.0] - 2026-10-14

First release.

### Added

- Bar widget for the Omarchy shell that controls QCY earbuds over Bluetooth Low Energy, with no phone app.
- Battery for the left earbud, right earbud and case, with charging flags.
- Active noise control and listening modes (off, adaptive, indoor, commuting, noisy, anti-wind, transparency), plus LDAC, multipoint, game mode, sleep mode and wind noise reduction toggles.
- Equalizer with a 10-band editor, the effect library and a personal library of saved curves.
- Hearing test that builds a personal listening profile.
- Find my earbuds (ring each earbud).
- Touch controls: assign functions to tap, double tap, triple tap, quadruple tap and long press.
- Device pictures in the panel, fetched on demand and cached.
- Interface in English and Portuguese (Brazil), following the system language, with user-supplied translations.
- Automatic setup: the plugin downloads the matching `qcyctl` and `qcyctld` release, verifies its SHA-256, and installs it inside the plugin's own `.bin` directory, so removing the plugin removes the programs (falls back to building from source with Go); the daemon starts on demand, exits by itself when its executable is removed, and can optionally run as a systemd user service.
- Features follow each model's own panel: nothing is blocked because a model was not tested on hardware (the panel only says so). Prompt volume steps, wear sensitivity items, noise modes and equalizer geometry come from the model; features whose encoding is not understood are listed under "Not available on this model".

[0.1.5]: https://github.com/bbztp/qcy-omarchy/releases/tag/v0.1.5
[0.1.4]: https://github.com/bbztp/qcy-omarchy/releases/tag/v0.1.4
[0.1.3]: https://github.com/bbztp/qcy-omarchy/releases/tag/v0.1.3
[0.1.2]: https://github.com/bbztp/qcy-omarchy/releases/tag/v0.1.2
[0.1.1]: https://github.com/bbztp/qcy-omarchy/releases/tag/v0.1.1
[0.1.0]: https://github.com/bbztp/qcy-omarchy/releases/tag/v0.1.0
