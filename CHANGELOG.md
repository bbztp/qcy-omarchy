# Changelog

All notable changes to this project are documented in this file.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
the project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Fixed

- First-time setup no longer makes the shell reload the plugin over and over (and leave the widget missing until `omarchy restart shell`): the bootstrap now prepares the binaries in a hidden staging directory beside the plugin and installs them into `.bin` with a single rename, writing nothing under the plugin directory meanwhile; interrupted runs leave nothing behind and a failed update keeps the installed binaries.

### Fixed

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

[0.1.2]: https://github.com/bbztp/qcy-omarchy/releases/tag/v0.1.2
[0.1.1]: https://github.com/bbztp/qcy-omarchy/releases/tag/v0.1.1
[0.1.0]: https://github.com/bbztp/qcy-omarchy/releases/tag/v0.1.0
