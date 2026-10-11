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
| `0x22` | EQ v2 read **and** write | see [EQ](#eq) | verified read and write; every write is read-modify-write with a backup |
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
| eq `0x22` | honours the write; only written through a read-patch-backup-write transaction (see [EQ](#eq)) |
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

**Verified read and write on `0x22`.** The device **reports 143 bytes**: 73 known bytes followed by 70 bytes that
have always read zero. Only the 73 known bytes are ever written (the 73-byte
form is the proven write format). The known layout:

```
[slot:1][masterGain:int16 LE][10 x (freq:u16 LE, gain:int16 LE, q:u16 LE, type:u8)]
 byte 0   bytes 1-2            bytes 3-72, 7 bytes per band
```

- **Slot** (byte 0): the active preset slot. Slot 11 is the manufacturer's
  hearing-test profile; the plugin maps slots 0-4 to Pop, Rock, Classical, Jazz
  and Bass Booster (names not independently verified). The device only reports the *active* slot; an inactive
  slot cannot be read back.
- **Master gain** (bytes 1-2): signed int16 in centi-dB, so `0x0096` is
  +1.50 dB. The client accepts -8..+8 dB (-800..+800) and rejects NaN, Inf and
  anything outside that range before converting to an integer.
- **Bands** (bytes 3-72): `[freq:2][gain:2][q:2][type:1]` each. Gain is signed
  centi-dB. **Frequency, Q and type are not fixed**: the hearing-test slot (11)
  carries per-user values, so a writer must preserve them. An earlier
  implementation synthesised a fixed 31 Hz ... 16 kHz table with Q=1.00 and
  type 0; that destroyed the hearing-test curve and is gone.

### Units in `status --json`

`eq.preset` is the active slot (byte 0), `eq.masterGain` is the master gain in
**dB** (float, int16 / 100), and `eq.bands[].gain` is in dB too, `freq` in Hz as
stored on the device. `qcyctl eq get --json` and the results of the write
commands additionally carry each band's `q` and `type`, the raw `payload` (hex)
and `slot`.

### Writing: read, patch, back up, write, confirm

There is no select-only command and no way to read an inactive slot, so a write
always starts from the payload the device reports **now** and changes only the
bytes that were asked for:

| Command | Bytes changed |
|---|---|
| `qcyctl master-gain <dB>` | 1-2 only |
| `qcyctl eq set <g1..g10> --yes` | the 10 band gains; master gain kept unless `--master-gain dB` |
| `qcyctl eq set ... --preset N` | byte 0 as well: the current curve is written into slot N, which also makes N active |

Order of operations (done under one transaction lock in the daemon):

1. read `0x22` (143 bytes expected; at least 73 accepted; refused if all 0xFF or
   if any byte beyond 73 is non-zero, since its meaning is unknown);
2. patch a copy of the first 73 bytes;
3. **save the full payload read in step 1** (all 143 bytes; see Backups); if this fails the write is
   **aborted** and the error is returned;
4. write the patched 73-byte payload;
5. confirm by readback (echo may be 143 bytes; the first 73 are compared): slot,
   master gain and every band gain must echo.

The earbuds first answer a query with the pre-change state, so confirmation
keeps re-querying until the echo matches or the deadline passes.

Writing `0x22` does not restart the earbuds (unlike `0x23` LDAC or `0x24`
multipoint). Writing into a slot replaces whatever curve that slot held and
that content cannot be read first when the slot is inactive, so the backup
covers the payload that was *active*, not the target slot's previous content.

### Backups

Before every `0x22` write (direct or through the daemon, including slot 11) the
payload read from the device is stored as one line of lowercase hex in
`~/.local/share/qcy-omarchy/backups/` (`$XDG_DATA_HOME/qcy-omarchy/backups/`).
The directory is mode 0700, files 0600, and nothing is ever written to
`~/.cache`.

| File | Meaning |
|---|---|
| `eq-slot<N>-first.hex` | the first payload ever seen for slot N; created with no-replace semantics, never overwritten |
| `eq-slot<N>-<UTC timestamp>.hex` | one file per write, timestamp `YYYYMMDDTHHMMSS.ffffffZ`; append-only |
| `legacy-eq-backup*.json` | copies of the old `~/.cache/qcyctl/eq-backup*.json`, made once; the originals are left in place |

Each file is written to a temp file, fsynced, then hard-linked into place, so a
crash never leaves a half-written backup and an existing file is never replaced.
Restoring is a manual `eq set`-style write of the saved bytes; there is no
restore command yet.

### CLI

- `qcyctl eq get [--json] [seconds]`: the active slot, master gain and bands.
  `--preset N` is **not supported** and returns an error (it used to write to
  characteristic `000B`, which this model does not have, and never restored it).
- `qcyctl eq set <g1,...,g10> --yes [--preset N] [--master-gain dB] [--json] [seconds]`
  gains in dB. Out-of-range values are rejected, not clamped. Without
  `--preset` the target is the currently active slot; without `--master-gain`
  the device's current master gain is kept.
- `qcyctl master-gain <dB> [--json] [seconds]`: master gain in dB, -8..+8.
- `qcyctl eq preset <n>`: writes characteristic `000B`; fails on this model
  because the characteristic is absent.

All of these go through the daemon when it is running, and fall back to a
direct connection when it is not.

## Restarts caused by writing

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

## Daemon socket protocol

`qcyctld` listens on `$XDG_RUNTIME_DIR/qcyctld.sock` (mode 0600); without
`XDG_RUNTIME_DIR` it uses `/tmp/qcyctl-<uid>/qcyctld.sock` in a directory that
must be owned by the user with mode 0700, otherwise the daemon refuses to start
and clients ignore it. A second daemon never replaces a live one. Requests are
one JSON object per line, `{"cmd": ..., "args": {...}}`; replies are
`{"ok": true, "data": ...}` or `{"ok": false, "error": "..."}`. Clients set a
90 s deadline per round trip.

| `cmd` | `args` | `data` |
|---|---|---|
| `status` | none | raw per-opcode dump (`eq` is the raw 0x22 byte array) |
| `battery` | none | battery object |
| `capabilities` | none | model capabilities |
| `set` | `action`, `params`, optional `expect` | confirmed payload bytes or null; `action: "eq"` is refused |
| `eq` | none | `{slot, masterGain (dB), bands[{freq,gain,q,type}], payload (hex)}` |
| `eqApply` | optional `gains` (10 ints, centi-dB), `masterGain` (int, centi-dB), `slot` (int) | the `eq` object plus `previous`, `backupFirst`, `backupHistory` |
| `eqMasterGain` | `masterGain` (int, centi-dB), required | same as `eqApply` |
| `wear` | `state`: 1 on / 2 off | confirmed `0x2C` payload (byte array) |
| `noise` | none | raw `0x17` payload `[mode, scene, level]` |
| `find`, `keys`, `keysSet`, `disconnect` | as before | |

`eqApply`, `eqMasterGain` and `wear` read, patch and write under a single
transaction lock, so a concurrent status sweep cannot interleave. `eqApply` is
the only way to write `0x22`.

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
| `QCYCTL_DEBUG=1` | per-frame trace on stderr (`qcyctld`) |
| `QCY_DEBUG=1` | per-frame trace on stderr (`qcyctl`, direct mode) |
| `QCYCTL_PROBE=1` | enumerate service `a001` with read results |