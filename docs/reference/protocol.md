# QCY protocol reference


Status compares each item with `docs/PROTOCOL.md` and `cmd/qcyctld/main.go`. `n/a-N70` means the item belongs to another product class (SD player, speaker LED, head gestures, JL/ZR only) and the N70 panel does not declare it; it is not a promise that the N70 lacks it.

Counts (all rows): implemented-different 9, implemented-same 25, missing 27, n/a-N70 22, n70Extension 9  
Counts (opcode rows only): implemented-different 6, implemented-same 17, missing 22, n/a-N70 22

## Envelope, characteristics, cmdIds

| id | name | dir | frame / bytes | status |
|---|---|---|---|---|
| 0xFE | Query a cmdId | query | FF 03 FE 01 <cid> | implemented-same |
| char 1001 write / 1002 notify+read | Frame envelope | write | FF <bodyLen> { <cmd> <paramLen> <params...> }+   bodyLen = total bytes after it; several blocks may share one frame | implemented-same |
| char 0007 | GATT 0007 firmware version (read) | query | 3 bytes = left a.b.c, 6 bytes = left + right | implemented-same |
| char 0008 | GATT 0008 battery (read + notify) | query | 3 bytes left,right,case; bit7 = charging, bits0-6 = level | implemented-same |
| char 000D | GATT 000D key functions v2 (read, write, notify) | write | write: raw pairs [eventId, funId]... (no FF envelope); read/notify: pairs | implemented-same |
| char 000B | GATT legacy EQ characteristic | query | legacy EQ char: read = [type, 10 gain bytes (1 dB/unit) ...]; write = [type\|0x80 variant, gains]; (type&0x80) adds the panel eqBaseGain on write and subtracts it on read | implemented-same |
| char 000F | GATT legacy function char | query | legacy function V1 (UUID_FUNCTION_V1): byte0 = in-ear detection (0xFF = unset), byte1 = byte shown as bool cmd 10 | missing |
| char 0001..0006 (+0007/0008 CL4/CR4, 0009/000A CLL/CRL) | GATT legacy per-key characteristics, used when 000D is absent; 07/08/09/0A collide with version/battery/language/reset | query | legacy V1 key functions (UUID_KEY_FUNCTION_C[LR][1-4/L]_V1): one byte per characteristic | missing |
| char 000A | GATT legacy reset/OTA trigger | query | legacy reset (UUID_RESET_V1): write [01]=restore settings, [02]=clear paired, [03]=factory reset; OTA writes [05] (reboot into OTA) | missing |
| char 000C | GATT legacy time sync char | query | legacy time sync (UUID_SENDTIME_V1): 4 bytes epoch seconds big endian | missing |
| char 0009 | GATT read-only language read (UUID_LANUAGE) | query | language string, read only: comma separated, first token is the current language | missing |
| 0x01 | Restore default settings | write | FF 02 01 00 (getResetDefaultCMD [01 00]) | missing |
| 0x02 | Clear paired records | write | FF 02 02 00 | missing |
| 0x03 | Factory reset | write | FF 02 03 00 | missing |
| 0x04 | Music control (play/pause/prev/next) | write | FF 03 04 01 <a>   a: 1 play, 2 pause, 3 previous, 4 next (0 used by SD player as toggle/stop) | n/a-N70 |
| 0x05 | LED flash (Quicky NewLightFlash) | write | FF 03 05 01 <1\|0> | implemented-different(daemon maps action find to 0x05 in opcodeMap, qcyctl find does NOT send it: local tone only) |
| 0x06 | In-ear detection (legacy, 'ruer') | write | FF 03 06 01 <1 on\|2 off> | n/a-N70 |
| 0x07 | ANC strength (legacy) | write | FF 03 07 01 <v>; read request = FF 03 07 01 FF (sent through sendBleData, not FE) | implemented-different(we query with FE 01 07 and treat silence as absent; the app reads with [07 01 FF]) |
| 0x08 | Media volume on the earbuds (SD/speaker) | write | FF 05 08 03 <l> <r> 00 | n/a-N70 |
| 0x09 | Game / low-latency mode | write | FF 03 09 01 <1 on\|2 off> | implemented-same |
| 0x0A | Hear-through ('jianting') | query | (no writer in the code; cmd id only) | n/a-N70 |
| 0x0C | ANC mode (legacy single byte) | write | FF 03 0C 01 <m>   m: 1 ANC, 2 outdoor, 3 transparency | missing |
| 0x0D | Touch test mode | write | FF 03 0D 01 <1 on\|2 off> | missing |
| 0x0F | Time sync v2 | write | FF 06 0F 04 <t3> <t2> <t1> <t0>  (epoch seconds, big endian) | missing |
| 0x10 | Sleep mode | write | FF 03 10 01 <1 on\|2 off> | implemented-same |
| 0x11 | Fit (ear-tip seal) test | write | FF 05 11 03 <v> <v> <v>   v=1 start, 0 stop (CM.setCompactnessEnable; same byte three times) | implemented-same |
| 0x12 | Status LED on/off | write | FF 03 12 01 <1 on\|2 off> | missing |
| 0x14 | Timed power-off | write | FF 06 14 04 <lo> <hi> 00 00   minutes; 65535 = never; 0 = power off NOW | implemented-same |
| 0x16 | L/R balance | write | FF 03 16 01 <0..100>   50 centre | implemented-same |
| 0x17 | ANC mode / scene / level | write | FF 05 17 03 <mode> <subScene> <level> | implemented-same |
| 0x18 | Bluetooth pairing name | write | FF <n+2> 18 <n> <utf8 name...> | missing |
| 0x19 | Prompt language (voice pack code) | write | FF <n+2> 19 <n> <ascii code...> | missing |
| 0x1D | Prompt (tone) volume | write | FF 04 1D 02 <v> 00  (vendorId 16737: FF 03 1D 01 <v>) | implemented-same |
| 0x1E | Take photo trigger (notify) | notify | [30 01 <v>] from the earbuds | n/a-N70 |
| 0x1F | Standby state | query | FE 01 1F -> reply (1 or 2 bytes, LE) | missing |
| 0x20 | Multi-band EQ v1 (6 bytes per band, no type) | write | FF <6n+5> 20 <6n+3> <slot> <master lo> <master hi> { <f lo> <f hi> <g lo> <g hi> <q lo> <q hi> }*n | implemented-different(only 0x22 is written; 0x20 listed unverified in PROTOCOL.md) |
| 0x22 | Multi-band EQ v2 (7 bytes per band, with type) | write | FF <7n+5> 22 <7n+3> <slot> <master lo> <master hi> { <f lo> <f hi> <g lo> <g hi> <q lo> <q hi> <type> }*n | implemented-different(app writes slot 128 when a last-used record exists; we write 255 as measured; we write 73 bytes (10 positions) when the tail is empty and the pad is 0xFF type only for slot 255) |
| 0x23 | LDAC | write | FF 03 23 01 <1 on\|2 off> | implemented-same |
| 0x24 | Multipoint / dual device | write | FF 03 24 01 <1 on\|2 off> | implemented-same |
| 0x27 | Adaptive EQ trigger (game effect) | write | FF 03 27 01 01 | n/a-N70 |
| 0x28 | ANC calibration result (notify) | notify | [28 01 <1 ok>] | missing |
| 0x29 | Wear-state result (notify) | notify | [29 01 <v>] or [29 02 <l> <r>] (single byte is duplicated) | missing |
| 0x2A | Wind-noise detection | write | FF 03 2A 01 <1 on\|0 or 2 off> | implemented-same |
| 0x2B | Key functions (cmd form, notify) | notify | [2B 2n {keyId funId}*n] on 1002; query FE 01 2B | missing |
| 0x2C | Wear (in-ear) detection | write | FF 05 2C 03 <enable 0\|1> <musicIdx> <ancIdx>   (4th byte toneEnable exists on read) | implemented-same |
| 0x2D | Spatial audio | write | FF 03 2D 01 <idx>   (idx from panel items) | implemented-different(opcode 0x2D wired as [1\|2]; the app writes an item index and defaults to 0x5B) |
| 0x2E | Music mode (EQ vs game etc.) | write | FF 03 2E 01 <tag> | missing |
| 0x2F | Battery (cmd form) | notify | [2F 03 L R case] | implemented-same |
| 0x30 | Version (cmd form) | query | FE 01 30 | implemented-same |
| 0x32 | Environment adaptation | write | FF 03 32 01 <0\|1> | missing |
| 0x34 | TWS enable | query | (constant only; decoded as the vendor app) | n/a-N70 |
| 0x35 | LED switch | write | FF 03 35 01 <0\|1> | n/a-N70 |
| 0x36 | LED effect | write | FF <3k+5> 36 <3k+3> <speed> <bright> <effect> {R G B}*k | n/a-N70 |
| 0x37 | Player mode | notify | the vendor app; 255 = ignored | n/a-N70 |
| 0x39 | BT / SD source mode | write | FF 03 39 01 <0 bluetooth\|1 sd card> | n/a-N70 |
| 0x3A | Local music state | write | FF 08 3A 06 <id0..id3 LE> <playing 0\|1> <mode> | n/a-N70 |
| 0x3B | Local music file list | write | FF <6n+3> 3B <6n+1> 00 {id0..id3 LE, total lo, hi}*n | n/a-N70 |
| 0x3D | Tone-preview flag | write | FF 03 3D 01 <1 playing\|0 stopped> | implemented-different(PROTOCOL.md calls it [1,toneId]; the app writes a 0/1 flag; not used by us) |
| 0x3E | Date/time sync (speakers) | write | FF 09 3E 07 <yy> <mm> <dd> <hh> <mi> <ss> <weekdayBit> | n/a-N70 |
| 0x3F | Alarm clock list | write | FF 09 3F 07 <op 1 add\|2 delete\|3 edit> <id> <enabled> <h> <m> <cycle> <extra> | n/a-N70 |
| 0x43 | AI/noise status (notify) | notify | the vendor app | missing |
| 0x44 | Max EQ band positions | query | FE 01 44 -> u16 LE | missing |
| 0x45 | Hearing-test mode | write | FF 03 45 01 <1 on\|0 off> | implemented-same |
| 0x46 | Hearing curve, left ear | write | FF <7n+5> 46 <7n+3> <index=11> <master lo hi> {f lo hi, g lo hi, q lo hi, type}*n | implemented-same |
| 0x47 | Hearing curve, right ear | write | FF <7n+5> 47 <7n+3> <index=11> <master> {band*7}*n | implemented-same |
| 0x48 | Wear-sensor sensitivity | write | FF 03 48 01 <1 medium\|2 normal\|3 high> | implemented-same |
| 0x4A | Game mode config | write | FF 04 4A 02 <on> <tip> | missing |
| 0x4D | Paired phones list / action | write | FF 09 4D 07 <action 1 connect\|2 disconnect\|3 remove> <mac6>; query FE 01 4D | missing |
| 0x4F | Phone link status (notify) | notify | [4F 07 mac6 status] | missing |
| 0x50 | Head-gesture enable | write | FF 03 50 01 <0\|1> | n/a-N70 |
| 0x51 | Head-gesture function map | write | FF 04 51 02 <eventId> <funcId> | n/a-N70 |
| 0x52 | Head-gesture test | write | FF 03 52 01 <250 start\|251 stop (inferred)> | n/a-N70 |
| 0x54 | AI ANC enable/state | write | FF 08 54 06 <0 on\|4 off> 00 00 00 00 01 | n/a-N70 |
| 0x55 | AI ANC default config (read) | query | FE 01 55 -> 45 bytes = 9 x [status, scene, mode, sub, level] | n/a-N70 |
| 0x56 | AI ANC user config | write | FF 07 56 05 <status> <scene> <mode> <sub> <level>   (one frame per entry) | n/a-N70 |
| 0x58 | Hearing-health limiter | write | FF 06 58 04 <on> <timeSwitch> <idx lo> <idx hi> | missing |
| 0x5A | AI ANC preview | write | FF 06 5A 04 <op 1 enter\|2 try\|3 exit> <mode> <sub> <level> | n/a-N70 |
| 0x5B | Spatial audio mode | write | FF 03 5B 01 <idx> | missing |

