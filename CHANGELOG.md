# Changelog

All notable changes to this project are documented in this file.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
the project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- `qcyctl library import-device --expect-slot N` saves nothing unless slot N is the one active on the earbuds.
- The Sound tab shows a "Hearing experience" placeholder (with a shortcut to the test) when the model declares the hearing test and the library has no entry for it yet; when the earbuds report that effect active and the entry is missing, the panel imports it once per session (read-only, never writes to the earbuds).

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

[0.1.3]: https://github.com/bbztp/qcy-omarchy/releases/tag/v0.1.3
[0.1.2]: https://github.com/bbztp/qcy-omarchy/releases/tag/v0.1.2
[0.1.1]: https://github.com/bbztp/qcy-omarchy/releases/tag/v0.1.1
[0.1.0]: https://github.com/bbztp/qcy-omarchy/releases/tag/v0.1.0
