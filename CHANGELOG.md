# Changelog

All notable changes to this project are documented in this file.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
the project uses [Semantic Versioning](https://semver.org/).

## [0.1.0] - 2026-10-13

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
- Automatic setup: the plugin downloads the matching `qcyctl` and `qcyctld` release, verifies its SHA-256, and installs it under the user's data directory (falls back to building from source with Go); the daemon starts on demand and can optionally run as a systemd user service.

[0.1.0]: https://github.com/bbztp/qcy-omarchy/releases/tag/v0.1.0