## Flows

- **Connect sequence** (implemented-different(we connect lazily and release after an idle window; no MTU renegotiation recorded; 0x44 not queried)): enable notify on every characteristic with the NOTIFY property; MTU change; +1000 ms FE 01 44; then readSettingStatus / key functions / EQ / ANC / battery / version as the panel needs. Pre: link up (connectDevice rate-limited to once per second per mac). Post: EventBus 42; time sync lazily. BLE connect retries 2, service discovery retries 3.
- **EQ write transaction (app)** (implemented-different(we read-patch-backup-write and confirm; the app writes blind from its own model)): max-gain compute -> build bands (panel freq/Q/type + last-used non-edited bands) -> drop extras above cap by nearest frequency -> pad -> write 0x22 (or 0x20) -> save last-used record. Pre: single worker thread (ThreadUtils.executeBySingle); not read-modify-write; no backup. Post: saveCurrentEQParams, setUuidString, EventBus 70. Band cap n = min((mtu-7)/6, MAXEQ or 20). If more bands than the cap, bands nearest in frequency to a user-edited one are dropped first. Vendor 4579 with effect 240 appends 100/150/200 Hz notches.
- **Find my earbuds** (implemented-different(local tone with ceiling, timeout and warning)): none (no BLE frame). Pre: phone plays a looped sound and raises the media volume. Post: . Confirmed in the vendor app (PROTOCOL.md). Listed so nobody searches for an opcode.
- **Hearing-test flow (app)** (implemented-same): 0x45 [1] -> app plays tones locally and records thresholds -> 0x46 left, 0x47 right (index 11, 8 bands 63..8000 Hz) -> 0x45 [0] on exit. Pre: isStartTest in CEQ2; both curves built from the result with getMaxGain per ear. Post: GSON-cached LeftEQEdit/RightEQEdit on the phone; EventBus 70 index 11. The app keeps the left/right curves on the phone and re-sends 0x46/0x47 when the effect is re-selected.
- **Panel-driven generic writes** (implemented-same): raw bytes = server panel `cmd` hex, sent on the panel `character` (normally 1001): switches use sub_items[boolVal].cmd; lists use SubItemList/SubStringItemList hex; mode selects use [cmd 01 tag]. Pre: N70 panel declares: ANC, EQ, balance, find, 0x14, 0x11, 0x2C/0x48, 0x1D, name, tones, switches 9/16/35/36/42/44. Post: uploadFuncSetting to the server (type 4/6/8), KV cache SINGLEVALUE_<mac>_<cmd>. This is why most opcodes above have no hard-coded byte sequence in the app: the exact frame is server data (our docs/WEB_API.md has the N70 panel).

