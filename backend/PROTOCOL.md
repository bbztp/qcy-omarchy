# Protocol reference

Notes on the QCY BLE protocol as observed on this hardware — a MeloBuds N70,
LE address `AA:BB:CC:DD:EE:FF`.

Anything marked **verified** was read back from the earbuds. Anything marked
**unverified** exists in the device's command space but has not been exercised,
and its name is provisional until it is.

## Packet format

**Verified.** Every frame is:

```
FF <bodyLen> <cmdId> <paramLen> <params...>
```

`bodyLen` counts every byte after itself, so it equals `paramLen + 2`.

A **query** for a cmdId is the same frame with `cmdId = 0xFE` and the target
cmdId as its single parameter. Asking for ANC (`0x17`) is therefore
`FF 03 FE 01 17`.

A notification can carry several blocks back to back, so parsing must loop over
the buffer rather than assuming one block. `buildFrame` and `parseFrames` must
stay byte-identical to each other; an earlier off-by-one in `parseFrames` read
the `paramLen` byte as data and decoded balance as 1 instead of 50.

## Command table

Payloads marked verified were confirmed by reading the value back. The on/off
convention throughout the device is **`0x01` = on, `0x02` = off** — the earbuds
answer a game-mode query with `0x02` when it is off.

| Cmd | Name | Payload | Status |
|---|---|---|---|
| `0x05` | LED flash | `[1, 1]` on / `[1, 0]` off | verified, no visible effect while connected |
| `0x07` | ANC strength | `[1, level]` | **verified: no response on this model** |
| `0x09` | game / low latency | `[1, 1|2]` | verified |
| `0x0C` | ANC mode | `[1, mode]` | unverified |
| `0x10` | sleep mode | `[1, 1|2]` | verified |
| `0x11` | fit test | `[2, 1, 1]` start / `[2, 2, 2]` stop | unverified; no query reply on this model |
| `0x14` | timed power-off | `[4, lo, hi, 0, 0]` | verified, 4 bytes not 2 |
| `0x16` | L/R balance | `[1, value]` | verified |
| `0x17` | ANC setting | `[3, mode, subScene, level]` | verified |
| `0x1D` | prompt volume | `[1, value]` | verified read-only, writes ignored, max 15 |
| `0x1E` | music control | `[1, action]` | unverified |
| `0x20` | EQ v1 write | — | unverified |
| `0x22` | EQ v2 read **and** write | see below | verified read-only |
| `0x23` | LDAC | `[1, 1\|2]` | verified: reboots the earbuds |
| `0x24` | multipoint | `[1, 1\|2]` | verified: reboots the earbuds, expected |
| `0x2A` | wind noise reduction | `[1, 1\|2]` | verified (added — was missing from this table) |
| `0x2A` | wind detection | `[1, 1\|2]` | verified |
| `0x2C` | wear detection | `[3, enabled, …]` | verified |
| `0x2D` | spatial audio | `[1, 1\|2]` | unverified; no query reply on this model |
| `0x35` | EQ test | — | unverified |
| `0x3D` | play a tone | `[1, toneId]` | verified: no audible effect |
| `0x43` | touch remap | — | via characteristic `000d` |

`0x07`, `0x11`, `0x2D` and `0x35` respond to a query on at least some
firmware; on this unit only `0x07`, `0x11` and `0x2D` are ever asked, and all
three stay silent, which is what makes `spatial`, `fit` and `ancLevel` report
as unsupported.

`0x07` not answering on this model is also why ANC strength is carried in the
third byte of `0x17` instead of being set directly.

## Setter behaviour measured on this unit

Every wired setter was exercised through the daemon and read back with
`qcyctl status --json`:

| Setter | Result |
|---|---|
| balance `0x16` | honours the write |
| auto-off `0x14` | honours the write |
| game `0x09` | honours the write |
| sleep `0x10` | honours the write |
| wind `0x2A` | honours the write |
| wear `0x2C` | honours the write |
| prompt volume `0x1D` | **ignores the write**, always reports 6 of 15 |
| ANC `0x17` | not exercised — rejected unless both earbuds are worn |
| spatial `0x2D`, fit `0x11` | no query reply on this model |
| eq `0x22` | read-only by design (see below) |
| ldac `0x23` | reboots the earbuds; refused |

Game, sleep and wind read back `false` after a reconnect and wind persisted
across one, so treat their post-reconnect value as unsettled rather than as
persisted state.

## ANC

**Verified.** `0x17` is `[mode, subScene, level]`, read back in the same order.
A mode can be packed as `(mode << 16) | (subScene << 8) | level`; mode 3 /
subScene 1 / level 0 is a special case that gets rewritten to `(3, 2, 0)`.

Level behaviour on this model:

- 0, 1 and 2 are accepted; anything above 2 is clamped to 0
- indoor, commuting and transparency honour the level
- adaptive and anti-wind always read back 0 regardless of what is written

## EQ

**Verified read/write from `0x22`:**

The opcode format is `[preset:1][masterGain:2][10 × (freq:2, gain:2, q:2, type:1)]` — 73 bytes total.

