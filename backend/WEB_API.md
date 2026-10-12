# QCY web API (read from the official app, verified on a MeloBuds N70)

Source: the QCY phone app's web interface and
one anonymous request made on 2026-10-13. Nothing here needs an account.

The BLE protocol does not carry model names, feature lists or effect names
(PROTOCOL.md). The app gets them from this API, keyed by the 16-bit `vendorId` the
earbuds broadcast (manufacturer data, company id `0x521c`, bytes 0-1, big-endian).

## Request that works without login

```
POST https://api.watch.qcy.com/product/findControlPanels
Content-Type: application/x-www-form-urlencoded
lang: en          country: US          sys_: android          appversion: 4.0.7_717

modelId=23872&firmwareVersion=3.0.13
```

- `modelId` is the **vendorId** of the earbuds (N70: 23872 and 23877; 23873-23875 are other products, 23876 is not in the database),
  *not* the product `modelId` 21020 of the product list: that one returns `data: null`.
- `firmwareVersion` is the version string the earbuds report (opcode `0x07`). An empty
  value answers `0.0.0` (a generic panel).
- The same path on `appcontent.qcy.com` returns `data: null`: use `api.watch.qcy.com`.
- The reply is `{"code":200,"msg":"success","data":{"firmwareVersions":...,
  "firmwareversionGte":"0.0.0","firmwareversionLte":"999.999.999","controlPanel":{"layouts":[...]}}}`.
- `Authorization: Bearer <token>` is sent by the app only when logged in; `user/touristMode`
  creates a guest user for calls that need a `userId` (not needed here).

Other endpoints seen in the code (not called): `product/findProductList` (ZIP with the
product database, 199 products; the Quicky module embeds a copy), `public/findSoundMarkets`
and `public/useSoundMarket` (the EQ "market", needs a `userId`), `earphoneoOta/findEarphoneoOta`,
`cuetone/*`, `pairingName/findAllPairingNames`, `devicebind/*`, `earphoneTheme/*`.

## What the N70 panel says (layouts, by `type`)

| type | name | content |
|---|---|---|
| 10 | button function | touch/double/triple/quad/hold events, action ids (0 none, 1 play/pause, 2 prev, 3 next, 4 assistant, 5/6 volume, 7 game mode, 11 ANC) |
| 9 | noise canceling | modes with `startcmdid`/`endcmdid` ranges (adaptive 66816, indoor 65792..65794, commuting 66048..66050, noisy 66304..66306 ...) |
| 2 | sound effect | 10 bars 31..16000 Hz, `mindb`/`maxdb` **8**, `sys_eq`, `individuation_eq` (below) |
| 8 / 3 | balance / find headset | |
| 102 | timing power-off | `cmdid` 20 (0x14) |
| 110 | fit detection | `cmdid` 17 (0x11) |
| 104 | wear detection | `cmdid` 44 (0x2C), sensitivity `cmdID` 72 (0x48): 1 medium, 2 normal, 3 high |
| 103 | beep volume | `cmdid` 29 (0x1D) |
| 1001 / 101 / 1000 | tone switch / rename / power off | |

### Sound effect: the official factory effects

All eight carry `mastergain -12`. That is the vendor's own value, not an artefact of
this tool: it is the overall gain every effect is meant to be played at (the app writes
-12 for its own custom curves too). Gains in dB, Q as the server writes it (raw = x100),
type as the `iitType` byte.

| id (slot) | server name | master | bands (Hz: dB, Q, type) |
|---|---|---|---|
| 1 | Default | -12 | 200: -9, .7 · 900: -1, .7 · 2800: +2, 1.2 · 8500: -7, 3.5 · 7000: -1, 1.2 (t3) · 15000: -2, 4 |
| 2 | Pop | -12 | 60: -3, .5 · 200: -9, .7 · 900: -1, .7 · 2800: +2, 1.2 · 8000: -7, 3.5 |
| 3 | Heavy Bass | -12 | 60: +5, .5 · 200: -7, .7 · 900: -1, .7 · 2800: +1, 1.2 · 8000: -7, .8 |
| 4 | Rock | -12 | 60: +5, .5 · 200: -12, .7 · 900: -2, .7 · 2800: +2, 1.2 · 8000: -7, 3.5 |
| 5 | Soft | -12 | 50: -2, .5 · 200: -9, .7 · 900: -1, .7 · 2800: +1, 1.2 · 8000: -8, 1 |
| 6 | Classic | -12 | 60: +3, .5 · 200: -5, .7 · 900: -1, .7 · 2800: +2, 1.2 · 8000: -7, 3.5 |
| 10 | Spatial sound effects | -12 | same bands as Default |
| 14 | Soft Soundstage | -12 | 50: +2, .5 · 200: -8.5, .7 · 900: -1, .7 · 2800: +2, 1.2 · 8500: -10.5, 3.8 · 7000: -1, 1.2 (t3) · 15000: -2.5, 4 |
| 11 | Personalized listening experience (`individuation_eq`) | -10 | 8 bands 63..8000 Hz, Q .7, built from the hearing test (`soundDB`) |

These match, band for band, what this tool read from the earbuds (`library list`), so the
slot ids and the effect names are now confirmed by the vendor rather than inferred.

The phone app's own "Personalizado" (slot 255) is not in the panel: the app writes it
itself (`EQItemView`: custom effects use `soundEffectId` = saveIndex + 19, or 255).

### Things this changes

- The app's own bar range is +-8 dB (`mindb`/`maxdb`). The earbuds accepted +-12 on this
  unit; the panel here allows +-12 (see PROTOCOL.md) — a deliberate difference.
- The N70 panel lists fit detection (0x11), wear sensitivity (0x48) and beep volume (0x1D).
  The capability probe of this tool reports them as absent on this firmware; not resolved.

## Detecção por vendorId

The daemon identifies the model by the `vendorId` (manufacturer data bytes 0-1,
big-endian, company id `0x521c`) instead of by the Bluetooth name:

1. vendorId 23872 or 23877 (exactly these two) -> `melobuds-n70`, the only verified model; 23873-23875 are other products.
2. vendorId present in the Quicky product database (199 products) but not an N70 ->
   `qcy-<vendorId>`, driven by a profile generated from the product entry
   (`Features`: ANC, channel balance, find, auto-off, settings by cmdid 9/16/35/36/42/44
   or by name). It offers only the actions of the shared command set that the product
   declares; EQ is never offered or written, `ldac`/`promptVol` stay read-only.
3. otherwise the advertised name, then the firmware string, then `generic`.

The vendorId is only visible in a scan, so it is stored as an extra last field of
`~/.cache/qcyctl/le-address` (`<addr> <random> <name|-> <vendorId>`; older 2- and 3-field
files still load) and used on the fast path, which does not scan.

`capabilities` carries `verified` (true only for the N70), `vendorId` and
`productTitle` (both omitted when unknown). `status` adds a `model` object (also `actions` and `features`, the ids the model declares; the panel hides anything undeclared on an unverified model)
(`modelId`, `displayName`, `verified`, `vendorId`, `productTitle`) and `capabilities.verified`.
The panel shows "Modelo não verificado" in Ajustes > Aparelho when `verified` is false.
