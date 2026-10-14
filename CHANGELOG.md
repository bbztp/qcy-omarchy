# Changelog

All notable changes to this project are documented in this file.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
the project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

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

[0.1.0]: https://github.com/bbztp/qcy-omarchy/releases/tag/v0.1.0
