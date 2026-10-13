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
| `0x11` | fit test | write `[1, 1, 1]` start / `[0, 0, 0]` stop; notification `[status, left, right]` (status 2 = final, 0 = ready; per ear 3 sealed, 2 good, 1 fair, 0 poor); never queried | read in the app; **verified on hardware 2026-10-13**: `[1,1,1]` plays the test sound and the final notification arrived as `[2, 1, 3]` (left poor, right sealed); the old `[2,1,1]` / `[2,2,2]` frames got no reply |
| `0x14` | timed power-off | `[4, lo, hi, 0, 0]` | verified, 4 bytes not 2 |
| `0x16` | L/R balance | `[1, value]` | verified |
| `0x17` | ANC setting | `[3, mode, subScene, level]` | verified |
| `0x1D` | prompt volume | query reply `[value, max]`; write `FF 04 1D 02 <v> 00` (two parameter bytes), v in 4/6/8/10 (Quieter / Medium / Louder / Maximum) | read in the app; **verified on hardware 2026-10-13**: `FF 04 1D 02 v 00` for v = 4, 6, 8, 10 each read back as written; one-byte writes of 0/3/9/12/15 were ignored (see [Prompt volume](#prompt-volume)) |
| `0x48` | wear-sensor sensitivity | write `FF 03 48 01 <p>`, p 1 medium / 2 normal / 3 high; query `FF 03 FE 01 48` -> `<p>` | read in the app; **verified on hardware 2026-10-13**: 1, 2 and 3 each read back as written (value was 2). Only meaningful with wear `0x2C` on |
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
| prompt volume `0x1D` | one-byte writes of 0, 3, 9, 12, 15 were **ignored** (always 6 of 15); the app's two-byte frame `FF 04 1D 02 v 00` with v = 4, 6, 8, 10 **honours the write** (verified 2026-10-13) |
| wear sensitivity `0x48` | `FF 03 48 01 p` with p = 1, 2, 3 honours the write (verified 2026-10-13) |
| ANC `0x17` | not exercised — rejected unless both earbuds are worn |
| spatial `0x2D` | no query reply on this model |
| fit `0x11` | no query reply (the app never queries it); start `FF 05 11 03 01 01 01` / stop `FF 05 11 03 00 00 00` verified 2026-10-13: the final notification `[2, left, right]` arrives |
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
`qcycore.ProtectedSlots`. Enforced inside `ApplyEq`, so the CLI (direct and via
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
  | 1 | Predefinido | medido |
  | 2 | Popular | medido |
  | 3 | Bass pesados | medido |
  | 4 | Rock | medido |
  | 5 | Suave | medido |
  | 6 | Clássico | medido |
  | 7, 8, 9 | ? | não observado |
  | 10 | Som espacial (payload identical to slot 1) | medido |
  | 11 | Experiência auditiva (the user's hearing-test profile) | medido |
  | 12, 13 | ? | não observado |
  | 14 | Agudos suaves | medido |
  | 255 | Personalizado | medido |

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
(Predefinido, Popular, Bass pesados, Rock, Suave, Clássico, Agudos suaves, Som
espacial), then Experiência auditiva, then your own curves (source user, by
name), then everything else (device imports such as the old slot 0 curve and
"Personalizado (slot 255)"). Older seeds that a newer curve replaces
(`classico-slot-6`, `slot-11-primeira-leitura`, `experiencia-auditiva-slot-11`,
`<effect>-slot-N`, `slot-N-primeira-leitura`; see `qcycore.SupersededCurveIDs`)
are hidden by default and shown by `--all`; they are never deleted or renamed
and stay valid for `show`/`apply`/`delete`. `slot-0-primeira-leitura` has no
replacement and is always listed. A user's own curve is never hidden.

**`seed`** is idempotent and never renames, replaces or deletes a curve (an id
already in the library is skipped, whatever its source). It creates:

| Curve (id) | Source | Slot | From |
|---|---|---|---|
| Predefinido, Popular, Bass pesados, Rock, Suave, Clássico, Agudos suaves, Som espacial | factory-snapshot | 1, 2, 3, 4, 5, 6, 14, 10 | payloads **embedded in the binary** (`internal/qcycore/factory.go`; public factory data, measured on the device) |
| Experiência auditiva (`experiencia-auditiva`) | device-import | 11 | `<backups>/eq-slot11-first.hex`, else `<backups>/preset-map/experiencia-auditiva-slot11-*.json` (`payload`); skipped with a reason when neither exists |
| Slot 0 (primeira leitura), Personalizado (slot 255), ... | device-import | N | every other `<backups>/eq-slotN-first.hex`, named as older versions named them; factory slots and 11 are no longer seeded this way, the curves above replace them |

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
read-only. The write is limited to verified models (verified on the N70): a model that is not verified refuses it.

The two other controls, also limited to verified models (verified on the N70), read from the same app:

| Control | Write | Query | Values |
|---|---|---|---|
| Prompt volume `0x1D` | `FF 04 1D 02 v 00` | `FF 03 FE 01 1D` -> `<value> <max>` | v 4 / 6 / 8 / 10 |
| Wear-sensor sensitivity `0x48` | `FF 03 48 01 p` | `FF 03 FE 01 48` -> `<p>` | 1 medium, 2 normal, 3 high |
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
`{"ok": false, "error": "earbuds are in use by another app (close the phone app and retry)", "code": "busy"}`.
Further requests fail fast with the same answer for 3 s. `status` and `battery`
instead return the last valid reading when one exists, with two extra fields,
`"stale": true` and `"ageSeconds": N`; fresh readings carry neither. `code` is
absent on every other error except `not_found`. `capabilities` is answered from the known model
without connecting.

If the scan finds nothing and no cached address answers, the daemon replies `{"ok": false, "error": "earbuds not found: powered off or out of range", "code": "not_found"}`: the earbuds are off or out of range, not held by another app. Only `In Progress`, `Already Connected`, `Connection Refused`, `Resource Busy` and `busy` count as `busy`; timeouts, aborts and `Not Connected` go on to the scan. A write that reaches the device drops the cached readings, so `status` and `battery` never answer `stale` after it.

| `cmd` | `args` | `data` |
|---|---|---|
| `status` | none | raw per-opcode dump (`eq` is the raw 0x22 byte array) |
| `battery` | none | battery object |
| `capabilities` | none | model capabilities |
| `set` | `action`, `params`, optional `expect` | confirmed payload bytes or null; `action: "eq"` is refused |
| `eq` | none | `{slot, effect (when known), masterGain (dB), bands[{freq,gain,q,type}], payload (hex)}` |
| `eqApply` | optional `gains` (10 ints, centi-dB), `masterGain` (int, centi-dB), `slot` (int), `bands` (array of `{index, freq, gain (dB, number), q, type}`, exclusive with `gains`), `allowProtected` (bool), `allowFull` (bool). Nothing else: unknown keys and raw payloads (`payload`, `raw`, `hex`, ...) are rejected | the `eq` object plus `previous`, `fullLength`, `backupFirst`, `backupHistory` |
| `eqMasterGain` | `masterGain` (int, centi-dB), required; optional `allowProtected`, `allowFull` | same as `eqApply` |
| `wear` | `state`: 1 on / 2 off | confirmed `0x2C` payload (byte array) |
| `fitTest` | optional `seconds` (5..60, default 30) | `{status, left, right}` once the notification with status 2 arrives; the stop frame is always sent at the end; verified models only (verified on the N70) |
| `fitStop` | none | cancels a running `fitTest` (which then fails with `fit: cancelled`), otherwise sends the stop frame |
| `noise` | none | raw `0x17` payload `[mode, scene, level]` |
| `find`, `keys`, `keysSet`, `disconnect` | as before | |

`set` also accepts `promptVol` (`params` `[v]`, v in 4/6/8/10; sent as `[v, 0]`) and `wearSensitivity` (`params` `[1..3]`), both limited to verified models (verified on the N70) and read back after ~200 ms; `status` carries `wearSensitivity` (1..3) only when the unit answers `0x48`.

`eqApply`, `eqMasterGain`, `keysSet` and `fitTest` are refused on a model that
is not verified (`<cmd> is disabled: <model> is not a verified model`), and so
are the `promptVol` and `wearSensitivity` setters. `wear` is refused unless the
model's profile declares wear detection. `status` carries a `model` object:
`{modelId, displayName, verified, actions, features, vendorId?, productTitle?}`.

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

The vendorId is stored as the last field of `~/.cache/qcyctl/le-address` so
the fast path (no scan) keeps it. Source of the product data, the embedded
snapshot and its limits: `WEB_API.md`.

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