## Differences against our implementation

- **0x05 LED flash (Quicky NewLightFlash)**: daemon maps action find to 0x05 in opcodeMap, qcyctl find does NOT send it: local tone only
  - The app's find-earbuds does not use it. Verified no effect while connected on the N70.
- **0x07 ANC strength (legacy)**: we query with FE 01 07 and treat silence as absent; the app reads with [07 01 FF]
  - Our probe asks 0x07 with FE and stays silent on the N70 (verified); the app's read frame is the write-shaped [07 01 FF], not FE. Not tried by us (harmless to try).
- **0x20 Multi-band EQ v1 (6 bytes per band, no type)**: only 0x22 is written; 0x20 listed unverified in PROTOCOL.md
  - Chosen when the panel customEQ JSON has no iitType list; also for vendorIds 19779/19781/19783/... raw saved EQs (CM:~600). Gain = round(g_dB*100) int16 LE, Q = round(Q*100) u16.
- **0x22 Multi-band EQ v2 (7 bytes per band, with type)**: app writes slot 128 when a last-used record exists; we write 255 as measured; we write 73 bytes (10 positions) when the tail is empty and the pad is 0xFF type only for slot 255
  - Slot byte: factory preset id 1..14 as the panel 'id'; custom curves: saveIndex 255 or 0 becomes 255 when no last-used EQ record exists and 128 (0x80) when one does (CM:~574). Unused positions are padded to the cap with freq 0, gain 0, Q 0, type 0xFF; app UI range is +-8 dB (EqViewCusomTest, panel mindb/maxdb 8). Read-back slots with (id&127)==122 are mapped to 255; ids <122 except 11 refresh the stored base gain.