- **Preset index** (byte 0): selects which slot to read/write. Slot 11 is the manufacturer's hearing-test profile, backed up to `~/.cache/qcyctl/eq-backup.json`. Other slots hold user presets.
- **Master gain** (bytes 1–2, signed int16 in centi‑dB): global gain offset. Clamped to ±8 dB (–800…+800 centi‑dB). A value of `0x0064` is +1.0 dB.
- **10 bands** (bytes 3–72): fixed centre frequencies 31 Hz, 62 Hz, 125 Hz, 250 Hz, 500 Hz, 1 kHz, 2 kHz, 4 kHz, 8 kHz, 16 kHz. Each band is `[freq:2 bytes][gain:2 bytes][q:2 bytes][type:1 byte]`. Type `0` = peaking EQ.

**Bidirectional:** the same layout is used to read and to write. Writing overwrites the slot at the given preset index. There is no select‑only command, and an inactive slot cannot be read back, so switching presets destroys the curve in the target slot.

**Backup:** writing to slot 11 is automatically backed up to `~/.cache/qcyctl/eq-backup.json`. Writing to any other slot is backed up to `~/.cache/qcyctl/eq-backup-slotN.json` (N = preset number). Backups store the preset number, timestamp, and the 10 band gains.

**CLI commands:**

- `qcyctl eq get [--preset N] [seconds]` — read the current or a specific preset's bands
- `qcyctl eq set <g1,g2,…,g10> [--yes] [--preset N] [--master-gain N] [seconds]` — write gains (in dB) to a preset slot, with confirmation
- `qcyctl master-gain <value> [seconds]` — set the master gain in dB (e.g. `0`, `‑3.5`, `+2.0`)
- `qcyctl eq preset <n> [seconds]` — write the preset index to characteristic 000B

**Level behaviour on this model:**

- Gains are stored as hundredths of a dB (`0x0064` = +1.0 dB, `0x0200` = +2.0 dB, etc.)
- Master gain is clamped to ±8 dB
- Writing to an uninitialized slot will overwrite whatever data happens to be there; always backup first

**Restart caused by writing:** writing `0x22` does not restart the earbuds (unlike `0x23` LDAC or `0x24` multipoint).## Restarts caused by writing

Two writes restart the earbuds, for different reasons:

- **Multipoint (`0x24`) — expected.** The product documentation states the
  earphones restart each time dual-device connection is toggled.
- **LDAC (`0x23`) — a firmware fault.** Nothing documents this restart, so the
  setter is refused rather than allowed to reset the user's hardware on a
  click.

## Find my earbuds

An audible tone cannot help here: a lost earbud is by definition not in your
ear, so it could never play anything the owner could hear. What would actually
work is the indicator blinking — `0x05` — which is why that is what the button
sends.

Neither `0x05` nor `0x3D` produced any change while the earbuds were connected.
The untested hypothesis is that find only applies while disconnected, which is
the state they would actually be lost in.

## Prompt volume

`0x1D` reads and writes cleanly but the device keeps reporting 6 regardless of
what is written. The CLI's readback confirmation therefore fails loudly rather
than reporting a success it cannot back up.

Measured on this unit: writing 0, 3, 9, 12 and 15 all read back `06 0f` —
value 6, max 15. The write is accepted and discarded; the reply is not an echo
of the last value sent.

This is why prompt volume is listed in `readOnlyCmds` in the daemon rather than
being probed. A query cannot establish writability: this device answers with a
perfectly plausible maximum and then ignores the write, so the reply is
indistinguishable from a working setter that happens to sit at 6. Probing by
writing a different value would change the user's volume on every connection,
so the measured behaviour is recorded instead. The `0x1D` query still runs, and
`sanePromptVolume` still rejects a malformed reply — but both only answer "does
this unit have the command", never "will it obey it".

The reported maximum is 15, not the 255 the single-byte payload would allow.

## Characteristic layout (service `a001`)

Enumerated with `QCYCTL_PROBE=1`:

| UUID | Purpose | Readable |
|---|---|---|
| `00001001` | command write | no |
| `00001002` | command notify | yes |
| `00000008` | battery, L/R/case; bit 7 = charging | yes |
| `00000007` | firmware version, 3 or 6 bytes | yes |
| `0000000d` | touch remap, 10 key:fun pairs | yes |
| `0000000b` | absent on this model | — |

Touch ids: the low byte is a key index (`0x01`–`0x0a` for left/right
single, double, triple, quad and hold; `0x15`–`0x1e` repeat the scheme for
in-call gestures) and the high byte a function (`0x00` none, `0x01` play/pause,
`0x02` previous, `0x03` next, `0x04` voice assistant, `0x05`/`0x06` volume,
`0x07` game mode, `0x08`–`0x0a` call handling, `0x0b` redial). Ids outside
those ranges are shown as raw hex rather than guessed at.

## Supporting more than one model

The cmdId space above is the same across this product family — it is the
device's protocol, not one model's. What differs between models is **which
subset a given unit answers**. So a capability-driven plugin does not need a
hard-coded list per model: probe the candidate cmdIds once, keep the ones that
reply, and drive the UI from that.

Two probes already exist for exactly this: `cmd-constant` asks the device
how many EQ slots it has, and the ANC level range differs by model.

## Debug switches

| Variable | Effect |
|---|---|
| `QCYCTL_DEBUG=1` | per-frame trace on stderr |
| `QCYCTL_PROBE=1` | enumerate service `a001` with read results |