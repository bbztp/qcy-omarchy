# Protocol reference

Notes on the QCY BLE protocol. These behaviours were derived and verified on a QCY MeloBuds N70, the device used as the test model for this code; other models are unverified.

Verified on: QCY MeloBuds N70 (firmware 3.0.13), LE address `AA:BB:CC:DD:EE:FF`.

Which model a unit is comes from the advertised `vendorId`, not from this
protocol: see [Model detection](#model-detection) and `WEB_API.md`.

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

## Registry in code

The command table is also embedded in the binaries as `internal/protocol`:
opcode constants (`protocol.OpANC`, ...), the single frame envelope
(`protocol.Block`, `protocol.Query`, `protocol.ParseBlocks`) and the reference
table (`protocol.Lookup`, `ByName`, `All`). `internal/protocol/table.json` is
generated from `docs/reference/protocol.json` by `scripts/gen_protocol.py`
(`make protocol` or `go generate ./internal/protocol`); do not edit it by hand.

## Command table

Payloads marked verified were confirmed by reading the value back. The on/off
convention throughout the device is **`0x01` = on, `0x02` = off** — the earbuds
answer a game-mode query with `0x02` when it is off.

| Cmd | Name | Payload | Status |
|---|---|---|---|
| `0x05` | LED flash | `[1, 1]` on / `[1, 0]` off | verified, **no effect** while connected; the vendor app does not use it for "find" (see Find my earbuds) |
| `0x07` | ANC strength | `[1, level]` | **verified: no response on this model** |
| `0x09` | game / low latency | `[1, 1|2]` | verified |
| `0x0C` | ANC mode | `[1, mode]` | unverified |
| `0x10` | sleep mode | `[1, 1|2]` | verified |
| `0x11` | fit test | write `[1, 1, 1]` start / `[0, 0, 0]` stop; notification `[status, left, right]` (status 2 = final, 0 = ready; per ear 3 sealed, 2 good, 1 fair, 0 poor); never queried | read in the app; **verified on hardware 2026-10-13**: `[1,1,1]` plays the test sound and the final notification arrived as `[2, 1, 3]` (left poor, right sealed); the old `[2,1,1]` / `[2,2,2]` frames got no reply |
| `0x14` | timed power-off | `[4, lo, hi, 0, 0]` | verified, 4 bytes not 2 |
| `0x16` | L/R balance | `[1, value]` | verified |
| `0x17` | ANC setting | `[3, mode, subScene, level]` | verified |
| `0x1D` | prompt volume | query reply `[value, max]`; write `FF 04 1D 02 <v> 00` (two parameter bytes), v in 4/6/8/10 (Quieter / Medium / Louder / Maximum) | read in the app; **verified on hardware 2026-10-13**: `FF 04 1D 02 v 00` for v = 4, 6, 8, 10 each read back as written; one-byte writes of 0/3/9/12/15 were ignored (see [Prompt volume](#prompt-volume)) |
| `0x48` | wear-sensor sensitivity | write `FF 03 48 01 <p>`, p 1 medium / 2 normal / 3 high; query `FF 03 FE 01 48` -> `<p>` | read in the app; **verified on hardware 2026-10-13**: 1, 2 and 3 each read back as written (value was 2). Only meaningful with wear `0x2C` on |
| `0x1E` | music control | `[1, action]` | unverified |
| `0x20` | EQ v1 write | — | unverified |
| `0x22` | EQ v2 read **and** write | see [EQ](#eq) | verified read and write; every write is read-modify-write with a backup |
| `0x23` | LDAC | `[1, 1\|2]` | restarts the earbuds (the official panel declares it: "Turning on LDAC will restart the device"); written on any model whose panel declares it (cmd 35); **not verified on hardware**: the readback after the restart is unobserved |
| `0x24` | multipoint | `[1, 1\|2]` | verified: reboots the earbuds, expected |
| `0x2A` | wind noise reduction | `[1, 1\|2]` | verified (added — was missing from this table) |
| `0x2A` | wind detection | `[1, 1\|2]`; off is **0x00** as the app writes it (**verified on hardware 2026-10-13**: `FF 03 2A 01 00` reads back 0x00); 0x02 also reads as off (also verified) | verified |
| `0x2C` | wear detection | `[3, enabled, …]`; byte 0 = 1 on, **0x00 off** (what the app writes; **verified on hardware 2026-10-13**: `FF 05 2C 03 00 ..` reads back 0x00), 0x02 also reads as off (also verified); the rest of the payload is written back as read | verified |
| `0x2D` | spatial audio | `[1, 1\|2]` | unverified; no query reply on this model |
| `0x35` | EQ test | — | unverified |
| `0x3D` | play a tone | `[1, toneId]` | verified: no audible effect |
| `0x43` | touch remap | — | via characteristic `000d` |

`0x07`, `0x11`, `0x2D` and `0x35` respond to a query on at least some
firmware; on this unit only `0x07`, `0x11` and `0x2D` are ever asked, and all
three stay silent, which is what makes `spatial`, `fit` and `ancLevel` report
as unsupported.

Measured on a MeloBuds N70 (firmware 3.0.13) on 2026-10-15 with read-only
query frames, with the earbuds both out of and in the ears: `FF 03 FE 01 2D`,
`FF 03 FE 01 07` and `FF 03 FE 01 11` get no reply (each asked twice in the
ears, 3 s window), and neither do `FF 03 FE 01 20` and `FF 03 FE 01 35`;
`FF 03 FE 01 17` and `FF 03 FE 01 22` answer. The fit test result is only
delivered as the notification during a test, never on a query.

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
| prompt volume `0x1D` | one-byte writes of 0, 3, 9, 12, 15 were **ignored** (always 6 of 15); the app's two-byte frame `FF 04 1D 02 v 00` with v = 4, 6, 8, 10 **honours the write** (verified 2026-10-13) |
| wear sensitivity `0x48` | `FF 03 48 01 p` with p = 1, 2, 3 honours the write (verified 2026-10-13) |
| ANC `0x17` | not exercised — rejected unless both earbuds are worn |
| spatial `0x2D` | no query reply on this model |
| fit `0x11` | no query reply (the app never queries it); start `FF 05 11 03 01 01 01` / stop `FF 05 11 03 00 00 00` verified 2026-10-13: the final notification `[2, left, right]` arrives |
| eq `0x22` | honours the write; only written through a read-patch-backup-write transaction (see [EQ](#eq)) |
| ldac `0x23` | an earlier test rebooted the earbuds; the restart is declared by the official panel, so the write is allowed on any model whose panel declares it (`qcyctl ldac <on\|off> --yes`); **not verified on hardware since this change** |

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

**Verified read and write on `0x22`.** The device **reports 143 bytes**:

```
[slot:1][masterGain:int16 LE, centi-dB][20 x (freq:u16 LE, gain:int16 LE, q:u16 LE, type:u8)]
 byte 0   bytes 1-2                      bytes 3-142, 7 bytes per band position
```

A position with `freq` 0 is empty (factory presets mark it `0,0,0,type 0xFF`,
the custom slot leaves it all zero). The factory preset "Classico" (slot 6)
uses 5 positions; the user's custom curve (slot 11) uses 13 (positions 0-12;
positions 10-19 are the "tail", bytes 73-142).

**Write format.** The 73-byte write (positions 0-9, tail omitted) is the
**proven** format and is used whenever the tail holds no band (all zero, or
only the `0xFF` type marker of an empty position). If any position 10-19 is
populated, the payload to write is the full length read (143 bytes). That
format is **not proven on the device** for the factory slots, so it is
**refused** unless the caller passes `--full-length` (CLI) / `allowFull: true`
(socket); the result then carries `fullLength: true` and the CLI prints a
warning. **Exception: an explicit target of slot 255** (`slot: 255` /
`--slot 255`, the phone app's "Personalizado" buffer): the app itself writes
its custom curves there with up to 143 bytes (measured), so that target implies
`allowFull`. The allowance looks at the explicit slot only: any other target
keeps the requirement, and so does a slot-less patch while slot 255 happens to
be active. A patch carries everything not named in the command over byte for
byte from the payload just read.

**Build mode (`bands`).** A caller may instead give the whole curve as
structured bands (`{index, freq, gain (dB), q, type}`; library presets use
this). The payload is then **constructed** from those bands, never copied from
the payload read: byte 0 is the target slot, bytes 1-2 the master gain,
positions not named are all zero (freq 0, type 0, no `0xFF` marker). The
payload read is used only as proof of life and for the backup. Positions 0-9
give the proven 73-byte payload with a zero tail; any position 10-19 gives 143
bytes and needs `allowFull` (implied for slot 255). **Tail of a 143-byte
payload for slot 255:** every position that holds no band is all zero except
its type byte, which is `0xFF`, exactly what the device reports for payloads
the app wrote (`preset-map/teste2-slot255-*.json`), so the echo is comparable
byte for byte. Other targets keep the all-zero tail. Per band: `index` 0-19 and
unique, `freq` 20-20000 Hz, `gain` -12..+12 dB, `q` 1-65535 (raw), `type` one
of **0, 2, 3, 255** (the values seen on the device: 3 on factory presets 1, 10
and 14, 255 on the app's own bars in slot 255); 1, 7 and every other value are
refused. `masterGain` may be omitted only when the target
is the active slot (its own master gain is kept); for another slot it is
required, because the reported master gain belongs to the active slot. `bands`
and `gains` are mutually exclusive.

**Protected slots.** Slots 6 (factory "Classico") and 11 (hearing test) are in
`qcycore.ProtectedSlots` *for 0x22 writes*: the phone app never writes slot 11
through 0x22 either (the hearing-test result is stored and re-applied with
0x46/0x47 index 11, see "Hearing test frames"), so a 0x22 write there would be
something the app does not do. Enforced inside `ApplyEq`, so the CLI (direct and via
the daemon), the socket and the library apply all inherit it. Without
`--allow-protected` / `allowProtected: true` the write is refused, before the
backup and before anything is sent, when

- an explicit target slot is 6 or 11 (omit the slot to adjust the active one);
- the active slot is 6 or 11 and the target is another slot (a cross-slot write
  also switches the device away from it);
- `bands` would replace the whole curve of a protected active slot.

A master-gain or band-gain patch of the active protected slot (no slot given)
stays allowed when it is the 73-byte write (Classico), and otherwise needs
`--full-length` (custom slot 11 carries bands in positions 10-12, so its
master-gain slider now needs `allowFull`).

- **Slot** (byte 0): the active preset slot. The device only reports the
  *active* slot; an inactive slot cannot be read back. `qcycore.EffectName`
  names the slots measured on a MeloBuds N70 (reads of 0x22):

  | Slot | Effect | Status |
  |---|---|---|
  | 0 | (none: the old test curve of this tool) | n/a |
  | 1 | Default | measured |
  | 2 | Popular | measured |
  | 3 | Heavy bass | measured |
  | 4 | Rock | measured |
  | 5 | Soft | measured |
  | 6 | Classical | measured |
  | 7, 8, 9 | ? | not observed |
  | 10 | Spatial sound (payload identical to slot 1) | measured |
  | 11 | Hearing experience (the user's hearing-test profile) | measured |
  | 12, 13 | ? | not observed |
  | 14 | Soft treble | measured |
  | 255 | Custom | measured |

  Rule: any curve of its own made in the phone app is written to **slot 255**;
  the name given there stays on the phone only, so 255 is always reported as
  "Personalizado". Unknown slots have no effect name.
- **Master gain** (bytes 1-2): signed int16 in centi-dB, so `0x0096` is
  +1.50 dB. The client accepts -12..+12 dB (-1200..+1200) and rejects NaN, Inf
  and anything outside that range before converting to an integer. Band gains
  are limited to -12..+12 dB as well (the factory curves reach -12).
- **Bands** (bytes 3-142): `[freq:2][gain:2][q:2][type:1]` each. Gain is signed
  centi-dB. **Frequency, Q and type are not fixed**: the hearing-test slot (11)
  carries per-user values, so a writer must preserve them. An earlier
  implementation synthesised a fixed 31 Hz ... 16 kHz table with Q=1.00 and
  type 0; that destroyed the hearing-test curve and is gone.

### Units in `status --json`

`eq.preset` is the active slot (byte 0), `eq.effect` its effect name (string,
omitted when the slot is not in the table above; additive, same field in
`qcyctl eq get --json`, the daemon `eq` reply and write results), `eq.masterGain` is the master gain in
**dB** (float, int16 / 100). `eq.bands[]` lists every position with `freq` != 0
(up to 20), in order, each `{index, freq, gain, q, type}`: `index` is the
position (0-19), `freq` in Hz as stored on the device, `gain` in dB. `qcyctl eq
get --json` and the results of the write commands additionally carry the raw
`payload` (hex) and `slot`; write results add `fullLength: true` when the
unproven full-length format was used.

### Writing: read, patch, back up, write, confirm

Measured on a MeloBuds N70 (firmware 3.0.13) on 2026-10-15: a slot argument
after the 0x22 query does not select what is reported. `FF 04 FE 01 22 06`,
`FF 04 FE 01 22 07`, `FF 04 FE 01 22 0B` and `FF 04 FE 02 22 FF` all answer
with the active slot's 0x22 frame, byte for byte identical to the plain query.

There is no select-only command and no way to read an inactive slot, so a write
always starts from the payload the device reports **now** and changes only the
bytes that were asked for:

| Command | Bytes changed |
|---|---|
| `qcyctl master-gain <dB>` | 1-2 only |
| `qcyctl eq set <g1..g10> --yes` | the gains of positions 0-9 only; positions 10+ stay as read; master gain kept unless `--master-gain dB` |
| `qcyctl eq set ... --preset N` | byte 0 as well: the current curve is written into slot N, which also makes N active |

Order of operations (done under one transaction lock in the daemon):

1. read `0x22` (143 bytes expected; at least 73 accepted; refused if all 0xFF);
2. build the payload (`ApplyEq`): patch a copy of what was read, the first 73
   bytes when the tail holds no band, otherwise the full length read
   (`fullLength: true`, unproven, needs `--full-length`); or construct it from
   `bands`. Protected-slot and 143-byte refusals happen here, before step 3;
3. **save the full payload read in step 1** (all 143 bytes; see Backups); if this fails the write is
   **aborted** and the error is returned;
4. write the patched payload;
5. confirm by readback with zero tolerance: the echo (which may be longer than
   what was sent) must equal every byte sent: slot, master gain and every
   band written. The one allowance: a position that holds no band (freq, gain
   and q zero) matches an empty position whose type byte is 0 or `0xFF`, either
   way round (the device reports empty positions with the marker). When 73
   bytes were sent and the echo is longer, the echo's bytes 73.. must also hold
   no band (zero, or `0xFF` empty-type markers).

The earbuds first answer a query with the pre-change state, so confirmation
keeps re-querying until the echo matches or the deadline passes. At the
deadline the last echo decides the failure:

- it reports **another slot** than the one written (`echo[0]` != `sent[0]`):
  state **written, unconfirmed**. The write was sent but the device does not
  report that slot as active, so what it stored cannot be told from here. The
  daemon answers with `code: "unconfirmed"`; the message carries the payload
  that was sent, the one reported and the backup path. `qcyctl` prints that and
  exits non-zero;
- anything else (same slot, different bytes or a populated tail, or silence): a
  plain `readback mismatch`, no `code`; the message names the backup too.

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
- `qcyctl eq set <g1,...,g10> --yes [--preset N] [--master-gain dB] [--allow-protected] [--full-length] [--json] [seconds]`
  gains in dB. Out-of-range values are rejected, not clamped. Without
  `--preset` the target is the currently active slot; without `--master-gain`
  the device's current master gain is kept. `--preset 6|11`, or any `--preset`
  while slot 6 or 11 is active, needs `--allow-protected`; a payload that needs
  the 143-byte format needs `--full-length` (see Write format).
- `qcyctl master-gain <dB> [--full-length] [--json] [seconds]`: master gain in
  dB, -12..+12. On a slot whose positions 10-19 hold bands (the custom slot 11)
  it needs `--full-length`.
- `qcyctl eq preset <n>`: writes characteristic `000B`; fails on this model
  because the characteristic is absent.

All of these go through the daemon when it is running, and fall back to a
direct connection when it is not.

### Curve library CLI

`qcyctl library ...` manages named EQ curves stored one JSON file each under
`$XDG_DATA_HOME/qcy-omarchy/library` (format v1, see `internal/qcycore/presets.go`).
"Preset" is not used: it already means a hardware slot.

Offline (no Bluetooth, no daemon): `list [--all] [--json]`, `show <id>`,
`save <name> --gains g1,...,g10 [--master dB] [--note ..] [--replace]` (source
user, positions 0-9 at 31..16000 Hz, Q 100, type 0, the slot 0 layout; never
replaces unless `--replace`, and then only a source=user curve),
`delete <id> --yes` (moved to `.trash`), `export <id> <file>`, `import <file>`,
`seed [--json]`, and `compose --dry-run` (below). Unreadable files go to stderr.

**Display order of `list`:** the factory effects in the phone app's order
(Default, Popular, Heavy bass, Rock, Soft, Classical, Soft treble, Spatial
sound), then Hearing experience, then your own curves (source user, by
name), then everything else (device imports such as the old slot 0 curve and
"Custom (slot 255)"). Older seeds that a newer curve replaces
(`classico-slot-6`, `slot-11-primeira-leitura`, `experiencia-auditiva-slot-11`,
`<effect>-slot-N`, `slot-N-primeira-leitura`; see `qcycore.SupersededCurveIDs`)
are hidden by default and shown by `--all`; they are never deleted or renamed
and stay valid for `show`/`apply`/`delete`. `slot-0-primeira-leitura` has no
replacement and is always listed. A user's own curve is never hidden.

**`seed`** is idempotent and never renames, replaces or deletes a curve (an id
already in the library is skipped, whatever its source). It creates:

| Curve (id) | Source | Slot | From |
|---|---|---|---|
| Default, Popular, Heavy bass, Rock, Soft, Classical, Soft treble, Spatial sound | factory-snapshot | 1, 2, 3, 4, 5, 6, 14, 10 | payloads **embedded in the binary** (`internal/qcycore/factory.go`; public factory data, measured on the device) |
| Hearing experience (`experiencia-auditiva`) | device-import | 11 | `<backups>/eq-slot11-first.hex`, else `<backups>/preset-map/experiencia-auditiva-slot11-*.json` (`payload`); skipped with a reason when neither exists |
| Slot 0 (first read), Custom (slot 255), ... | device-import | N | every other `<backups>/eq-slotN-first.hex`, named as older versions named them; factory slots and 11 are no longer seeded this way, the curves above replace them |

Every seeded curve is `applyable` (see below).

With the earbuds: `import-device <name> [seconds]` saves the active EQ
(lossless payload, source device-import) and `apply <id> --yes [--full-length]
[--slot 0|255] [--json] [seconds]` writes a curve to **slot 255** (default), the
phone app's "Personalizado" buffer (byte 0 of the payload is `0xFF`), or, with
`--slot 0`, to the old test slot (any other slot is refused), in build mode
(`bands`, master gain from the curve) through the same path as `eq set` (daemon
or direct, backup, readback). It never passes `allowProtected`. Slot 255 is the
only target that stays allowed while slot 6 or 11 is active (measured: the
phone app does exactly this, and slots 6 and 11 were re-read identical
afterwards; `ApplyEq` lifts the protected-slot refusal for an explicit slot
255, the active slot is still backed up first); `--slot 0` with a protected
active slot is refused.

A curve is `applyable` when every band has a type in 0, 2, 3, 255 (anything
else is refused before connecting). Bands at positions 10-19 do **not** make a
curve unapplyable: slot 255 takes the 143-byte payload with no flag (see Write
format); `--slot 0` with such a curve is refused offline unless `--full-length`
is given. `--json` carries `wrote`, `backup` and `confirmed` (false plus `code`
on an unconfirmed write, which also exits non-zero).

**`compose`** builds the layout the phone app writes into slot 255 for "your
curve on top of a factory effect", and writes it through the same path as
`apply` (daemon or direct, backup of the active payload, readback, `unconfirmed`
handling; `allowProtected` is never passed and there is no `--slot`):

```
qcyctl library compose --base <id|none> --gains g1,...,g10 [--master dB] --yes [--json] [seconds]
qcyctl library compose --base <id|none> --gains g1,...,g10 [--master dB] --dry-run [--json]
```

- **Layout:** first one band per bar of `--gains` whose gain is not 0 (dB,
  -12..+12), at the bar's ISO frequency (31, 62, 125, 250, 500, 1000, 2000,
  4000, 8000, 16000 Hz), Q 100, type 255, in bar order; then the bands of the
  base curve in their original order, with their own freq/gain/Q/type. Positions
  are consecutive from 0. Up to 20 bands in all; more is an error that names
  both counts. `--base none` writes only the bars (at least one bar must be
  non-zero).
- **Payload:** at most 10 bands give the 73-byte payload with a zero tail; 11-20
  give 143 bytes with the app's tail (`0xFF` in the type of every empty
  position), allowed without a flag because the target is slot 255.
- **Master gain:** `--master dB` if given, else the base curve's master gain,
  else -12 (what every factory effect reports).
- **`--dry-run`** (no `--yes` needed) loads the base from the library, builds
  and validates the payload and prints it without connecting to anything. With
  `--json`: `{base, slot, masterGain, nBands, fullLength, payload (hex), bands:[{index, freq, gain, q, type}]}`.
  A real write prints the `apply` result plus `base`, with `id: "compose"`.

## Restarts caused by writing

Two writes restart the earbuds:

- **Multipoint (`0x24`) — expected.** The product documentation states the
  earphones restart each time dual-device connection is toggled.
- **LDAC (`0x23`) — expected, per the official panel.** The vendor server
  declares the action with the text "Turning on LDAC will restart the device"
  (see WEB_API.md), so the restart is documented behaviour, like multipoint.
  The setter is therefore enabled on models that declare it
  (`qcyctl ldac <on|off> --yes`; the `ldac` capability comes from the `FE 01 23`
  probe). The write is sent once and never repeated: if the link drops right
  after the write was accepted, the daemon reports "applied, the earbuds
  restarted" instead of an error. **Verified on hardware: NO** (not tested
  since the write was enabled); treat the restart/readback behaviour as
  unverified until it is.
  When the earbuds still echo the old value at 4 s and the link is up, `set`
  replies `{"indeterminate": "indeterminado: aplicado possivelmente, reinício
  pendente — releia o estado"}` (the string is Portuguese verbatim; it means
  "indeterminate: possibly applied, restart pending, read the state again")
  (not an error, never rewritten); the plugin then
  reads the state again instead of assuming success.

## Find my earbuds

**Vendor app: find is not a BLE command.** The phone
plays a sound in a loop through the media (A2DP) stream, raises the system
volume (it starts at about half of the maximum and rises one step every 2 s up
to the maximum), and on stop restores the saved volume. It stops on the stop
button, on audio-focus loss and on disconnection. The app has no ceiling, no
warning and no timeout.

`0x05` (LED flash) and `0x3D` produced no change
while the earbuds were connected, so `qcyctl find` no longer sends them.

`qcyctl find [--sink NAME] [--start-volume 100%] [--tone-level -3] [--max-volume 100%] [--timeout 60] [--yes] [--events]`
does what the app does, locally, plus the limits the app lacks:

| Rule | App | `qcyctl find` |
|---|---|---|
| command to the earbuds | none | none (no BLE, no daemon) |
| tone | looped sound | generated "di-di" beeps, 2 kHz, 150 ms each, 100 ms apart, 10 ms ramps, peak -3 dBFS by default (`--tone-level`, -30..-3) |
| start volume | about half | `--start-volume`, default 100% of the sink (the earbuds are lost: the tone must carry) |
| step | one system step per 2 s | +5 points per 2 s |
| ceiling | maximum | `--max-volume`, default 100% |
| timeout | none | `--timeout`, default 60 s |
| warning | none | "Remove the earbuds from your ears before starting"; needs `--yes` or a typed `y` |
| stop | button, focus loss, disconnect | q / Enter / space, signal, timeout, sink gone, player died |
| on stop | restores volume | restores volume and mute, resumes the players it paused |

The volume is never raised before the sink was measured silent (the music is
paused over MPRIS and each stream is measured with `parec --monitor-stream`,
as in `hearing run`), the sink must be a Bluetooth sink identified by
`api.bluez5.address`, and the tone is played with `pw-cat ... -P
"node.dont-fallback=true node.dont-reconnect=true"`.

Known limit: while the tone plays, a stream that was already on the sink when
the tone settled (about 0.5 s after it started) is not re-measured. A stream
that is NEW afterwards (a resumed player, another app) is caught by a cheap
check every 2 s, before each volume rise (`pactl list sink-inputs`): the run
stops with reason `audio-started` and restores the volume. A resumed player
whose stream was already uncorked at the baseline is the one case this misses.

`--events` is for the plugin: no terminal, JSON lines on stdout
(`started`, `volume`, `error`, `stopped` with a `reason`), the line `stop` on
stdin ends it (EOF too), progress text goes to stderr. `stopped` is written
after the volume and the music were restored. A `stop` (or EOF) that arrives before the tone starts, even during the music pause or the silence wait, aborts the run before any volume change. Not verified on hardware.

## Prompt volume

**Update (read in the vendor app; verified on hardware 2026-10-13).** The
rest of this section records the earlier one-byte measurement. The app
(`ToneVolumeManagerActivity`) writes `FF 04 1D 02 <v> 00` (two parameter bytes,
the second 0) for every model but vendorId 16737, with v from the server list
4, 6, 8, 10; the query `FF 03 FE 01 1D` answers `<value> <max>`. The
measurement below used a one-byte frame and values outside that list. The
daemon and `qcyctl prompt-vol <4|6|8|10|low|medium|high|max>` now write the
app's frame and confirm by reading back (after ~200 ms) that byte 0 equals v;
a mismatch is reported as "escrito mas o aparelho reportou X". The constant
`promptVolWritable` (daemon and CLI, currently `true`) turns this back into
read-only. The write is offered to every model whose panel declares a beep list on command 29 (the daemon takes the values from the panel; the frame is verified on the N70); a model that does not declare it refuses it.

The two other controls, offered to every model whose panel declares them (frames verified on the N70), read from the same app:

| Control | Write | Query | Values |
|---|---|---|---|
| Prompt volume `0x1D` | `FF 04 1D 02 v 00` | `FF 03 FE 01 1D` -> `<value> <max>` | v from the panel's list (`model.promptVolumeSteps`; 4 / 6 / 8 / 10 on the N70) |
| Wear-sensor sensitivity `0x48` | `FF 03 48 01 p` | `FF 03 FE 01 48` -> `<p>` | p from the panel (`model.wearSensitivityItems`; 1 medium, 2 normal, 3 high) |
| Fit (seal) test `0x11` | start `FF 05 11 03 01 01 01`, stop `FF 05 11 03 00 00 00` | none | notification `[status, left, right]`; the daemon waits for status 2; per ear 3 sealed, 2 good, 1 fair, 0 poor |

All three were verified on the N70 on 2026-10-13. The daemon and `qcyctl`
wait ~200 ms and read back before reporting a prompt-volume or sensitivity
write as confirmed. CLI: `qcyctl prompt-vol`, `qcyctl wear-sensitivity`,
`qcyctl fit start|stop|begin`. Sensitivity only matters with wear detection
`0x2C` on.

`0x1D` reads and writes cleanly but the device keeps reporting 6 regardless of
what is written. The CLI's readback confirmation therefore fails loudly rather
than reporting a success it cannot back up.

Measured on this unit: writing 0, 3, 9, 12 and 15 all read back `06 0f` —
value 6, max 15. The write is accepted and discarded; the reply is not an echo
of the last value sent.

Historical, from the one-byte measurement: while `promptVolWritable` is false, prompt volume is listed in `readOnlyCmds` in the daemon rather than
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

### One client at a time, idle window

The earbuds accept a single BLE central. `qcyctld` therefore connects lazily
(nothing at startup, no background reconnect or probing) and releases the
adapter again after an idle window:

- the first request connects; further requests inside the window reuse the link;
- every request restarts the idle timer, and the link is never dropped while a
  transaction or a request is in flight;
- when the window expires the daemon disables notifications and disconnects;
- window: `QCYCTL_IDLE_SECONDS` (default 6) or `qcyctld -idle N`; `0` keeps the
  link open for good (the previous behaviour). Positional `[scanSeconds]` is
  unchanged.

If the connect fails in a way that means another app holds the earbuds
(`In Progress`, `Already Connected`, `Connection Refused`, `Resource Busy`), the
daemon retries 3 times with a 1.5 s backoff, then answers
`{"ok": false, "error": "earbuds are in use by another app: close it on this or any other device (phone, tablet, computer) and retry", "code": "busy"}`.
Further requests fail fast with the same answer for 3 s. `status` and `battery`
instead return the last valid reading when one exists, with two extra fields,
`"stale": true` and `"ageSeconds": N`; fresh readings carry neither. `code` is
absent on every other error except `not_found`. `capabilities` is answered from the known model
without connecting.

If the scan finds nothing and no cached address answers, the daemon replies `{"ok": false, "error": "earbuds not found: powered off, out of range, or held by an app open on this or another device (they accept one client at a time)", "code": "not_found"}`: the earbuds are off or out of range, or an app on this or another device holds them (a connected client stops advertising, so the scan cannot see them). Only `In Progress`, `Already Connected`, `Connection Refused`, `Resource Busy` and `busy` count as `busy`; timeouts, aborts and `Not Connected` go on to the scan. A `status` whose battery read and ANC query both get no answer (a session left dead when another client took the earbuds) also replies `not_found` (or `busy` when the transport error says so) and drops the session, so the next request reconnects; a status with at least one of the two answered stays a success. A write that reaches the device drops the cached readings, so `status` and `battery` never answer `stale` after it.

| `cmd` | `args` | `data` |
|---|---|---|
| `status` | none | raw per-opcode dump (`eq` is the raw 0x22 byte array) |
| `battery` | none | battery object |
| `capabilities` | none | model capabilities |
| `set` | `action`, `params`, optional `expect` | confirmed payload bytes or null; `action: "eq"` is refused |
| `eq` | none | `{slot, effect (when known), masterGain (dB), bands[{freq,gain,q,type}], payload (hex)}` |
| `eqApply` | optional `gains` (10 ints, centi-dB), `masterGain` (int, centi-dB), `slot` (int), `bands` (array of `{index, freq, gain (dB, number), q, type}`, exclusive with `gains`), `allowProtected` (bool), `allowFull` (bool). Nothing else: unknown keys and raw payloads (`payload`, `raw`, `hex`, ...) are rejected | the `eq` object plus `previous`, `fullLength`, `backupFirst`, `backupHistory` |
| `eqMasterGain` | `masterGain` (int, centi-dB), required; optional `allowProtected`, `allowFull` | same as `eqApply` |
| `wear` | `state`: 1 on / 2 off (written as 0x00 where the model's panel declares 0 as off, as the N70 does, 0x02 otherwise); `literal: true` with `state` 0 or 2 writes exactly that byte (0 refused where the panel does not declare it) | confirmed `0x2C` payload (byte array) |
| `fitTest` | optional `seconds` (5..60, default 30) | `{status, left, right}` once the notification with status 2 arrives; the stop frame is always sent at the end; models whose panel declares the fit test (cmd 17) |
| `fitStop` | none | cancels a running `fitTest` (which then fails with `fit: cancelled`), otherwise sends the stop frame |
| `noise` | none | raw `0x17` payload `[mode, scene, level]` |
| `find`, `keys`, `keysSet`, `disconnect` | as before | |

`set` also accepts `promptVol` (`params` `[v]`, v one of `model.promptVolumeSteps`; sent as `[v, 0]`) and `wearSensitivity` (`params` `[p]`, p one of `model.wearSensitivityItems`), both only when the model declares them, and read back after ~200 ms; `status` carries `wearSensitivity` only when the model declares it and the unit answers `0x48`.

Nothing is gated on whether a model was tested on hardware. A command runs when
the connected model declares the feature it drives: `eqApply` and
`eqMasterGain` need `eq` in `model.features`, `keysSet` needs `keysRemap`,
`fitTest` needs `fit`, `wear` needs the `wearDetect` action, `windOffByte` the
`windNoise` action, and the `promptVol`, `wearSensitivity` and `ldac` setters
their own action (`<cmd> is disabled: <model> does not declare it`, or `<action>
is not declared by this model`). The generic fallback (unknown model) declares
no panel, so those three setters are refused there. `hearingMode` and
`hearingWrite` are driven for the N70 only (the baseline levels and the
`0x45`-`0x47` frames are not known for other models). Only firmware/OTA
writing is forbidden for every model.

`status` carries a `model` object:
`{modelId, displayName, verified, tested, actions, features, vendorId?, productTitle?, keyActions?, promptVolumeSteps?, wearSensitivityItems?, ancModes?, eq?, notes?}`.

- `verified` / `tested` (same value): informational, "this tool was tested on
  hardware with this model" (only the N70). It gates nothing; a client must not
  hide a declared feature because it is false.
- `actions` / `features`: what the model declares (see `capabilities`). `features` also carries `spatial` (the N70 handler) and `hearingTest`
  (the hearing-test frames are driven; the N70 only). `anc --raw` writes the three
  ANC bytes a model's `ancModes` lists.
- `keyActions` maps each music-mode key id (two hex digits, as in the `keys`
  pairs) to the actions the model's panel offers for it, `[{id, name}]`; it is
  absent when the panel declares none. Call-mode keys are not listed. Present
  together with the `keysRemap` feature, which is declared only for panels with
  the type-10 key layout; the daemon checks every `keysSet` pair against it for
  models other than the N70.
- `promptVolumeSteps`: `[int]`, the values `promptVol` accepts (the panel's
  beep list). Present with the `promptVolume` action.
- `wearSensitivityItems`: `[{name, value}]`, the choices of `wearSensitivity`,
  ascending. Present with that action.
- `ancModes`: `[{name, start, end, default, params, items?[{name, start, end,
  params}]}]`, the ANC groups of the panel. `start`, `end` and `default` are the
  packed values `(mode<<16)|(scene<<8)|level`; `params` is `default` (for an
  item `start`) split into the three bytes `[mode, scene, level]` that `set anc`
  carries. Present only with the packed encoding (all values >= 65536); a panel
  with single-byte ANC values gets no `anc` action and a note.
- `eq`: `{bands, freqs, minDb, maxDb, slots, protectedSlots}`. `bands` is 10
  (the only layout `eqApply` writes), `freqs` the panel's band centres, `minDb`
  and `maxDb` the panel's own range as signed values (the daemon itself accepts
  +-12 dB; the N70 reports its tested +-12), `slots` the effect ids the panel lists (an effect without an id takes
  its list position + 1) plus the personalised-listening slot, `protectedSlots`
  the personalised-listening slot and the classic curve (id 6) when listed.
  For a model other than the N70, `eqApply` accepts an explicit `slot` only
  among `slots` and 255, and refuses `protectedSlots` without `allowProtected`.
  A 5-band panel gets no `eq` and a note.
- `notes`: `[string]`, why a feature the panel lists is not offered: its
  encoding is not understood by this tool (for example `anc: encoding not
  supported ...`, `eq: 5-band layout not supported ...`, `keys: layout not
  supported ...`). Absent when there is nothing to say.

`eqApply`, `eqMasterGain` and `wear` read, patch and write under a single
transaction lock, so a concurrent status sweep cannot interleave. `eqApply` is
the only way to write `0x22`, and it never takes raw payload bytes: the
payload is always built from structured values validated in `qcycore`.

`eqApply` / `eqMasterGain` failure codes (`code` in the response, in addition
to `busy` and `not_found`):

| `code` | Meaning | Written? |
|---|---|---|
| `protected_slot` | the target or the active slot is 6 or 11; pass `allowProtected` | no |
| `full_length_required` | the payload needs 143 bytes (unproven); pass `allowFull` (not needed when the explicit `slot` is 255) | no |
| `unconfirmed` | the write was sent, the device keeps reporting another slot | sent, unknown |
| none | validation error, backup failure or readback mismatch | see message |

An `unconfirmed` response also carries three string fields next to `error` and
`code`: `sent` (hex of the payload written), `deviceReported` (hex of the last
payload the device reported) and `backup` (path of the pre-write payload).
`qcyctl` rebuilds a typed `*qcycore.EqError` from `code` and these fields, so
`errors.Is(err, qcycore.ErrUnconfirmed / ErrProtectedSlot /
ErrFullLengthRequired)` and `library apply --json` (`code`, `sent`,
`deviceReported`, `backup`) behave the same through the daemon as in direct
mode.

## Characteristic layout (service `a001`)

Enumerated with `QCYCTL_PROBE=1`:

| UUID | Purpose | Readable |
|---|---|---|
| `00001001` | command write | no |
| `00001002` | command notify | yes |
| `00000008` | battery, L/R/case; bit 7 = charging, level = byte & 0x7F (the daemon decodes it) | yes |
| `00000007` | firmware version, 3 or 6 bytes | yes |
| `0000000d` | touch remap, 10 key:fun pairs | yes |
| `0000000b` | absent on this model | — |

Touch ids: the low byte is a key index (`0x01`–`0x0a` for left/right
single, double, triple, quad and hold; `0x15`–`0x1e` repeat the scheme for
in-call gestures) and the high byte a function (`0x00` none, `0x01` play/pause,
`0x02` previous, `0x03` next, `0x04` voice assistant, `0x05`/`0x06` volume,
`0x07` game mode, `0x08`–`0x0a` call handling, `0x0b` ANC mode, as the N70 panel declares it). Ids outside
those ranges are shown as raw hex rather than guessed at.

The N70 panel does not list the quadruple keys (`0x07`, `0x08`), but the
firmware accepts them: writing `07:01` applied and `07:00` restored it. The
daemon therefore reports, in `model.keyActions`, the same action list for
`07` and `08` as for the other music keys. The in-call keys stay read-only.

## Model detection

The protocol carries no model name, so the daemon picks the model in this order
(`detectModel` in `cmd/qcyctld/main.go`):

1. `vendorId` from the advertisement (manufacturer data, bytes 0-1,
   big-endian): **23872 or 23877 -> `melobuds-n70`**, the only verified model;
2. any other `vendorId` in the product database (Quicky list, then the embedded
   snapshot) -> `qcy-<vendorId>`, a conservative profile (`cmd/qcyctld/profile.go`)
   that offers only the shared commands the product declares and never EQ;
3. otherwise the advertised name (`n70`), then the firmware string, then
   `generic`.

The vendorId is stored as field 4 of `~/.cache/qcyctl/le-address` so
the fast path (no scan) keeps it.

### Advertisement: colour

The advertisement carries manufacturer data under company ID `0x521C` (an
alternative, `0x522C`, is accepted too). For a payload of at least 20 bytes
(company ID not included), bytes 0-1 are the vendorId (big-endian) and bits 1-3
of byte 3, `(b[3] & 0x0E) >> 1`, are the colour index (0..7). The index matches
the `colorIndex` of a `colors[]` entry of the product list; an entry without
one is matched by its position in the list. Measured: the manufacturer data is
present while scanning; **inferred**: the bit layout of the colour field (the
colour-to-image mapping has not been checked against every unit). Once
connected, BlueZ no longer exposes the manufacturer data, so the daemon records
the colour during the scan and stores it as field 5 of the `le-address` cache
(only alongside the vendorId it was seen with). The status `model` object then
carries `colorIndex` and `colorSource: "device"`. Source of the product data, the embedded
snapshot and its limits: `WEB_API.md`.

## Supporting more than one model

The cmdId space above is the same across this product family — it is the
device's protocol, not one model's. What differs between models is **which
subset a given unit answers**. So a capability-driven plugin does not need a
hard-coded list per model: probe the candidate cmdIds once, keep the ones that
reply, and drive the UI from that.

Two probes already exist for exactly this: opcode `0x44` (max EQ band count) asks the device
how many EQ slots it has, and the ANC level range differs by model.

## Debug switches

| Variable | Effect |
|---|---|
| `QCYCTL_DEBUG=1` | per-frame trace on stderr (`qcyctld`) |
| `QCY_DEBUG=1` | per-frame trace on stderr (`qcyctl`, direct mode) |
| `QCYCTL_PROBE=1` | enumerate service `a001` with read results |
## Hearing test frames (0x45, 0x46, 0x47) — NOT verified on hardware

Read from the phone app, never captured on a device. `0x45` (`FF 03 45 01 0x`)
switches the personalised-effect flag (the test mode); `0x46` / `0x47` carry the
left / right curve of effect 11 (`FF <n+2> 46|47 <n> 0B <master int16 LE>
{freq, gain, q, type}*8`). What the earbuds answer is unknown: the daemon waits
briefly for an echo of the opcode, never requires one, and reads 0x22 back. The
master gain is limited to -12..+12 dB (the range recorded for 0x22; the range of
0x46/0x47 is an assumption). `qcyctl hearing run` sends 0x45 only with
`--device-mode` or `--apply --yes`; `qcyctl hearing mode on|off --yes` sends it
by hand. Both daemon commands require `confirm:true`.

Commands: `qcyctl hearing run [--device-mode] [--apply --yes [--accept-floor]]
[--out result.json]` runs the test (and writes only with `--apply --yes`);
`qcyctl hearing apply <result.json> --yes [--accept-floor] [--device-mode]`
writes the stored result of an earlier run without redoing the test. Both use
the same path: the full status and the raw active 0x22 payload are saved first
(`~/.config/qcy-omarchy/backups/hearing-before-<timestamp>.json` plus a curve
file `curve-eq-antes-do-teste-<UTC>.json` in `~/.local/share/qcy-omarchy/backups/`,
never a library entry; restored with `qcyctl library apply --file <that file> --yes`); then the left and right
frames are sent with `confirm:true`, each answer and the 0x22 readback are
printed, the test mode (if it was switched on) is switched off, and the result
is declared "applied" only when the device echoed the opcode or its readback
shows the written curve ("NOT CONFIRMED" otherwise). If the right ear fails
after the left was written the output says "MIXED STATE" with the restore
command. Bands still heard at the lowest level of the staircase ("floor") are
refused unless `--accept-floor` is given.

`qcyctl hearing run --events [--sink NAME] [--sink-volume 90%] [--out file.json]
[--baseline factory|zero] [--device-mode]` is the same test, with the same
guards, for the plugin: no terminal, JSON lines on stdout, commands on stdin,
the human progress text on stderr. It never writes to the earbuds (`--apply`,
`--yes`, `--accept-floor`, `--dry-run` are refused): the result is saved to
`--out` (default `~/.cache/qcy-omarchy/hearing-<timestamp>.json`, mode 0600)
and written later with `qcyctl hearing apply <out.json> --yes [--accept-floor]
[--baseline factory|zero]`. `--baseline zero` is recorded in the file only as
information: `hearing apply` refuses such a file ("the result file asks for
baseline zero (experiment); pass --baseline zero to confirm") unless
`--baseline zero` is given explicitly, so the plugin must pass the flag itself
when it applies a result of a run made with `--baseline zero`. Without `--out`
the file is created (exclusively, mode 0600, in a 0700 directory) as
`~/.cache/qcy-omarchy/hearing-<date>-<time.ms>.json`. `--device-mode` only
allows the test-mode frame (0x45) to be sent; curves are never sent in this
mode.

Commands, one per line on stdin: `heard`, `not-heard`, `repeat`, `abort`
(case-insensitive; anything else, and any line over 1 MiB, is ignored and does
not abort). `heard <index>` / `not-heard <index>` answer only the tone with that
`index`: any other index is discarded. EOF is an abort. A command that
arrives while no tone is playing is discarded, as a key is in the terminal run;
a tone that died is replayed (at most twice per band, then the run fails).

Events, one JSON object per line (`event` names the kind):

```
{"event":"phase","name":"pausing-music","message":"pausing the music"}
{"event":"tone","index":1,"ear":"left","band":0,"bands":8,"freq":63,"step":4,"levelDb":-58.4,"progress":0}
{"event":"answer","ear":"left","band":0,"heard":true}
{"event":"result","left":[..8 ints..],"right":[..8 ints..],"floor":{"left":[..8 bools..],"right":[..8 bools..]},"masterGain":-3.5,"outFile":"/home/u/.cache/qcy-omarchy/hearing-20261013-120000.json"}
{"event":"warning","message":"tone interrupted, answer discarded, repeating (1/2)"}
{"event":"error","message":"no Bluetooth sink found ..."}
{"event":"stopped","reason":"done"}
```

| Event | Fields |
|---|---|
| `phase` | `name`: `pausing-music`, `checking-earbuds`, `waiting-silence`, `starting`, `testing`, `finishing`, `done`, `aborted`, `error`; `message` |
| `tone` | `index` (1-based, counts answers; a repeat of the same tone has the same `index`), `ear` (`left`/`right`), `band` (0-based), `bands` (8), `freq` (Hz), `step`, `levelDb` (dBFS), `progress` (0..1: finished bands of 16; the staircase length is not fixed) |
| `answer` | `ear`, `band`, `heard` |
| `result` | `left`, `right` (gain per band), `floor.left`/`floor.right` (bands still heard at the lowest level), `masterGain`, `outFile` |
| `warning` | `message` (also for every cleanup problem) |
| `error` | `message`; followed by `phase` `error` and `stopped` |
| `stopped` | `reason`: `done`, `abort` (command or EOF), `signal`, `error` |

Order of a complete run: `phase` pausing-music, checking-earbuds,
waiting-silence, starting, testing; `tone`/`answer` pairs; `phase` finishing;
`result` (the file is already written); `phase` done; `stopped`. An abort ends
with `phase` aborted and `stopped`, with no `result`. `stopped` is always the
last line and is written only after the cleanup: the tone is stopped, the test
mode (when it was switched on) is off, the sink volume and mute are restored
and the players the run paused are resumed. An abort is a normal end (exit 0);
a signal or an error is a non-zero exit. A `heard`/`abort` that arrives during
the music pause or the silence wait aborts the run before any volume change.
Not verified on hardware.

How the phone app handles the result (as observed): "use this
result" writes `0x46` (left) and `0x47` (right) with effect index 11, nothing
else (no 0x22, no slot select). Tapping the "Hearing experience" effect again
with curves already saved does NOT reopen the test: the app re-sends 0x46/0x47
index 11 from the left/right curves it keeps on the phone. The vendor app never
asks the earbuds for the ears: it keeps both curves itself. The 0x22 report holds
the left ear only; the right ear is readable with the query below.

We mirror that. A library entry may carry `ears` (`left` and `right`, 8 bands
each at 63..8000 Hz, Q 70, type 2, plus `masterGain`); its `bands`/payload hold
the per-band mean for the chart. After a hearing result is written and
confirmed (`hearing apply`, `hearing run --apply --yes`) the entry
`experiencia-auditiva` (source device-import, slot 11) is replaced atomically
with the new `ears` and the previous version goes to the library `.trash`.
`library apply <id> --yes` of an entry with `ears` sends 0x46/0x47 index 11
(backups first, no 0x22, no slot 255); an entry without `ears` (a 13-band copy
read from the device) keeps the slot 255 path. If the library cannot be updated
after the earbuds were written, a warning says so and nothing is undone.

Slot 11 is what the hearing-test frames overwrite, and the earbuds report only
the ACTIVE slot, so before writing the tool preserves it as a curve file in the
backup directory (`~/.local/share/qcy-omarchy/backups/curve-slot11-backup-<UTC>.json`,
full curve including `ears`, named "Hearing experience (backup YYYY-MM-DD HH:MM)";
the library gets no card): read directly when slot 11 is the active slot,
otherwise a clone of the last known copy (`experiencia-auditiva`, else the most
recent entry with slot 11) with a note stating when that copy was read. With no
copy and no active slot 11 the write is refused unless `--no-slot11-backup` is
given. Restore with `qcyctl library apply --file <backup file> --yes`.

**Measured on a MeloBuds N70 (first real write of 0x46/0x47 index 11).** The
earbuds sent NO echo for 0x46 or 0x47 (`ack=false` for both). Afterwards the
0x22 reports slot 11 active ("Hearing experience"), the written master gain
(-10 dB) and 13 bands: positions 0-4 are the 5 bands of the default effect
(50/200/900/2800/8000 Hz, Q 60/60/70/120/350, which the firmware prefixes) and
positions 5-12 are the 8 test bands (63..8000 Hz, Q 70, type 2) with the gains
of the LEFT ear. The right ear, written last, does not appear: the 0x22 reports
one ear only. The prefix is not always those 5 default bands: it is whatever the
previous effect left behind (measured: 6 bands, 200/900/2800/8500/7000/15000 Hz,
after `library apply experiencia-auditiva` with Popular active in slot 255, 14
bands in all). Anything that recognises a hearing card in a readback (the
panel's `library matches`) therefore compares only the LAST 8 bands with
`ears.left` and ignores the prefix. Consequences for the tool:

- the readback is compared **by position** (the last 8 bands; any prefix length is
  accepted), never by frequency: the default effect's 8000 Hz band collides
  with the test's 8000 Hz band;
- the left ear is confirmed by that readback (master within 0.05 dB, bands
  within 0.1 dB); when the daemon cannot read the right ear (see "Reading both
  ears" below) it has no independent reading and is reported as
  "left confirmed by readback; right sent, not independently verifiable (0x22
  reports the left ear)". That is a PARTIAL confirmation and counts as success
  (exit 0, the `experiencia-auditiva` card is updated with a note saying so),
  provided the right-write readback shows the left curve just confirmed and the
  written master. If the left does not match, or the readback shows something
  else, the result is "NOT CONFIRMED" as before.

**Experiment, opt-in and NOT verified: `--baseline zero`.** After 0x46/0x47
index 11 with 8 bands the firmware reports 13 (the 5 factory bands first). To
see whether the earbuds can be left without that baseline,
`qcyctl hearing apply <file> --yes --baseline zero` (default `--baseline
factory`, the behaviour above) sends N = 13 bands per ear: the 5 factory bands
(50/200/900/2800/8000 Hz, Q 60/60/70/120/350, type assumed 2) at gain 0, then
the 8 test bands, so `paramLen = 7*13+3 = 0x5E` and `bodyLen = paramLen+2`
(the vendor app takes n = the band count of the item).
The master gain comes from the 8 test gains alone. The positional readback
check then accepts (as for the factory baseline: the last 8 bands are the ear's and the master matches, whatever the prefix length) and only tells apart: (a) 13 bands, first 5 at gain 0, last 8 the test bands;
(b) 18 bands (the firmware prefixed the factory baseline again), reported as
"firmware still prefixes the factory baseline (18 bands)" and counted as a
RESULT OF THE EXPERIMENT (exit 0, with a warning); (c) 13 bands with a non-zero
prefix, reported as "firmware replaced the zero baseline with the factory
baseline (13 bands)", also a result; any other prefix length is confirmed without a note, and a readback whose last 8 bands are not the ear's is NOT CONFIRMED.
The `experiencia-auditiva` card stores only the 8 test bands in `ears` plus
`ears.baseline: "zero"`; re-applying the card (`library apply`) honours it
(zero: 13 bands per ear; absent or factory: 8). The daemon's `hearingWrite`
takes an optional `baseline` (`"factory"` | `"zero"`).


## Model snapshot v2

`internal/qcycore/models/models.json` is `{"schemaVersion":2,"models":{"<vendorId>":Product}}`
(`models.SchemaVersion()`; the loader still reads the bare v1 map). Every v1 field
of `Product` and `Panel` is unchanged; v2 adds `panel.functions`: one entry per
function the server panel declares, in declaration order (layouts, with the
items of the type-4 container expanded in place). Written by
`scripts/gen_models.py` (input directory in `$QCY_PANELS_DIR`).

A `Function` has `type` (the server's numeric type), `cmdId`, `name`,
`character` (32-bit characteristic id, hex, when the item names one) and at most
one typed block:

| block | types | content |
|---|---|---|
| `toggle` | 200-206 | `on[]`, `off[]`: full frames in hex (`FF03230101`); wind noise off is `FF032A0100` |
| `command` | item 3, 4, 100 | `bytes`: raw write in hex (`FF020100` reset, `FF020200` clear pairing, `FF020300` factory) |
| `choices[]` | 2 (item), 103, 104, 111, 116, 1000 | `label` plus `value` (level, cid, index) or `bytes` (hex); `secondary` is the wear sensitivity selector (cmd 72) |
| `standby` | 102/1000 with cmd 20/31 | `options[]`: `label`, `bytes` (2 bytes little-endian minutes), `minutes`, `never` (FFFF) |
| `language` | 1001 | `options[]` (`label`, `bytes` = 4 ASCII bytes) and `languages[]` (locales the row is shown in) |
| `anc` | 6, 9, 11 | `modes[]` with `start/end/default`, `viewType`, `voiceCmdId`, `envAdaptation`, `envName`, `ancInvert`, `hidePop`, `noSpaceTip`, `items[]` |
| `eq` | 2, 23 | `count`, `freq`, `minDb/maxDb/baseDb`, `q`, `iitType`, `volumeLevel`, `supportMaxCount`, `rebootTip`, `effects[]` (+`vals`, `onlyTws`, `alarmMsg`), `individuation` (+`soundDB`), `auxEq` |
| `keys` | 1, 10, 14, 114 | `events[]` with `eventID`, `group`, `side`, `keyName`, `character`, `defaultFun`, `actions[]`; `voice[]` call-mode labels; `groups` |

`messages` carries the server texts (`msg`, `msgIos`, `msgAndroid`, `alert`,
`rebootTip`, `alarmMsg`, `confTip`, `toastMsg`, `toastTip`, `desc` ...), the
only place where the server says an action restarts the earbuds. `props` holds
leftover scalars (row `id`, `jump`, `repeat`, `maxLength`, `latitude`,
`manufacturers` ...). `raw` holds the sanitised leftover structures of types not
interpreted (105, 115, 13, 24, and layouts without a type), so no data is lost.

Not carried: artwork and sound URLs, HTML help texts, the "update the app"
placeholder `msg`. Texts are the single language the panel was fetched in.
Frequencies written as `1k`/`2k` are numbers (v1 stored 0 for them).
`models.FunctionsFor(vendorId)` returns the list; `Function` has typed accessors
`Toggle()`, `Command()`, `Choices()`, `Standby()`, `Language()` and `Messages()`
(the struct fields carry a `Block`/`List` suffix). See "Generic functions".

## Generic functions

The daemon drives the panel functions that no specialised handler covers
(`cmd/qcyctld/fn.go`). Only functions declared by the panel of the connected
unit's vendorId are offered, and the frame written is the one the panel
declares.

A function is offered when all of these hold:

- it has a drivable block: `toggle` with one `on` and one `off` frame, `command`,
  `standby` or `language` with a cmdId, or `choices` whose options carry `bytes`
  (a bare level number declares no frame, so none is invented);
- it names no characteristic, or the command characteristic (`00001001`);
- every declared frame validates: `FF <bodyLen> <op> <paramLen> <params>` with
  consistent lengths and no extra bytes (the same block without the envelope, and
  a bare opcode for a parameterless command, are also accepted), and `op` equals
  the declared cmdId (a command without a cmdId takes the frame's opcode);
- its opcode is not one a specialised path already drives (game, sleep, LDAC,
  multipoint, wind, wear, auto-off, balance, ANC, EQ, hearing, fit, prompt volume,
  wear sensitivity, find) and neither the function name nor the protocol table
  row mentions firmware, upgrade, OTA or update. `standby` is covered by the
  auto-off control when the handler offers it.

Socket commands (the daemon answers them only after connecting):

| command | args | result |
|---|---|---|
| `fnList` | none | `[{cmdId, type (toggle, list, action), kind, label, options[{label,value}], state, raw, text, restart, confirm, danger}]` |
| `fnSet` | `cmdId`, `value` (or `on`/`off`), `confirm?`, `confirmAgain?` | `{cmdId, sent, confirmed, state, raw, restart, note}` |

`value` is `on`/`off` for a toggle; for a list an option `label`, its hex
`value`, or for a standby list the minutes or `never`. A function with a
`rebootTip`, `alarmMsg` or `confTip` needs `confirm:true`; a `command` (reset,
clear pairing, factory reset) needs `confirm:true` and `confirmAgain:true`. A
write is sent once; it is read back with `FE 01 <cmdId>` (toggle on-state: the
declared on parameters; off: the declared off parameters, or 0/2 for a 01-on
toggle). A silent unit, or a link that drops after a function that declares a
restart, is reported as `restart:true`, never as an error and never repeated; a
reply that differs from the value written is an error.

`status` carries `functions` (cmdId -> `on`, `off`, an option value or
`raw:<hex>`) only when the model declares generic functions. Readings are cached
per session; a function that does not answer is not asked again until the next
connection (a pass gives up after three silent queries in a row).

CLI: `qcyctl fn list [--json]`, `qcyctl fn set <cmdId> <value|on|off> [--yes]`
(`--yes` for a restart warning, `--yes --yes` for a reset-type command). The
plugin shows the section "More controls" in Ajustes, hidden when the list is
empty.

### Reading both ears (measured on an N70, firmware 3.0.13; other models inferred)

While slot 11 is the active slot, the generic query frame (`FF 03 FE 01 <op>`)
with `op` 0x46 or 0x47 is answered by a frame of opcode **0x22** (not 0x46/0x47)
whose payload is that ear's curve, in the same layout as a 0x22 report: slot byte
0x0b, master gain, then the bands (the prefix left by the previous effect plus the
8 ear bands). Measured: the reply to 0x46 is byte-identical to the 0x22 report
(the left ear is what 0x22 reports), the reply to 0x47 is the right ear and is
byte-identical to what was written to it; the order of queries (47, 46, 47) does
not change the answers. With slot 255 active the same queries answer with the
active 0x22 EQ, not an ear, so the per-ear read only exists while slot 11 is
active.

The daemon reads it only when the 0x22 report says slot 11 and the model's panel
declares the hearing test, under one transaction lock: 0x22 (opening), query 0x46,
query 0x47, 0x22 (closing). The read is discarded unless the opening equals the
payload the status saw, the closing equals the opening (the EQ did not change in
between), the 0x46 reply equals the opening, both replies are slot 11 with the
same master gain and carry at least 8 bands, no second, different 0x22 frame
crosses a reply, and the 8 bands of each ear pass the ear-curve validation. The
last 8 bands of each reply are the ear. The result is cached by the 0x22 payload;
a failed read is not retried for 60 s. It is exposed in the status as
`eq.ears` (see CLI.md).