- **0x2D Spatial audio**: opcode 0x2D wired as [1|2]; the app writes an item index and defaults to 0x5B
  - the vendor app default spaceCMDID is 91 (SPACE_AUDIO_MODE); JSON may override with 45. Our probe of 0x2D stays silent on the N70.
- **0x3D Tone-preview flag**: PROTOCOL.md calls it [1,toneId]; the app writes a 0/1 flag; not used by us
  - Value is a 0/1 flag, not a tone id; our PROTOCOL.md describes 0x3D as [1, toneId] (no effect measured).
- **Connect sequence Connect sequence**: we connect lazily and release after an idle window; no MTU renegotiation recorded; 0x44 not queried
  - BLE connect retries 2, service discovery retries 3.
- **EQ write transaction (app) EQ write transaction (app)**: we read-patch-backup-write and confirm; the app writes blind from its own model
  - Band cap n = min((mtu-7)/6, MAXEQ or 20). If more bands than the cap, bands nearest in frequency to a user-edited one are dropped first. Vendor 4579 with effect 240 appends 100/150/200 Hz notches.
- **Find my earbuds Find my earbuds**: local tone with ceiling, timeout and warning
  - Confirmed in the vendor app (PROTOCOL.md). Listed so nobody searches for an opcode.

## Documentation discrepancies found

- PROTOCOL.md `0x1E` 'music control': in the app 0x1E (30) is (opcode); music control is 0x04 ((opcode)).
- PROTOCOL.md `0x35` 'EQ test': 0x35 (53) is (opcode); the EQ/hearing test switch is 0x45 (69), which the hearing code already uses.
- PROTOCOL.md `0x43` 'touch remap': 0x43 (67) is (opcode) (notify); touch remap is only characteristic 000D.
- PROTOCOL.md `0x3D` 'play a tone [1, toneId]': app writes a 0/1 flag (FF 03 3D 01 <0|1>) while the phone previews audio.
- PROTOCOL.md `0x2D` spatial `[1,1|2]`: the app writes an item index and defaults to 0x5B ((opcode)).
- PROTOCOL.md `0x0C` ANC mode is the legacy single-byte mode (1 ANC, 2 outdoor, 3 transparency); the packed mode<<16|sub<<8|level always goes to 0x17.
- `docs/app` row 0x17: reading (3,2,x) is shown by the app as the packed 0x030100 (transparency); our (3,1,0)->(3,2,0) write rewrite is the inverse of that.

## N70 extensions (where we go beyond the app)

| item | app | ours | evidence |
|---|---|---|---|
| 20 positions / 143-byte write | allowed when MAXEQ_COUNT (0x44) and MTU permit; JL capped at 10 | refused unless slot 255 or --full-length; 73-byte write otherwise | PROTOCOL.md Write format |
| Auto-off 0 | 0 minutes = power off now (Power button) | refused | main.go prepareSetter autoOff |
| EQ gain range | UI and server panel limit bars to +-8 dB (EqViewCusomTest maxGain 8; panel mindb/maxdb 8); legacy byte path clamps at 12 (SysEQSeted.setEqs 127) | bands and master gain accepted -12..+12 dB (centi-dB int16) | docs/PROTOCOL.md EQ (factory curves reach -12; slot 255 write measured) |
| Find with limits | loop, system volume to maximum, no timeout | tone peak <= -20 dBFS, 20% start, 70% ceiling, 60 s timeout, warning, restores volume | PROTOCOL.md Find my earbuds |
| Hearing baseline zero | sends exactly the 8 test bands; firmware prefixes the 5 factory bands | --baseline zero sends 13 bands (5 factory at gain 0 + 8); readback accepts 13 or 18 | PROTOCOL.md hearing experiment (opt-in, NOT verified) |
| Master gain | never user-set: getMaxGain derives it from curve peak and base -10 dB (volumeLevel +0.5/+4 dB rules) | settable (-12..+12), kept from the active slot when omitted, -12 default for compose | PROTOCOL.md Master gain, compose |
| Protected slots / backup / readback confirm | no backup, no confirm, no protection | slots 6 and 11 refused for 0x22; backup file before every write; readback confirm | PROTOCOL.md Protected slots |
| Slot 255 compose | app composes user bars + chosen factory effect bands itself and writes slot 128/255 with up to the MAXEQ cap | library compose builds the same layout (<=20 positions, 143-byte payload, 0xFF type tail) with --dry-run | PROTOCOL.md compose; preset-map teste2-slot255 measurements |
| Wind/wear off byte | writes 0x00 for off | writes 0x00 on verified models (verified 2026-10-13), 0x02 elsewhere; 0x02 also reads as off | PROTOCOL.md 0x2A/0x2C |

## Doubts / not resolved from the app alone

- Custom EQ slot: app picks 128 (0x80) when a last-used EQ record exists and 255 otherwise; the N70 captures show 255. Either the record is absent in the captures or the firmware maps 128 to 255. Not resolved.
- 0x44 MAXEQ_COUNT value on the N70 is not recorded; we never query it.
- 0x52 head-gesture test second value (251) is not explained by the documented data.
- Fit result strings per ear (3 sealed ... 0 poor) come from our hardware verification, not the app bean (which only exposes raw status/left/right).
- Panel switches send server-provided bytes; every `FF 03 <cmd> 01 <1|2>` form above is the shape we verified, not a string found in the app.
- vendorId special cases: 16737 (tone volume one byte), 16758 (ANC throttle), 19784 (ANC needs both buds), 4579 (extra EQ notches), 19779/19781/19783 (raw EQ via 0x20).
