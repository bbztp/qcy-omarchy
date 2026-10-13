import QtQuick
import Qt5Compat.GraphicalEffects
import Quickshell.Io
import qs.Commons
import qs.Ui

// QCY earbuds widget: the QCY wordmark in the bar and a popup in three tabs:
// Status (battery, connection, noise mode), Som (live EQ curve + the curve
// library: click a card to apply it) and Ajustes (scenes, toggles, balance,
// touch controls, device info).
//
// Actions apply immediately with optimistic UI; no wait for a scan. Full status
// is read on (re)connect, on manual refresh, and once a minute while the panel
// is open; battery is read on open and every 20s while open.
//
// Bar button — left click: panel · right click: re-read
// Panel — Esc: close · r: refresh · 1/2/3: tabs
Panel {
  id: root
  moduleName: "io.github.bbztp.qcy"
  ipcTarget: "io.github.bbztp.qcy"
  // Own the single IpcHandler the target permits so refresh() can be routed.
  manageIpc: false
  // Only exist while a matching device is connected; the bar collapses an
  // invisible slot to zero, so nothing shows when the earbuds are away.
  visible: Qcy.connected

  readonly property color foreground: bar ? bar.foreground : Color.foreground
  readonly property color accent: Color.accent
  readonly property string fontFamily: bar ? bar.fontFamily : Style.font.family
  readonly property bool vertical: bar ? bar.vertical : false

  // Optimistic toggle state while a setter is in flight, cleared when a fresh
  // report lands. Lets the switch throw immediately without waiting a scan.
  property var optimistic: ({})
  property int nowTick: 0

  // Which of the three panel tabs is showing. Kept here rather than in the
  // panel body so the sections can bind their own visibility to it.
  property int tab: 0

  readonly property var rep: Qcy.report
  readonly property var batt: Qcy.battery ? Qcy.battery : (rep ? rep.battery : null)
  readonly property var noise: rep ? rep.noise : null
  readonly property var togglesReport: rep && rep.toggles ? rep.toggles : ({})


  // Detailed noise scenes (Ajustes). The three headline modes on the Status
  // tab are noiseButtons below.
  readonly property var ancModes: [
    { key: "adaptive", label: Qcy.tr("Adaptive") },
    { key: "indoor", label: Qcy.tr("Indoor") },
    { key: "commuting", label: Qcy.tr("Commuting") },
    { key: "noisy", label: Qcy.tr("Noisy") },
    { key: "antiwind", label: Qcy.tr("Anti-wind") }
  ]

  // Headline modes: glyphs are Material Design Icons code points (headphones,
  // ear-hearing, volume-high). "Cancellation" is the adaptive scene; the
  // finer scenes are in Ajustes.
  readonly property var noiseButtons: [
    { key: "adaptive", label: Qcy.tr("Cancellation"), glyph: 0xF02CB },
    { key: "transparency", label: Qcy.tr("Transparency"), glyph: 0xF07E6 },
    { key: "off", label: Qcy.tr("Normal"), glyph: 0xF057E }
  ]
  readonly property var _cancelScenes: ["adaptive", "indoor", "commuting", "noisy", "antiwind"]

  // LDAC is deliberately absent: writing 0x23 reboots this N70 and, unlike
  // multipoint below, the restart is not documented behaviour. Multipoint IS
  // listed because the N70 product notes state the earphones restart every time
  // Dual Device Connection is toggled — that is expected, not a fault.
  readonly property var toggleDefs: [
    { key: "game", label: Qcy.tr("Game mode"), hint: Qcy.tr("Lower latency") },
    { key: "sleep", label: Qcy.tr("Sleep mode"), hint: Qcy.tr("Disables the touch controls; no effect on the sound") },
    { key: "wind", label: Qcy.tr("Wind detection"), hint: "" },
    { key: "wear", label: Qcy.tr("Wear detection"), hint: Qcy.tr("Pauses when an earbud is removed and resumes when put back") },
    { key: "multipoint", label: Qcy.tr("Multipoint"), hint: Qcy.tr("Restarts the earbuds; this is expected, not a fault") }
  ]

  // ---- Som tab state ----
  // A fill that is only a hint lighter than the panel, from the shell's
  // foreground so it follows the theme.
  readonly property color cardFill: Qt.rgba(foreground.r, foreground.g, foreground.b, 0.06)
  property bool saveOpen: false
  // The user effect whose small menu is open, and the one asked to be deleted.
  property string menuId: ""
  property string pendingDeleteId: ""

  // ---- equalizer state ----
  // Ten bars at the standard 10-band layout (the same table `library save` writes).
  readonly property var editorFreqs: [31, 62, 125, 250, 500, 1000, 2000, 4000, 8000, 16000]
  readonly property real editorMaxDb: 12
  property var editGains: [0, 0, 0, 0, 0, 0, 0, 0, 0, 0]
  property real editMaster: 0
  // The overall gain is only sent once the user has touched it, so an untouched
  // equalizer leaves the effect's own level alone.
  property bool masterTouched: false
  // Why the last "apply" did nothing, when it is not an error (empty bars, no base).
  property string eqNote: ""
  // The effect the bars sit on top of. Set by clicking an effect card and kept
  // once the equalizer applies; empty means "the effect in use".
  property string baseId: ""

  readonly property bool isStale: !!(rep && rep.stale === true)
  readonly property int activeSlot: rep && rep.eq && rep.eq.preset !== null && rep.eq.preset !== undefined
    ? Number(rep.eq.preset) : -1
  // Everything is clickable while the earbuds are connected and read; nothing
  // is blocked by the active slot, the band count or the payload size.
  readonly property bool canApplyLibrary: Qcy.connected && !!rep

  function ancGroup() {
    var s = currentAnc()
    if (s === "transparency") return "transparency"
    if (s === "off") return "off"
    if (_cancelScenes.indexOf(s) !== -1) return "adaptive"
    return ""
  }

  function entrySig(e) {
    if (!e || !e.bands) return ""
    return e.bands.map(function (x) { return x.freq + "/" + x.gain }).join(",")
  }

  // Leftover of the old editor's working copy: kept on disk, never shown.
  function hiddenEntry(e) {
    return !!e && e.id === "rascunho"
  }

  // The list entry the earbuds are playing now, or "". Matched by its bands
  // first (the only test for Personalizado, slot 255), then by the slot the
  // entry came from, then by the effect name the earbuds report.
  readonly property string activeEntryId: {
    var l = Qcy.library
    var slot = root.activeSlot
    var sig = root._eqSig
    var i
    // The effect the user just chose or adjusted stays marked at once; waiting
    // for the readback made the marker drop and come back seconds later.
    if (root.baseId !== "" && (slot === 255 || slot < 0)) {
      for (i = 0; i < l.length; i++) if (l[i].id === root.baseId && !root.hiddenEntry(l[i])) return root.baseId
    }
    if (sig !== "") {
      for (i = 0; i < l.length; i++) {
        if (!root.hiddenEntry(l[i]) && root.entrySig(l[i]) === sig) return l[i].id
      }
    }
    if (slot >= 0 && slot !== 255) {
      for (i = 0; i < l.length; i++) {
        if (!root.hiddenEntry(l[i]) && l[i].source !== "user" && Number(l[i].slot) === slot) return l[i].id
      }
      var eff = rep && rep.eq && rep.eq.effect ? String(rep.eq.effect) : ""
      if (eff !== "") {
        for (i = 0; i < l.length; i++) {
          if (!root.hiddenEntry(l[i]) && l[i].name === eff) return l[i].id
        }
      }
    }
    return ""
  }

  // What the equalizer composes on: the pinned base, else the effect in use.
  readonly property string composeBase: root.baseId !== ""
    ? root.baseId : (root.activeEntryId !== "" ? root.activeEntryId : "none")

  function curveName(id) {
    var l = Qcy.library
    for (var i = 0; i < l.length; i++) if (l[i].id === id) return l[i].name
    return id
  }

  // Header name: the earbuds' own effect name; in Personalizado (255) the name
  // of the list entry with the same bands, else "Personalizado".
  function effectName() {
    if (!rep || !rep.eq) return "—"
    var id = root.activeEntryId
    var adj = root.adjusted && id === root.baseId
    if (root.activeSlot === 255) {
      if (id === "") return Qcy.tr("Custom")
      return adj ? Qcy.tr("{name} (adjusted)", { name: root.curveName(id) }) : root.curveName(id)
    }
    var eff = rep.eq.effect
    if (eff !== undefined && eff !== null && String(eff) !== "") return String(eff)
    if (id !== "") return root.curveName(id)
    return root.activeSlot >= 0 ? Qcy.tr("Custom") : "—"
  }

  // The bars or the overall gain were moved on top of the effect in use.
  readonly property bool adjusted: root.baseId !== "" && root.baseId !== "none"
    && (root.masterTouched || root.editGains.some(function (g) { return Number(g) !== 0 }))

  function baseLabel() {
    return root.composeBase === "none" ? Qcy.tr("no effect") : root.curveName(root.composeBase)
  }

  // The overall gain on screen: the user's value once touched, else the one the
  // earbuds report (the base effect's own).
  function shownMaster() {
    return root.masterTouched ? root.editMaster : root.masterGainDb()
  }

  // "+3.5", "-2.0", "0"; the decimal separator comes from the language ("_decimal").
  function fmtDb(v) {
    var n = Number(v) || 0
    if (n === 0) return "0"
    return (n > 0 ? "+" : "") + n.toFixed(1).replace(".", Qcy.decimalSep)
  }

  // Names the user types for a saved effect. Same length rule as the backend.
  function nameValid(n) {
    var t = String(n || "").trim()
    return t.length > 0 && t.length <= 40 && t.charAt(0) !== "-"
  }

  function submitSave(n) {
    if (!nameValid(n)) return
    // What is playing is saved whole: the base's own bands plus the bars. The
    // new effect then becomes the one in use, with nothing adjusted on top.
    Qcy.libraryCreate(String(n).trim(), root.editGainsCsv(), root.shownMaster(), root.composeBase, function (ok) {
      if (!ok) return
      var id = Qcy.libraryOp.id
      eqTimer.stop()
      root.editGains = [0, 0, 0, 0, 0, 0, 0, 0, 0, 0]
      root.editMaster = 0
      root.masterTouched = false
      root.eqNote = ""
      root.baseId = id
      root.closeSave()
    })
  }

  // Closing the name field hands the keyboard back to the panel's key catcher.
  function closeSave() {
    root.saveOpen = false
    nameField.text = ""
    Qt.callLater(function () { keyCatcher.forceActiveFocus() })
  }

  // ---- equalizer ----

  function clampGain(v) {
    var n = Number(v)
    if (isNaN(n)) n = 0
    return Math.max(-root.editorMaxDb, Math.min(root.editorMaxDb, n))
  }

  function editGainsCsv() {
    return root.editGains.map(function (g) {
      return (Math.round(root.clampGain(g) * 100) / 100 + 0).toFixed(2)
    }).join(",")
  }

  // The base effect seen through the ten bars: each band of the effect is a
  // bell (gain / (1 + Q^2 (f/f0 - f0/f)^2), Q stored x100) summed at the ten
  // bar frequencies. An approximation to show and to edit from; the effect
  // itself is still written whole and the bars add on top of it.
  readonly property var baseGains: {
    var out = [0, 0, 0, 0, 0, 0, 0, 0, 0, 0]
    var l = Qcy.library
    var e = null
    for (var k = 0; k < l.length; k++) if (l[k].id === root.composeBase) { e = l[k]; break }
    if (!e || !e.bands) return out
    for (var i = 0; i < 10; i++) {
      var f = root.editorFreqs[i]
      var sum = 0
      for (var j = 0; j < e.bands.length; j++) {
        var b = e.bands[j]
        if (!b.freq || !b.gain) continue
        var q = (b.q > 0 ? b.q : 100) / 100
        var d = f / b.freq - b.freq / f
        sum += b.gain / (1 + q * q * d * d)
      }
      out[i] = Math.round(Math.max(-root.editorMaxDb, Math.min(root.editorMaxDb, sum)) * 2) / 2 + 0
    }
    return out
  }

  // What the bars show: the effect's curve plus what the user moved.
  readonly property var shownGains: root.editGains.map(function (g, i) {
    return Math.max(-root.editorMaxDb, Math.min(root.editorMaxDb, Number(root.baseGains[i]) + Number(g)))
  })

  function shownGainsCsv() {
    return root.shownGains.map(function (g) {
      return (Math.round(g * 100) / 100 + 0).toFixed(2)
    }).join(",")
  }

  // A bar was dragged to the value v on screen: the difference to the effect's
  // own curve is what gets composed on top of it.
  function editSetShown(i, v) {
    var target = Math.round(root.clampGain(v) * 2) / 2
    root.editSetGain(i, target - Number(root.baseGains[i]))
  }

  function editSetGain(i, v) {
    var nv = Math.round(root.clampGain(v) * 2) / 2 + 0
    if (Number(root.editGains[i]) === nv) return
    var g = root.editGains.slice()
    g[i] = nv
    root.editGains = g
  }

  function editSetMaster(v) {
    var n = Number(v)
    if (isNaN(n)) return
    root.editMaster = Math.round(Math.max(root.masterGainMin, Math.min(root.masterGainMax, n)) * 2) / 2 + 0
    root.masterTouched = true
  }

  // Back to the state the app starts in: bars at zero, the effect in use as base.
  function resetEq() {
    eqTimer.stop()
    root.editGains = [0, 0, 0, 0, 0, 0, 0, 0, 0, 0]
    root.editMaster = 0
    root.masterTouched = false
    root.eqNote = ""
    root.baseId = ""
  }

  // A pinned base only means something in Personalizado (255). When a read
  // shows the earbuds on another effect (changed from outside), forget it so
  // the next release composes on what is really playing.
  function syncBase() {
    if (root.baseId !== "" && root.baseId !== "none" && !Qcy.writeBusy && root.activeSlot >= 0 && root.activeSlot !== 255)
      root.baseId = ""
  }

  // One write now: the bars on top of the base effect. Pinning the base keeps
  // it stable when the result happens to look like another effect.
  function applyEq() {
    eqTimer.stop()
    if (!root.canApplyLibrary) return
    // Without a base effect the bars alone are the whole curve: all at zero
    // there is nothing to write.
    var flat = root.editGains.every(function (g) { return Number(g) === 0 })
    if (root.composeBase === "none" && flat) {
      root.eqNote = Qcy.tr("With no base effect and all bars at zero there is nothing to apply. Choose an effect or move a bar.")
      return
    }
    root.eqNote = ""
    root.baseId = root.composeBase
    Qcy.libraryCompose(root.baseId, root.editGainsCsv(), root.masterTouched ? root.editMaster : null)
  }

  // Called when a bar or the overall gain is released: apply after a short
  // pause, the last value wins.
  function scheduleEq() {
    if (root.canApplyLibrary) eqTimer.restart()
  }

  // A release is still pending (tab change, panel close): send it, do not drop it.
  function flushEq() {
    if (eqTimer.running) root.applyEq()
  }

  // "Começar do zero": the bars keep what they show but the effect underneath
  // is dropped, so the ten bars are the whole curve. Nothing is written until
  // a bar is moved; the earbuds keep playing the exact effect until then.
  function detachBase() {
    eqTimer.stop()
    root.editGains = root.shownGains.slice()
    root.baseId = "none"
    root.eqNote = ""
  }

  // "Zerar": bars and overall gain to zero, applied now on the same base.
  function eqZero() {
    root.editGains = [0, 0, 0, 0, 0, 0, 0, 0, 0, 0]
    root.editMaster = 0
    root.masterTouched = false
    root.applyEq()
  }

  // Click on an effect card: apply it now, bars go back to zero on top of it.
  function chooseEffect(id) {
    if (!root.canApplyLibrary) return
    eqTimer.stop()
    root.menuId = ""
    root.pendingDeleteId = ""
    root.editGains = [0, 0, 0, 0, 0, 0, 0, 0, 0, 0]
    root.editMaster = 0
    root.masterTouched = false
    root.eqNote = ""
    root.baseId = id
    Qcy.libraryApply(id)
  }

  function deleteEffect(id) {
    root.menuId = ""
    root.pendingDeleteId = ""
    if (root.baseId === id) root.resetEq()
    Qcy.libraryDelete(id)
  }

  Timer {
    id: eqTimer
    interval: 350
    repeat: false
    onTriggered: root.applyEq()
  }

  onCanApplyLibraryChanged: if (!root.canApplyLibrary) eqTimer.stop()

  function opText() {
    var op = Qcy.libraryOp
    if (op.phase === "idle") return ""
    if (op.kind === "apply") {
      var nm = curveName(op.id)
      if (op.phase === "running") return Qcy.tr("Applying “{name}”…", { name: nm })
      if (op.phase === "ok") return Qcy.tr("“{name}” applied ✓", { name: nm })
      if (op.phase === "unconfirmed")
        return Qcy.tr("Sent, but the earbuds did not confirm.") + (op.message !== "" ? "\n" + op.message : "")
      return Qcy.tr("Could not apply: {message}", { message: op.message })
    }
    if (op.kind === "compose") {
      if (op.phase === "running") return Qcy.tr("Applying…")
      if (op.phase === "ok") return Qcy.tr("Applied ✓")
      if (op.phase === "unconfirmed")
        return Qcy.tr("Sent, but the earbuds did not confirm.") + (op.message !== "" ? "\n" + op.message : "")
      return Qcy.tr("Could not apply: {message}", { message: op.message })
    }
    if (op.kind === "save") {
      if (op.phase === "running") return Qcy.tr("Saving…")
      if (op.phase === "ok") return Qcy.tr("Effect saved ✓")
      return Qcy.tr("Could not save: {message}", { message: op.message })
    }
    if (op.kind === "delete") {
      if (op.phase === "running") return Qcy.tr("Deleting…")
      if (op.phase === "ok") return Qcy.tr("Effect deleted.")
      return Qcy.tr("Could not delete: {message}", { message: op.message })
    }
    return ""
  }

  // The equalizer card reports its own writes (apply, save); the line under
  // the effect grid reports clicks on a card and deletions.
  function eqOpText() {
    if (root.eqNote !== "") return root.eqNote
    var k = Qcy.libraryOp.kind
    return (k === "compose" || k === "save") ? root.opText() : ""
  }

  function gridOpText() {
    var k = Qcy.libraryOp.kind
    return (k === "apply" || k === "delete") ? root.opText() : ""
  }

  // Opening the Sound tab reads the list from disk (offline, no daemon).
  onTabChanged: {
    root.flushEq()
    root.saveOpen = false
    root.menuId = ""
    root.pendingDeleteId = ""
    if (root.tab === 1) {
      Qcy.libraryClearOp()
      Qcy.libraryReload()
    }
  }

  // The tooltip is the fastest read of the earbud state without opening the
    // panel: both earbuds and the case, since that is the one that is easy to
    // forget. Falls back to the model name until the first battery read lands.
    function barTooltip() {
      var name = root.modelName
      if (!Qcy.connected) return Qcy.tr("{name} — disconnected", { name: name })
      if (!batt) return Qcy.tr("{name} — reading battery…", { name: name })
      var parts = [Qcy.tr("L {n}%", { n: batt.left }), Qcy.tr("R {n}%", { n: batt.right })]
      if (batt.casePresent) parts.push(Qcy.tr("case {n}%", { n: batt.case }))
      return name + " — " + parts.join(" · ")
    }

  function heroStatus() {
    if (!Qcy.connected) return Qcy.tr("Disconnected")
    if (isStale) return Qcy.tr("Cached data from {n} s ago (earbuds in use by another app)",
      { n: Math.max(0, Math.round(Number(rep.ageSeconds) || 0)) })
    if (Qcy.loading) return Qcy.tr("Reading…")
    if (!rep) return Qcy.tr("Idle")
    return Qcy.tr("Connected")
  }

  // The model name BlueZ reports, trimmed of the vendor prefix so the header
  // reads "MeloBuds N70" rather than repeating "QCY".
  readonly property string modelName: {
    var raw = Qcy.productTitle
    if (raw === "" && Qcy.device) raw = (Qcy.device.name || Qcy.device.deviceName || "")
    var m = raw.match(/melobuds\s*(.+)/i)
    if (m) return "MeloBuds " + m[1]
    return raw || "QCY Earbuds"
  }

  function currentAnc() {
    if (optimistic["anc"] !== undefined) return optimistic["anc"]
    return noise ? noise.scene : ""
  }

  function toggleState(key) {
    if (optimistic[key] !== undefined) return optimistic[key]
    if (key === "spatial") return !!(rep && rep.spatial && rep.spatial.enabled === true)
    if (key === "wear") return !!(rep && rep.wear && rep.wear.enabled === true)
    return togglesReport[key] === true
  }

  function batteryRows() {
    if (!batt) return []
    var rows = [
      { label: Qcy.tr("Left"), level: batt.left, charging: batt.leftCharging, present: true },
      { label: Qcy.tr("Right"), level: batt.right, charging: batt.rightCharging, present: true }
    ]
    if (batt.casePresent) {
      rows.push({ label: Qcy.tr("Case"), level: batt.case, charging: batt.caseCharging, present: true })
    }
    return rows
  }

  function freqLabel(hz) {
    if (!hz) return ""
    if (hz >= 1000) {
      var k = hz / 1000
      return (k % 1 === 0 ? k : k.toFixed(1)) + "k"
    }
    return String(hz)
  }

  // Repeater models must not change identity when nothing in them changed: `rep`
  // is a fresh object on every status read, so handing the Repeater
  // `rep.eq.bands` directly gave it a new model on every read and rebuilt every
  // band. The cache is refreshed from a change handler rather than from inside
  // a binding — memoising in the binding itself makes the model depend on the
  // cache it writes, and QML drops the binding as a loop.
  property var eqBandModel: []
  property string _eqSig: ""

  function updateEqBands() {
    var b = rep && rep.eq ? rep.eq.bands : null
    var sig = b ? b.map(function (x) { return x.freq + "/" + x.gain }).join(",") : ""
    if (sig === root._eqSig) return
    root._eqSig = sig
    root.eqBandModel = b ? b : []
  }

  // Preset (read-only in this release: the panel shows the active slot, it
  // never writes one). {label, value} objects, because ChoiceRow renders
  // modelData.label and matches on modelData.value. value is the HARDWARE preset
  // slot, so it lines up with rep.eq.preset directly.
  readonly property var eqPresetModel: [
    { label: "Custom (hearing test)", value: 11 },
    { label: "Pop", value: 0 },
    { label: "Rock", value: 1 },
    { label: "Classical", value: 2 },
    { label: "Jazz", value: 3 },
    { label: "Bass Booster", value: 4 }
  ]
  property int _eqPreset: 0
  property string _eqPresetSig: ""

  function updateEqPreset() {
    var p = rep && rep.eq && rep.eq.preset !== null && rep.eq.preset !== undefined ? rep.eq.preset : 0
    var sig = String(p)
    if (sig === root._eqPresetSig) return
    root._eqPresetSig = sig
    root._eqPreset = p
  }

  function eqPreset() {
    return root._eqPreset
  }

  // Master gain, in dB (float, as the daemon reports it), +-12 dB.
  readonly property real masterGainMin: -12
  readonly property real masterGainMax: 12
  property real _masterGainDb: 0
  property string _masterGainSig: ""

  function updateMasterGain() {
    var v = rep && rep.eq ? Number(rep.eq.masterGain || 0) : 0
    if (isNaN(v)) v = 0
    var sig = String(v)
    if (sig === root._masterGainSig) return
    root._masterGainSig = sig
    root._masterGainDb = v
  }

  function masterGainDb() {
    return root._masterGainDb
  }

  function eqBands() {
    return root.eqBandModel
  }

  // Each updater runs on its own: a throw in one must not skip the rest.
  function updateAll() {
    var steps = [updateEqBands, updateEqPreset, updateMasterGain, updateTouchRows, syncBase]
    for (var i = 0; i < steps.length; i++) {
      try { steps[i]() } catch (e) { console.warn("qcy: update step failed:", e) }
    }
  }
  onRepChanged: updateAll()

  // What this unit actually answered, from the daemon's one-off probe. Falls
  // back to the shape of the report when no probe has run, so the panel is
  // never empty just because capabilities are missing.
  readonly property var caps: (rep && rep.capabilities) ? rep.capabilities : null

  // The daemon's detection of the connected model (rep.model). Unverified
  // models get only what their product declares, and no EQ.
  readonly property var unitModel: (rep && rep.model) ? rep.model : null
  readonly property bool unverifiedModel: !!(unitModel && unitModel.verified === false)

  // Probe name -> declared action id / feature flag, for the cross below.
  readonly property var declaredAs: ({
    anc: "anc", game: "gameMode", sleep: "sleepMode", ldac: "ldac",
    multipoint: "multipoint", wind: "windNoise", wear: "wearDetect",
    autoOff: "autoOff", balance: "balance", promptVol: "promptVolume"
  })

  function declared(feature) {
    if (!unverifiedModel || !unitModel.actions) return true
    if (feature === "find") return (unitModel.features || []).indexOf("find") !== -1
    var id = declaredAs[feature]
    if (id === undefined) return false   // spatial, fit, keys: never declared
    return unitModel.actions.indexOf(id) !== -1
  }

  function has(feature) {
    // The fit test never answers a probe; the daemon declares it per model.
    if (feature === "fit") {
      return !!(unitModel && unitModel.features && unitModel.features.indexOf("fit") !== -1)
    }
    if (unverifiedModel && !declared(feature)) return false
    if (caps) return caps[feature] === true
    if (feature === "spatial") return !!(rep && rep.spatial)
    if (feature === "keys") return !!(rep && rep.keys)
    return true
  }

  // ---- fit test ------------------------------------------------------------
  // `fit start --json 35` plays a sound and waits (up to 35 s) for the result.
  // Qcy.set runs it with a callback, whose watchdog (60 s) bounds the run so a
  // long test is neither cut short nor able to hold the setter queue forever.
  property bool fitRunning: false
  // null until a test finishes; then { ok, left, right } (grades 0..3).
  property var fitResult: null

  readonly property var fitGrades: [Qcy.tr("poor"), Qcy.tr("fair"), Qcy.tr("good"), Qcy.tr("sealed")]

  function fitGradeName(n) {
    return n >= 0 && n < fitGrades.length ? fitGrades[n] : "?"
  }

  function fitSummary() {
    var r = fitResult
    if (!r) return ""
    if (!r.ok) return Qcy.tr("No result — fit the earbuds and try again")
    return Qcy.tr("Left: {left} · Right: {right}", { left: fitGradeName(r.left), right: fitGradeName(r.right) })
  }

  // Urgent colour and hint when a side is poor or fair, or there was no result.
  readonly property bool fitNeedsAttention: !!fitResult
    && (!fitResult.ok || fitResult.left < 2 || fitResult.right < 2)

  function startFit() {
    if (fitRunning) return
    fitRunning = true
    fitResult = null
    Qcy.set("Fit test", ["fit", "start", "--json", "35"], function (code, out, err) {
      var res = { ok: false, left: -1, right: -1 }
      if (code === 0) {
        var lines = String(out || "").trim().split("\n")
        try {
          var j = JSON.parse(lines[lines.length - 1])
          if (j && typeof j.left === "number" && typeof j.right === "number") {
            res = { ok: true, left: j.left, right: j.right }
          }
        } catch (e) { /* no result line: reported as no result */ }
      }
      root.fitResult = res
      root.fitRunning = false
    })
  }

  function updatedLabel() {
    var tick = root.nowTick // binding dependency for the "ago" clock
    if (Qcy.error !== "") return Qcy.error
    if (Qcy.loading) return Qcy.tr("Reading the earbuds…")
    if (!Qcy.updatedAt) return Qcy.tr("Open to read the earbuds' state")
    var s = Math.round((Date.now() - Qcy.updatedAt) / 1000)
    if (s < 5) return Qcy.tr("Updated just now")
    if (s < 60) return Qcy.tr("Updated {n} s ago", { n: s })
    return Qcy.tr("Updated {n} min ago", { n: Math.round(s / 60) })
  }

  function setToggle(key, label, on) {
    var next = Object.assign({}, optimistic)
    next[key] = on
    optimistic = next
    Qcy.set(label, [key, on ? "on" : "off"])
  }

  function setAnc(key) {
    var next = Object.assign({}, optimistic)
    next["anc"] = key
    optimistic = next
    Qcy.set("ANC " + key, ["anc", key])
  }

  // ---- ANC intensity ------------------------------------------------------
  // The earbuds accept 0-2 and clamp anything higher to 0. Adaptive and
  // anti-wind ignore the value and always report 0, so the control is hidden
  // for those rather than shown doing nothing.
  readonly property int ancStrength: {
    var v = optimistic["ancLevel"]
    if (v !== undefined) return v
    return noise && noise.value !== undefined && noise.value !== null ? noise.value : 0
  }

  readonly property bool ancStrengthApplies: {
    var s = currentAnc()
    return s === "indoor" || s === "commuting" || s === "noisy" || s === "transparency"
  }

  readonly property var ancStrengthChoices: [
    { label: Qcy.tr("Low"), value: 0 },
    { label: Qcy.tr("Medium"), value: 1 },
    { label: Qcy.tr("High"), value: 2 }
  ]

  function setAncStrength(value) {
    var next = Object.assign({}, optimistic)
    next["ancLevel"] = value
    optimistic = next
    // The scene has to be re-sent with the new strength; the earbuds treat the
    // third ANC parameter as part of the same command.
    Qcy.set("ANC intensity", ["anc", currentAnc(), "--level", String(value)])
  }

  // ---- sliders -------------------------------------------------------------
  // The value shown is the optimistic one while the setter is in flight, so the
  // knob does not snap back to the last read value for the second or two the
  // round-trip takes.

  function sliderValue(key, fallback) {
    if (optimistic[key] !== undefined) return optimistic[key]
    var v = rep && rep[key] !== undefined && rep[key] !== null ? rep[key] : fallback
    // Some report fields are {value, max} objects (promptVolume).
    if (v !== null && typeof v === "object") return v.value !== undefined && v.value !== null ? v.value : fallback
    return v
  }

  function commitSlider(key, label, args, value) {
    var next = Object.assign({}, optimistic)
    next[key] = value
    optimistic = next
    Qcy.set(label, args(value))
  }

  // ---- auto power-off ------------------------------------------------------

  readonly property var autoOffChoices: [
    { label: Qcy.tr("Never"), value: "never" },
    { label: Qcy.tr("{n} min", { n: 15 }), value: "15" },
    { label: Qcy.tr("{n} min", { n: 30 }), value: "30" },
    { label: Qcy.tr("{n} min", { n: 60 }), value: "60" },
    { label: Qcy.tr("{n} min", { n: 90 }), value: "90" }
  ]

  // Touch-control remap table, decoded from the pairs reported for characteristic
  // 000D. Ids follow Quicky's KeyID/FuncID vocabulary; an id outside it is shown
  // as-is rather than guessed at.
  readonly property var funcNames: ({
    0x00: Qcy.tr("Disabled"), 0x01: Qcy.tr("Play/pause"), 0x02: Qcy.tr("Previous"), 0x03: Qcy.tr("Next"),
    0x04: Qcy.tr("Voice assistant"), 0x05: Qcy.tr("Volume +"), 0x06: Qcy.tr("Volume −"),
    0x07: Qcy.tr("Game mode"), 0x08: Qcy.tr("Answer"), 0x09: Qcy.tr("Reject"),
    0x0a: Qcy.tr("Hold call"), 0x0b: Qcy.tr("Redial")
  })

  readonly property var keyNames: ({
    0x01: Qcy.tr("Left tap"), 0x02: Qcy.tr("Right tap"), 0x03: Qcy.tr("Left double"), 0x04: Qcy.tr("Right double"),
    0x05: Qcy.tr("Left triple"), 0x06: Qcy.tr("Right triple"), 0x07: Qcy.tr("Left quadruple"), 0x08: Qcy.tr("Right quadruple"),
    0x09: Qcy.tr("Left hold"), 0x0a: Qcy.tr("Right hold"),
    0x15: Qcy.tr("Left tap (call)"), 0x16: Qcy.tr("Right tap (call)"), 0x17: Qcy.tr("Left double (call)"),
    0x18: Qcy.tr("Right double (call)"), 0x19: Qcy.tr("Left triple (call)"), 0x1a: Qcy.tr("Right triple (call)"),
    0x1b: Qcy.tr("Left quadruple (call)"), 0x1c: Qcy.tr("Right quadruple (call)"),
    0x1d: Qcy.tr("Left hold (call)"), 0x1e: Qcy.tr("Right hold (call)")
  })

  readonly property var gestureNames: [Qcy.tr("Tap"), Qcy.tr("Double"), Qcy.tr("Triple"), Qcy.tr("Quadruple"), Qcy.tr("Hold")]

  // Same identity-stable caching as the EQ bands: `rep` is replaced on every
  // status read, so the decoded rows are rebuilt only when the raw pair list
  // actually differs, and never from inside a binding.
  property var touchRowModel: []
  property string _touchSig: ""

  function updateTouchRows() {
    var k = rep ? rep.keys : null
    var pairs = k && k.pairs ? k.pairs : []
    var sig = pairs.join(",")
    if (sig === root._touchSig) return
    root._touchSig = sig
    // One row per gesture (and per call/normal context), Left | Right. A key
    // id outside the known ranges keeps its own row, named as before.
    var rows = []
    var byKey = {}
    for (var i = 0; i < pairs.length; i++) {
      var bits = String(pairs[i]).split(":")
      var kid = parseInt(bits[0], 16)
      var fid = parseInt(bits[1], 16)
      var action = root.funcNames[fid] || ("0x" + bits[1])
      var call = kid >= 0x15
      var n = kid - (call ? 0x15 : 0x01)
      if (!(n >= 0 && n < 10)) {
        rows.push({ gesture: root.keyNames[kid] || ("0x" + bits[0]), left: action, right: "" })
        continue
      }
      var gi = Math.floor(n / 2)
      var key = (call ? "c" : "n") + gi
      if (!byKey[key]) {
        byKey[key] = { gesture: call ? Qcy.tr("{gesture} (call)", { gesture: root.gestureNames[gi] }) : root.gestureNames[gi],
                       left: "—", right: "—", order: (call ? 10 : 0) + gi }
        rows.push(byKey[key])
      }
      byKey[key][n % 2 === 0 ? "left" : "right"] = action
    }
    rows.sort(function (a, b) { return (a.order === undefined ? 99 : a.order) - (b.order === undefined ? 99 : b.order) })
    root.touchRowModel = rows
  }

  function touchRows() {
    return root.touchRowModel
  }

  // Prompt volume accepts only these four levels (the reported max is not one).
  readonly property var promptVolChoices: [
    { label: Qcy.tr("Quieter"), value: 4 },
    { label: Qcy.tr("Medium volume"), value: 6 },
    { label: Qcy.tr("Louder"), value: 8 },
    { label: Qcy.tr("Maximum"), value: 10 }
  ]

  // Wear-sensor sensitivity; only meaningful while wear detection is on.
  readonly property var wearSensChoices: [
    { label: Qcy.tr("Medium"), value: 1 },
    { label: Qcy.tr("Normal"), value: 2 },
    { label: Qcy.tr("High"), value: 3 }
  ]
  readonly property bool wearSensVisible: !!(rep && rep.wear && rep.wear.enabled === true)
    && rep.wearSensitivity !== undefined && rep.wearSensitivity !== null

  function currentAutoOff() {
    if (optimistic["autoOff"] !== undefined) return String(optimistic["autoOff"])
    var a = rep ? rep.autoOff : null
    if (!a || a.minutes === null || a.minutes === undefined) return "never"
    return String(a.minutes)
  }

  function setAutoOff(value) {
    var next = Object.assign({}, optimistic)
    next["autoOff"] = value
    optimistic = next
    Qcy.set("Auto power-off", ["auto-off", value])
  }

  Component.onCompleted: {
    Qcy.settings = settings
    root.updateAll()
  }
  onSettingsChanged: Qcy.settings = settings

  // Opening the panel is the display trigger: read battery lightly now and keep
  // it fresh while open; full status re-runs only occasionally.
  onOpenedChanged: {
    if (!opened) root.flushEq()
    if (opened) {
      if (Qcy.connected) Qcy.pollBattery()
      batteryTimer.running = opened
      statusTimer.running = opened
      if (root.tab === 1) Qcy.libraryReload()
    }
  }

  Timer {
    id: batteryTimer
    interval: 20000
    repeat: true
    running: false
    onTriggered: {
      if (root.opened && Qcy.connected) Qcy.pollBattery()
    }
  }

  Timer {
    id: statusTimer
    interval: 60000
    repeat: true
    running: false
    onTriggered: {
      if (root.opened && Qcy.connected) Qcy.refresh()
    }
  }

  // A fresh report supersedes any optimistic toggle state.
  Connections {
    target: Qcy
    function onUpdatedAtChanged() {
      if (Qcy.busyAction === "") root.optimistic = ({})
    }
    // The base pinned by a card click is only real if that apply did not fail:
    // otherwise the next bar would compose on an effect that is not active.
    function onLibraryOpChanged() {
      var op = Qcy.libraryOp
      if (op.kind === "apply" && op.phase === "error" && op.id !== "" && op.id === root.baseId)
        root.baseId = ""
    }
    // Decoded touch rows are cached by their raw pairs: rebuild them in the
    // new language once the translated name tables have re-evaluated.
    function onTrRevChanged() {
      Qt.callLater(function () {
        root._touchSig = ""
        root.updateTouchRows()
      })
    }
    function onSetFailed() {
      // Drop the optimistic master gain; the refresh Qcy starts re-reads the
      // real value, which the cleared signature lets through.
      root._masterGainSig = ""
      Qcy.refresh()
    }
    function onConnectedChanged() {
      root.optimistic = ({})
      if (!Qcy.connected) {
        root.resetEq()
        root.close()
      }
    }
  }

  Timer {
    interval: 1000
    repeat: true
    running: root.opened
    onTriggered: root.nowTick++
  }

  implicitWidth: button.implicitWidth
  implicitHeight: button.implicitHeight

  IpcHandler {
    target: "io.github.bbztp.qcy"

    function open() { root.open() }
    function close() { root.close() }
    function show() { root.open() }
    function hide() { root.close() }
    function toggle() { root.toggle() }
    function refresh() { Qcy.refresh() }
    function battery() { Qcy.pollBattery() }
  }

  // The QCY wordmark, tinted with the bar foreground. Wider than an icon slot,
  // so a plain WidgetButton instead of the square BarIconButton holds it.
  readonly property Component _logoIcon: Component {
    Item {
      id: logoHost
      width: root.vertical ? 18 : 44
      height: root.vertical ? 44 : 18

      Image {
        id: logoImg
        anchors.fill: parent
        source: "assets/qcy-logo.svg"
        sourceSize: root.vertical ? Qt.size(18, 44) : Qt.size(44, 18)
        fillMode: Image.PreserveAspectFit
        rotation: root.vertical ? -90 : 0
      }
      ColorOverlay {
        anchors.fill: parent
        source: logoImg
        color: root.foreground
      }
    }
  }

  WidgetButton {
    id: button
    anchors.fill: parent
    bar: root.bar
    labelVisible: false
    hasVisualContent: true
    foreground: root.foreground
    fixedWidth: root.vertical ? -1 : 44
    fixedHeight: root.vertical ? 44 : -1
    tooltipText: root.barTooltip()
    onPressed: function (b) {
      if (b === Qt.RightButton) Qcy.refresh()
      else root.toggle()
    }
    Loader {
      anchors.centerIn: parent
      width: root.vertical ? 18 : 44
      height: root.vertical ? 44 : 18
      sourceComponent: root._logoIcon
    }
  }

  KeyboardPanel {
    id: panel
    anchorItem: button
    owner: root
    bar: root.bar
    open: root.opened
    focusTarget: keyCatcher
    contentWidth: panel.fittedContentWidth(Style.space(420))
    contentHeight: panel.fittedContentHeight(column.implicitHeight)

    PanelKeyCatcher {
      id: keyCatcher
      anchors.fill: parent
      // Typing a curve name must not trigger the panel's single-key shortcuts.
      blocked: nameField.activeFocus
      onCloseRequested: root.close()
      onTextKey: function (t) {
        if (t === "r" || t === "R") Qcy.refresh()
        else if (t === "1") root.tab = 0
        else if (t === "2") root.tab = 1
        else if (t === "3") root.tab = 2
      }

      // The card clamps its height to the space between the bar and the
      // bottom of the screen, but the content used to be a plain Column pinned
      // to that clamped box: on a short screen the tail spilled past the card
      // with no background under it. A Flickable keeps every section
      // reachable, and the thin bar on the right says there is more below.
      Flickable {
        id: scroller
        anchors.fill: parent
        contentWidth: width
        contentHeight: column.implicitHeight
        clip: true
        boundsBehavior: Flickable.StopAtBounds

        readonly property bool overflows: contentHeight > height

        // Reserved whether or not it is needed: making the column's width
        // depend on `overflows` would close a loop (contentHeight comes from
        // the column's own implicitHeight, which depends on its width).
        readonly property real gutter: Style.space(10)

        Rectangle {
          width: Style.space(4)
          radius: width / 2
          color: Qt.darker(root.foreground, 1.6)
          visible: scroller.overflows
          x: scroller.width - width
          // Divided by contentHeight, which is 0 until the first layout pass —
          // guard it so the thumb is never given a NaN or infinite size.
          height: scroller.contentHeight > 0
            ? Math.max(Style.space(24),
              scroller.height * scroller.height / scroller.contentHeight)
            : scroller.height
          y: scroller.contentHeight > 0
            ? scroller.contentY * scroller.height / scroller.contentHeight
            : 0
          z: 1
        }

        Column {
          id: column
          width: scroller.width - scroller.gutter
          spacing: Style.space(14)

          // ---------- Tabs ----------
          // Status (device + noise mode), Sound (live curve + library) and
          // Settings (everything else). Short tabs keep every section reachable.
          Row {
            id: tabBar
            width: parent.width
            spacing: Style.space(6)

            Repeater {
              model: [
                { label: Qcy.tr("Status"), index: 0 },
                { label: Qcy.tr("Sound"), index: 1 },
                { label: Qcy.tr("Settings"), index: 2 }
              ]
              TabButton {
                required property var modelData
                label: modelData.label
                index: modelData.index
                tabWidth: (tabBar.width - Style.space(6) * 2) / 3
              }
            }
          }

          // =====================================================================
          // STATUS
          // =====================================================================
          Column {
            visible: root.tab === 0
            width: parent.width
            spacing: Style.space(14)

            // Three charge rings: left, right, case.
            Row {
              id: ringRow
              width: parent.width
              spacing: Style.space(8)

              BatteryRing {
                width: (ringRow.width - Style.space(8) * 2) / 3
                label: Qcy.tr("L")
                valid: Qcy.connected && !!root.batt
                level: root.batt ? root.batt.left : 0
                charging: root.batt ? root.batt.leftCharging : false
              }
              BatteryRing {
                width: (ringRow.width - Style.space(8) * 2) / 3
                label: Qcy.tr("R")
                valid: Qcy.connected && !!root.batt
                level: root.batt ? root.batt.right : 0
                charging: root.batt ? root.batt.rightCharging : false
              }
              BatteryRing {
                width: (ringRow.width - Style.space(8) * 2) / 3
                label: Qcy.tr("Case")
                valid: Qcy.connected && !!(root.batt && root.batt.casePresent)
                level: root.batt ? root.batt.case : 0
                charging: root.batt ? root.batt.caseCharging : false
              }
            }

            // Name (level one) and connection state (level two).
            Column {
              width: parent.width
              spacing: Style.space(4)

              Text {
                width: parent.width
                text: root.modelName
                color: root.foreground
                font.family: root.fontFamily
                font.pixelSize: Style.font.title
                font.bold: true
                elide: Text.ElideRight
                horizontalAlignment: Text.AlignHCenter
              }

              Item {
                width: parent.width
                implicitHeight: stateRow.implicitHeight

                Row {
                  id: stateRow
                  anchors.horizontalCenter: parent.horizontalCenter
                  spacing: Style.space(6)

                  Rectangle {
                    width: Style.space(8)
                    height: width
                    radius: width / 2
                    anchors.verticalCenter: stateText.verticalCenter
                    color: root.isStale ? Color.urgent : root.accent
                  }
                  Text {
                    id: stateText
                    text: root.heroStatus()
                    color: Qt.darker(root.foreground, 1.3)
                    font.family: root.fontFamily
                    font.pixelSize: Style.font.bodySmall
                  }
                }
              }
            }

            // Noise mode: three big buttons, only when the unit answers ANC.
            Column {
              width: parent.width
              spacing: Style.space(8)
              visible: root.has("anc")

              PanelSectionHeader {
                text: Qcy.tr("Noise mode").toUpperCase()
                foreground: root.foreground
                fontFamily: root.fontFamily
              }

              Row {
                id: noiseRow
                width: parent.width
                spacing: Style.space(8)

                Repeater {
                  model: root.noiseButtons
                  ModeButton {
                    required property var modelData
                    label: modelData.label
                    glyph: String.fromCodePoint(modelData.glyph)
                    active: root.ancGroup() === modelData.key
                    btnWidth: (noiseRow.width - Style.space(8) * 2) / 3
                    onClicked: {
                      if (root.ancGroup() !== modelData.key) root.setAnc(modelData.key)
                    }
                  }
                }
              }
            }
          }

          // =====================================================================
          // SOM
          // =====================================================================
          Column {
            visible: root.tab === 1
            width: parent.width
            spacing: Style.space(14)

            // ---- In use ----
            Column {
              width: parent.width
              spacing: Style.space(4)

              PanelSectionHeader {
                text: Qcy.tr("In use").toUpperCase()
                foreground: root.foreground
                fontFamily: root.fontFamily
              }

              Text {
                width: parent.width
                text: root.effectName()
                color: root.foreground
                font.family: root.fontFamily
                font.pixelSize: Style.font.title
                font.bold: true
                elide: Text.ElideRight
              }

              Text {
                width: parent.width
                visible: !root.rep
                text: Qcy.loading ? Qcy.tr("Reading the earbuds…") : Qcy.tr("Connect the earbuds to choose an effect.")
                color: Qt.darker(root.foreground, 1.5)
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
                wrapMode: Text.WordWrap
              }
            }

            Text {
              visible: root.unverifiedModel
              width: parent.width
              text: Qcy.tr("Equalizer is only available on the MeloBuds N70.")
              color: Qt.darker(root.foreground, 1.5)
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
              wrapMode: Text.WordWrap
            }

            // ---- Effects: click one to apply it ----
            Column {
              visible: !root.unverifiedModel
              width: parent.width
              spacing: Style.space(8)

              PanelSectionHeader {
                text: Qcy.tr("Effects").toUpperCase()
                foreground: root.foreground
                fontFamily: root.fontFamily
              }

              Text {
                width: parent.width
                visible: Qcy.libraryError !== ""
                text: Qcy.libraryError
                color: Color.urgent
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
                wrapMode: Text.WordWrap
              }

              Text {
                width: parent.width
                visible: Qcy.libraryError === "" && Qcy.library.length === 0
                text: Qcy.libraryLoading || !Qcy.libraryLoaded
                  ? Qcy.tr("Loading effects…")
                  : Qcy.tr("No effect found. Tap Refresh.")
                color: Qt.darker(root.foreground, 1.5)
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
                wrapMode: Text.WordWrap
              }

              Grid {
                id: fxGrid
                width: parent.width
                columns: 3
                spacing: Style.space(8)

                Repeater {
                  model: Qcy.library
                  CurveCard {
                    required property var modelData
                    visible: !root.hiddenEntry(modelData)
                    entry: modelData
                    cardWidth: (fxGrid.width - fxGrid.spacing * 2) / 3
                    canApply: root.canApplyLibrary
                    live: root.activeEntryId === modelData.id
                    adjusted: root.adjusted
                    phase: Qcy.libraryOp.kind === "apply" && Qcy.libraryOp.id === modelData.id
                      ? Qcy.libraryOp.phase : "idle"
                    menuOpen: root.menuId === modelData.id
                    confirmingDelete: root.pendingDeleteId === modelData.id
                    onApplyRequested: root.chooseEffect(modelData.id)
                    onMenuToggled: {
                      root.pendingDeleteId = ""
                      root.menuId = root.menuId === modelData.id ? "" : modelData.id
                    }
                    onDeleteAsked: root.pendingDeleteId = modelData.id
                    onDeleteCancelled: root.pendingDeleteId = ""
                    onDeleteConfirmed: root.deleteEffect(modelData.id)
                  }
                }
              }

              // Result of the last click on a card (or deletion), one line.
              Text {
                width: parent.width
                visible: root.gridOpText() !== ""
                text: root.gridOpText()
                color: Qcy.libraryOp.phase === "error" ? Color.urgent : root.foreground
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
                wrapMode: Text.WrapAnywhere
              }
            }

            // ---- Equalizer: applies when a bar is released ----
            Column {
              visible: !root.unverifiedModel
              width: parent.width
              spacing: Style.space(8)

              PanelSectionHeader {
                text: Qcy.tr("Equalizer").toUpperCase()
                foreground: root.foreground
                fontFamily: root.fontFamily
              }

              Card {
                width: parent.width

                BarEditor {
                  width: parent.width
                  gains: root.shownGains
                  editable: root.canApplyLibrary
                  onEdited: function (i, v) { root.editSetShown(i, v) }
                  onCommitted: root.scheduleEq()
                }

                Text {
                  width: parent.width
                  text: Qcy.tr("The bars show the chosen effect. Drag (±12 dB) and release: the sound changes right away. Double-click to zero a bar. \"Save as…\" keeps what you see.")
                  color: Qt.darker(root.foreground, 1.5)
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.caption
                  wrapMode: Text.WordWrap
                }

                SliderRow {
                  width: parent.width
                  enabled: root.canApplyLibrary
                  opacity: root.canApplyLibrary ? 1.0 : 0.5
                  label: Qcy.tr("Overall gain")
                  minimum: root.masterGainMin
                  maximum: root.masterGainMax
                  step: 0.5
                  integer: false
                  value: root.shownMaster()
                  suffix: " dB"
                  onMoved: function (v) { root.editSetMaster(v) }
                  onReleased: function (v) {
                    root.editSetMaster(v)
                    root.scheduleEq()
                  }
                }

                Text {
                  width: parent.width
                  text: Qcy.tr("base: {name}", { name: root.baseLabel() })
                  color: Qt.darker(root.foreground, 1.5)
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.caption
                  elide: Text.ElideRight
                }

                Text {
                  width: parent.width
                  visible: root.eqOpText() !== ""
                  text: root.eqOpText()
                  color: (root.eqNote !== "" || Qcy.libraryOp.phase === "error") ? Color.urgent : root.foreground
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.bodySmall
                  wrapMode: Text.WrapAnywhere
                }

                Row {
                  spacing: Style.space(8)

                  Chip {
                    opacity: root.canApplyLibrary ? 1.0 : 0.5
                    label: root.composeBase === "none" ? Qcy.tr("Zero") : Qcy.tr("Restore")
                    onClicked: root.eqZero()
                  }
                  Chip {
                    visible: root.composeBase !== "none"
                    opacity: root.canApplyLibrary ? 1.0 : 0.5
                    label: Qcy.tr("Start from scratch")
                    onClicked: root.detachBase()
                  }
                  Chip {
                    label: Qcy.tr("Save as…")
                    active: root.saveOpen
                    onClicked: {
                      if (root.saveOpen) { root.closeSave(); return }
                      root.saveOpen = true
                      nameField.forceActiveFocus()
                      // The name field sits at the bottom, often below the
                      // visible part of the panel.
                      Qt.callLater(function () {
                        scroller.contentY = Math.max(0, scroller.contentHeight - scroller.height)
                      })
                    }
                  }
                }

                Column {
                  width: parent.width
                  spacing: Style.space(8)
                  visible: root.saveOpen

                  TextField {
                    id: nameField
                    width: parent.width
                    placeholderText: Qcy.tr("Effect name")
                    maximumLength: 40
                    foreground: root.foreground
                    accent: root.accent
                    onAccepted: root.submitSave(text)
                    Keys.onEscapePressed: root.closeSave()
                  }

                  Row {
                    spacing: Style.space(8)
                    Chip {
                      label: Qcy.tr("Save")
                      active: root.nameValid(nameField.text)
                      onClicked: root.submitSave(nameField.text)
                    }
                    Chip {
                      label: Qcy.tr("Cancel")
                      onClicked: root.closeSave()
                    }
                  }
                }
              }
            }
          }

          // =====================================================================
          // AJUSTES
          // =====================================================================
          Column {
            visible: root.tab === 2
            width: parent.width
            spacing: Style.space(14)

            // ---- Advanced noise scenes ----
            Column {
              width: parent.width
              spacing: Style.space(8)
              visible: root.has("anc")

              PanelSectionHeader {
                text: Qcy.tr("Cancellation scenes").toUpperCase()
                foreground: root.foreground
                fontFamily: root.fontFamily
              }

              Flow {
                width: parent.width
                spacing: Style.space(8)

                Repeater {
                  model: root.ancModes
                  Chip {
                    required property var modelData
                    label: modelData.label
                    active: root.currentAnc() === modelData.key
                    onClicked: root.setAnc(modelData.key)
                  }
                }
              }

              // Adaptive and anti-wind read back 0 whatever is written, so the
              // dial is hidden for them rather than shown doing nothing.
              ChoiceRow {
                width: parent.width
                visible: root.ancStrengthApplies
                label: Qcy.tr("Intensity")
                model: root.ancStrengthChoices
                current: String(root.ancStrength)
                onPicked: function (v) { root.setAncStrength(Number(v)) }
              }
            }

            // ---- Functions: one list, one row each ----
            Card {
              width: parent.width

              Repeater {
                model: root.toggleDefs
                ToggleRow {
                  required property var modelData
                  width: parent.width
                  label: modelData.label
                  hint: modelData.hint
                  checked: root.toggleState(modelData.key)
                  onToggled: root.setToggle(modelData.key, modelData.label, !root.toggleState(modelData.key))
                }
              }

              ToggleRow {
                width: parent.width
                visible: root.has("spatial")
                label: Qcy.tr("Spatial audio")
                hint: ""
                checked: root.toggleState("spatial")
                onToggled: root.setToggle("spatial", "Spatial audio", !root.toggleState("spatial"))
              }

              ChoiceRow {
                width: parent.width
                visible: root.wearSensVisible
                label: Qcy.tr("Sensor sensitivity")
                model: root.wearSensChoices
                current: String(root.sliderValue("wearSensitivity", 0))
                onPicked: function (v) { root.commitSlider("wearSensitivity", "Sensor sensitivity", function (n) { return ["wear-sensitivity", String(n)] }, Number(v)) }
              }
            }

            // ---- Audio ----
            Card {
              width: parent.width

              SliderRow {
                width: parent.width
                label: Qcy.tr("Balance")
                minimum: 0
                maximum: 100
                step: 1
                suffix: "%"
                value: root.sliderValue("balance", 50)
                onReleased: function (v) { root.commitSlider("balance", "Balance", function (n) { return ["balance", String(Math.round(n))] }, Math.round(v)) }
              }

              ChoiceRow {
                width: parent.width
                visible: root.has("promptVol")
                label: Qcy.tr("Prompt volume")
                model: root.promptVolChoices
                current: String(root.sliderValue("promptVolume", -1))
                onPicked: function (v) { root.commitSlider("promptVolume", "Prompt volume", function (n) { return ["prompt-vol", String(n)] }, Number(v)) }
              }

              ChoiceRow {
                width: parent.width
                label: Qcy.tr("Auto power-off")
                model: root.autoOffChoices
                current: root.currentAutoOff()
                onPicked: function (v) { root.setAutoOff(v) }
              }
            }

            // ---- Fit ----
            Card {
              width: parent.width
              visible: root.has("fit")

              Item {
                width: parent.width
                implicitHeight: fitButton.implicitHeight

                Text {
                  text: Qcy.tr("Earbud fit test")
                  color: root.foreground
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.body
                  anchors.left: parent.left
                  anchors.verticalCenter: parent.verticalCenter
                }

                Chip {
                  id: fitButton
                  anchors.right: parent.right
                  anchors.verticalCenter: parent.verticalCenter
                  label: root.fitRunning ? Qcy.tr("Playing…") : Qcy.tr("Start")
                  active: root.fitRunning
                  enabled: !root.fitRunning
                  onClicked: {
                    if (root.fitRunning || Qcy.settling) return
                    root.startFit()
                  }
                }
              }

              Text {
                width: parent.width
                visible: root.fitRunning || root.fitResult !== null
                wrapMode: Text.WordWrap
                text: root.fitRunning ? Qcy.tr("Playing… listen to the earbuds")
                  : (root.fitNeedsAttention && root.fitResult.ok
                    ? Qcy.tr("{summary}. Adjust the tips and repeat", { summary: root.fitSummary() })
                    : root.fitSummary())
                color: root.fitNeedsAttention ? Color.urgent : Qt.darker(root.foreground, 1.3)
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
              }
            }

            // ---- Touch controls (read-only) ----
            Column {
              visible: root.touchRows().length > 0
              width: parent.width
              spacing: Style.space(8)

              PanelSectionHeader {
                text: Qcy.tr("Touch controls").toUpperCase()
                foreground: root.foreground
                fontFamily: root.fontFamily
              }

              TouchCell {
                width: parent.width
                header: true
                gesture: ""
                leftText: Qcy.tr("Left").toUpperCase()
                rightText: Qcy.tr("Right").toUpperCase()
              }

              Repeater {
                model: root.touchRows()
                TouchCell {
                  required property var modelData
                  width: parent.width
                  gesture: modelData.gesture
                  leftText: modelData.left
                  rightText: modelData.right
                }
              }
            }

            // ---- Device ----
            Column {
              width: parent.width
              spacing: Style.space(8)

              PanelSectionHeader {
                text: Qcy.tr("Device").toUpperCase()
                foreground: root.foreground
                fontFamily: root.fontFamily
              }

              StatRow {
                width: parent.width
                label: Qcy.tr("Model")
                value: root.modelName
              }
              StatRow {
                width: parent.width
                label: "Firmware"
                value: root.rep && root.rep.firmware && root.rep.firmware.left
                  ? root.rep.firmware.left : "—"
              }
              Text {
                visible: Qcy.modelUnverified
                text: Qcy.tr("Unverified model — only the common controls were tested on another device.")
                color: Qt.darker(root.foreground, 1.5)
                font.family: root.fontFamily
                font.pixelSize: Style.font.caption
                wrapMode: Text.WordWrap
                width: parent.width
              }
            }

            // ---- Language: Automatic follows the system locale ----
            Column {
              width: parent.width
              spacing: Style.space(8)

              PanelSectionHeader {
                text: Qcy.tr("Language").toUpperCase()
                foreground: root.foreground
                fontFamily: root.fontFamily
              }

              ChoiceRow {
                width: parent.width
                label: Qcy.tr("Language")
                model: [{ label: Qcy.tr("Automatic"), value: "auto" }].concat(
                  Qcy.languages.map(function (l) { return { label: l.name, value: l.code } }))
                current: Qcy.langChoice
                onPicked: function (v) { Qcy.setLanguage(v) }
              }

              Text {
                width: parent.width
                text: Qcy.tr("To add a language: qcyctl i18n template (see the README).")
                color: Qt.darker(root.foreground, 1.5)
                font.family: root.fontFamily
                font.pixelSize: Style.font.caption
                wrapMode: Text.WordWrap
              }
            }
          }

          // ---------- Footer (all tabs) ----------
          Row {
            spacing: Style.space(8)

            Chip {
              label: Qcy.loading ? Qcy.tr("Reading…") : Qcy.tr("Refresh")
              active: Qcy.loading
              onClicked: if (!Qcy.loading) Qcy.refresh()
            }
          }

          Text {
            text: root.updatedLabel()
            color: Qcy.error !== "" ? Color.urgent : Qt.darker(root.foreground, 1.5)
            font.family: root.fontFamily
            font.pixelSize: Style.font.caption
            wrapMode: Text.WordWrap
            width: parent.width
          }

          // Partial-read warnings from qcyctl (a query that timed out).
          Text {
            visible: !!(root.rep && root.rep.errors && root.rep.errors.length > 0)
            text: root.rep && root.rep.errors ? root.rep.errors.join(" · ") : ""
            color: Qt.darker(root.foreground, 1.5)
            font.family: root.fontFamily
            font.pixelSize: Style.font.caption
            wrapMode: Text.WordWrap
            width: parent.width
          }
        }
      }
    }
  }

  // A selectable pill.
  component Chip: BorderSurface {
    id: chip
    required property string label
    property bool active: false
    signal clicked()

    implicitWidth: chipLabel.implicitWidth + Style.space(20)
    implicitHeight: chipLabel.implicitHeight + Style.space(12)
    radius: Style.cornerRadius
    color: chip.active
      ? Style.selectedFillFor(root.foreground, root.accent)
      : (chipMouse.containsMouse ? Style.hoverFillFor(root.foreground, root.accent) : "transparent")
    borderSpec: Border.controlSpec(chip.active ? "selected" : "normal", root.foreground, root.accent)
    Behavior on color { ColorAnimation { duration: 120 } }

    Text {
      id: chipLabel
      anchors.centerIn: parent
      text: chip.label
      color: root.foreground
      font.family: root.fontFamily
      font.pixelSize: Style.font.bodySmall
      font.bold: chip.active
    }

    MouseArea {
      id: chipMouse
      anchors.fill: parent
      hoverEnabled: true
      cursorShape: Qt.PointingHandCursor
      onClicked: chip.clicked()
    }
  }

  // A label on the left, a value on the right. The label is capped at half the
  // row and both sides elide, so a long value can never slide under the label.
  component StatRow: Item {
    id: stat
    required property string label
    required property string value

    implicitHeight: Math.max(statLabel.implicitHeight, statValue.implicitHeight)

    Text {
      id: statLabel
      text: stat.label
      color: Qt.darker(root.foreground, 1.3)
      font.family: root.fontFamily
      font.pixelSize: Style.font.body
      anchors.left: parent.left
      anchors.right: statValue.left
      anchors.rightMargin: Style.space(10)
      anchors.verticalCenter: parent.verticalCenter
      elide: Text.ElideRight
    }
    Text {
      id: statValue
      text: stat.value
      color: root.foreground
      font.family: root.fontFamily
      font.pixelSize: Style.font.body
      width: Math.min(implicitWidth, Math.max(0, parent.width / 2))
      anchors.right: parent.right
      anchors.verticalCenter: parent.verticalCenter
      elide: Text.ElideRight
      horizontalAlignment: Text.AlignRight
    }
  }

  // One circular charge indicator: a faint track, an arc for the level, the
  // percentage in the middle and a label underneath. No reading (offline, not
  // reported yet) leaves the track empty with a dash.
  component BatteryRing: Item {
    id: ring
    required property string label
    property int level: 0
    property bool valid: false
    property bool charging: false

    readonly property real diameter: Style.space(64)
    readonly property real stroke: Style.space(5)
    readonly property real shown: ring.valid ? Math.max(0, Math.min(100, ring.level)) : 0
    readonly property color arcColor: ring.valid && ring.level <= 20 ? Color.urgent : root.accent
    readonly property color trackColor: Qt.rgba(
      root.foreground.r, root.foreground.g, root.foreground.b, 0.18)

    implicitWidth: ring.diameter
    implicitHeight: ringDisc.height + Style.space(6) + ringLabel.implicitHeight

    Item {
      id: ringDisc
      width: ring.diameter
      height: ring.diameter
      anchors.horizontalCenter: parent.horizontalCenter

      // Animated copy of the level; the canvas repaints as it moves.
      property real progress: ring.shown
      Behavior on progress { NumberAnimation { duration: 150; easing.type: Easing.OutCubic } }

      Canvas {
        id: ringCanvas
        anchors.fill: parent
        antialiasing: true
        onPaint: {
          var ctx = getContext("2d")
          ctx.reset()
          var c = width / 2
          var r = c - ring.stroke / 2
          ctx.lineWidth = ring.stroke
          ctx.lineCap = "round"
          ctx.strokeStyle = ring.trackColor
          ctx.beginPath()
          ctx.arc(c, c, r, 0, Math.PI * 2)
          ctx.stroke()
          if (ringDisc.progress > 0.5) {
            var start = -Math.PI / 2
            ctx.strokeStyle = ring.arcColor
            ctx.beginPath()
            ctx.arc(c, c, r, start, start + Math.PI * 2 * ringDisc.progress / 100)
            ctx.stroke()
          }
        }
      }
      Connections {
        target: ringDisc
        function onProgressChanged() { ringCanvas.requestPaint() }
      }
      Connections {
        target: ring
        function onArcColorChanged() { ringCanvas.requestPaint() }
        function onTrackColorChanged() { ringCanvas.requestPaint() }
      }

      Column {
        anchors.centerIn: parent
        Text {
          anchors.horizontalCenter: parent.horizontalCenter
          text: ring.valid ? ring.level + "%" : "—"
          color: root.foreground
          font.family: root.fontFamily
          font.pixelSize: Style.font.body
          font.bold: true
        }
        Text {
          anchors.horizontalCenter: parent.horizontalCenter
          text: "󰂄"
          color: root.accent
          font.family: root.fontFamily
          font.pixelSize: Style.font.bodySmall
          visible: ring.valid && ring.charging
        }
      }
    }

    Text {
      id: ringLabel
      anchors.top: ringDisc.bottom
      anchors.topMargin: Style.space(6)
      anchors.horizontalCenter: parent.horizontalCenter
      text: ring.label
      color: Qt.darker(root.foreground, 1.3)
      font.family: root.fontFamily
      font.pixelSize: Style.font.bodySmall
    }
  }

  // The EQ response as a signed curve with a zero line, the axis labels and the
  // per-band dots. Read-only — see the note above, writing a preset overwrites
  // the slot in place.
  component EqCurve: Item {
    id: curve
    required property var bands
    // Mini version for library cards: shorter plot, tiny frequency labels
    // carved out of the plot's own height so the card does not grow.
    property bool compact: false

    readonly property real maxGain: 12
    // The plot stops short of the bottom so the band labels have room INSIDE
    // this item. They used to be anchored to the delegate's bottom edge, which
    // put them ~2px past `height` and on top of whatever followed.
    readonly property real plotHeight: Style.spacing.controlHeight * (curve.compact ? 1.1 : 1.6)
    readonly property real compactLabelPx: Style.font.caption * (curve.bands.length > 10 ? 0.5 : 0.6)
    readonly property int labelHeight: curve.compact ? 0 : Math.ceil(Style.font.caption * 0.85) + Style.space(2)
    // Where the bars live: all of the plot, minus the label strip in compact mode.
    readonly property real drawHeight: curve.compact ? curve.plotHeight - Math.ceil(curve.compactLabelPx) - 1 : curve.plotHeight
    implicitHeight: plotHeight + labelHeight

    Rectangle {
      id: zeroLine
      anchors.left: parent.left
      anchors.right: parent.right
      y: curve.drawHeight / 2
      height: 1
      color: Qt.darker(root.foreground, 1.9)
    }

    Repeater {
      model: curve.bands
      Item {
        id: band
        required property int index
        required property var modelData

        readonly property real slotWidth: curve.bands.length > 0
          ? curve.width / curve.bands.length : curve.width

        x: band.index * band.slotWidth
        width: band.slotWidth
        height: curve.drawHeight

        // Bar grows from the 0 dB line (vertical centre of the plot): up for
        // a boost, down for a cut, clamped to +-maxGain.
        Rectangle {
          id: bar
          readonly property real gainDb: Math.max(-curve.maxGain, Math.min(curve.maxGain, Number(modelData.gain) || 0))
          readonly property real barHeight: Math.abs(gainDb) / curve.maxGain * (parent.height / 2)
          anchors.horizontalCenter: parent.horizontalCenter
          width: Math.max(2, parent.width - Style.space(curve.compact ? 2 : 6))
          height: Math.max(1, barHeight)
          y: gainDb >= 0 ? parent.height / 2 - height : parent.height / 2
          radius: 2
          color: gainDb >= 0 ? root.accent : Qt.darker(root.foreground, 1.5)
        }

        Text {
          // Compact cards: a tiny label under each mark (see drawHeight).
          // Narrow slots: every 2nd or 3rd label only, so none runs into
          // its neighbour.
          visible: curve.compact && band.index % Math.max(1, Math.ceil(curve.compactLabelPx * 2.6 / band.slotWidth)) === 0
          readonly property int hz: modelData.freq || 0
          text: hz >= 1000
            ? ((hz % 1000 === 0 ? (hz / 1000) : (hz / 1000).toFixed(1)) + "k")
            : String(hz)
          color: Qt.darker(root.foreground, 1.5)
          font.family: root.fontFamily
          font.pixelSize: curve.compactLabelPx
          anchors.horizontalCenter: parent.horizontalCenter
          y: curve.drawHeight + 1
        }

        Text {
          // `root` here is the enclosing panel, exactly as it is everywhere
          // else in this file: an inline component does NOT shadow it.
          visible: !curve.compact
          readonly property int hz: modelData.freq || 0
          text: hz >= 1000
            ? ((hz % 1000 === 0 ? (hz / 1000) : (hz / 1000).toFixed(1)) + "k")
            : String(hz)
          color: Qt.darker(root.foreground, 1.75)
          font.family: root.fontFamily
          // Many bands (custom curves use up to 20) means narrow slots: shrink
          // the label with the count so neighbours do not overlap.
          font.pixelSize: Style.font.caption * (curve.bands.length > 10 ? 0.65 : 0.85)
          anchors.horizontalCenter: parent.horizontalCenter
          y: curve.plotHeight + Style.space(2)
        }
      }
    }
  }

  // A labelled slider with its current value on the right. The value only
  // follows the thumb while dragging, so releasing it and letting the setter
  // finish does not yank the knob backwards.
  component SliderRow: Item {
    id: srow
    required property string label
    required property real value
    property real minimum: 0
    property real maximum: 100
    property real step: 1
    property bool integer: true
    property string suffix: ""
    property string busyLabel: ""
    signal moved(real v)
    signal released(real v)

    implicitHeight: srowLabel.implicitHeight + Style.space(10) + Style.spacing.controlHeight

    Text {
      id: srowLabel
      text: srow.label
      color: Qt.darker(root.foreground, 1.3)
      font.family: root.fontFamily
      font.pixelSize: Style.font.body
      anchors.left: parent.left
      anchors.top: parent.top
    }
    Text {
      id: srowValue
      text: (srow.integer ? Math.round(srow.value) : srow.value.toFixed(2)) + srow.suffix
      color: root.accent
      font.family: root.fontFamily
      font.pixelSize: Style.font.body
      font.bold: true
      anchors.right: parent.right
      anchors.top: parent.top
    }

    PanelSlider {
      id: srowSlider
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.bottom: parent.bottom
      value: srow.integer ? Math.round(srow.value) : srow.value
      minimum: srow.minimum
      maximum: srow.maximum
      step: srow.step
      integer: srow.integer
      bar: root.bar
      fillColor: root.foreground
      knobColor: root.foreground
      trackColor: Qt.darker(root.foreground, 1.6)
      onMoved: function (v) { srow.moved(v) }
      onReleased: function (v) { srow.released(v) }
    }
  }

  // One gesture of the remap table: the gesture on the left, then the action
  // for the Left and Right earbud. Each side elides instead of colliding with
  // its neighbour. `header` renders the column titles instead.
  component TouchCell: Row {
    id: cell
    required property string gesture
    required property string leftText
    required property string rightText
    property bool header: false
    readonly property real gestureW: Style.space(84)
    readonly property real sideW: Math.max(0, (cell.width - cell.gestureW - Style.space(16)) / 2)
    spacing: Style.space(8)

    Text {
      width: cell.gestureW
      text: cell.gesture
      color: Qt.darker(root.foreground, 1.4)
      font.family: root.fontFamily
      font.pixelSize: Style.font.bodySmall
      font.bold: true
      elide: Text.ElideRight
    }
    Text {
      width: cell.sideW
      text: cell.leftText
      color: cell.header ? Qt.darker(root.foreground, 1.35) : root.foreground
      font.family: root.fontFamily
      font.pixelSize: cell.header ? Style.font.caption : Style.font.bodySmall
      font.bold: cell.header
      font.letterSpacing: cell.header ? 1.1 : 0
      elide: Text.ElideRight
    }
    Text {
      width: cell.sideW
      text: cell.rightText
      color: cell.header ? Qt.darker(root.foreground, 1.35) : root.foreground
      font.family: root.fontFamily
      font.pixelSize: cell.header ? Style.font.caption : Style.font.bodySmall
      font.bold: cell.header
      font.letterSpacing: cell.header ? 1.1 : 0
      elide: Text.ElideRight
    }
  }

  // A tab in the panel's top bar. Declared at root scope: an inline component
  // declared inside a Repeater is not valid QML and the delegate silently fails
  // to instantiate, which is why the tab bar rendered empty.
  component TabButton: BorderSurface {
    id: tabBtn
    required property string label
    required property int index
    property real tabWidth: 0

    readonly property bool active: root.tab === tabBtn.index

    width: tabBtn.tabWidth
    implicitHeight: Style.spacing.controlHeight
    radius: Style.cornerRadius
    color: tabBtn.active
      ? Style.selectedFillFor(root.foreground, root.accent)
      : "transparent"
    borderSpec: Border.controlSpec(
      tabBtn.active ? "selected" : "normal", root.foreground, root.accent)
    Behavior on color { ColorAnimation { duration: 120 } }

    Text {
      anchors.centerIn: parent
      text: tabBtn.label
      color: root.foreground
      font.family: root.fontFamily
      font.pixelSize: Style.font.bodySmall
      font.bold: true
      font.letterSpacing: 0.6
    }

    MouseArea {
      anchors.fill: parent
      cursorShape: Qt.PointingHandCursor
      onClicked: root.tab = tabBtn.index
    }
  }

  // A rounded surface with a faint fill; children are laid out in a column
  // with generous padding. `active` gives it the selected highlight.
  component Card: BorderSurface {
    id: card
    property bool active: false
    property real inset: Style.space(12)
    default property alias content: cardBody.data

    implicitHeight: cardBody.implicitHeight + card.inset * 2
    radius: Style.cornerRadius
    color: card.active ? Style.selectedFillFor(root.foreground, root.accent) : root.cardFill
    borderSpec: Border.controlSpec(card.active ? "selected" : "normal", root.foreground, root.accent)
    Behavior on color { ColorAnimation { duration: 120 } }

    Column {
      id: cardBody
      x: card.inset
      y: card.inset
      width: card.width - card.inset * 2
      spacing: Style.space(10)
    }
  }

  // A big square-ish mode button: glyph over label.
  component ModeButton: BorderSurface {
    id: mb
    required property string label
    required property string glyph
    property bool active: false
    property real btnWidth: 0
    signal clicked()

    width: mb.btnWidth
    implicitHeight: Style.spacing.controlHeight * 2
    radius: Style.cornerRadius
    color: mb.active
      ? Style.selectedFillFor(root.foreground, root.accent)
      : (mbMouse.containsMouse ? Style.hoverFillFor(root.foreground, root.accent) : root.cardFill)
    borderSpec: Border.controlSpec(mb.active ? "selected" : "normal", root.foreground, root.accent)
    Behavior on color { ColorAnimation { duration: 120 } }

    Column {
      anchors.centerIn: parent
      width: parent.width - Style.space(8)
      spacing: Style.space(4)

      Text {
        width: parent.width
        text: mb.glyph
        color: mb.active ? root.accent : root.foreground
        font.family: root.fontFamily
        font.pixelSize: Style.font.title * 1.5
        horizontalAlignment: Text.AlignHCenter
      }
      Text {
        width: parent.width
        text: mb.label
        color: root.foreground
        font.family: root.fontFamily
        font.pixelSize: Style.font.bodySmall
        font.bold: mb.active
        elide: Text.ElideRight
        horizontalAlignment: Text.AlignHCenter
      }
    }

    MouseArea {
      id: mbMouse
      anchors.fill: parent
      hoverEnabled: true
      cursorShape: Qt.PointingHandCursor
      onClicked: mb.clicked()
    }
  }

  // A settings row: label (level one) with an optional hint (level two) and a
  // switch on the right.
  component ToggleRow: Item {
    id: trow
    required property string label
    property string hint: ""
    property bool checked: false
    signal toggled()

    implicitHeight: Math.max(trowText.implicitHeight, trowSwitch.implicitHeight)

    Column {
      id: trowText
      anchors.left: parent.left
      anchors.right: trowSwitch.left
      anchors.rightMargin: Style.space(10)
      anchors.verticalCenter: parent.verticalCenter
      spacing: Style.space(2)

      Text {
        width: parent.width
        text: trow.label
        color: root.foreground
        font.family: root.fontFamily
        font.pixelSize: Style.font.body
        elide: Text.ElideRight
      }
      Text {
        width: parent.width
        visible: trow.hint !== ""
        text: trow.hint
        color: Qt.darker(root.foreground, 1.6)
        font.family: root.fontFamily
        font.pixelSize: Style.font.caption
        wrapMode: Text.WordWrap
      }
    }

    // Deliberately not bound to `busy`: Qcy.set() queues rather than drops, so
    // overlapping clicks are safe (see the note in the old toggle grid).
    ToggleSwitch {
      id: trowSwitch
      anchors.right: parent.right
      anchors.verticalCenter: parent.verticalCenter
      checked: trow.checked
      foreground: root.foreground
      accent: root.accent
      onToggled: trow.toggled()
    }
  }

  // One effect as a card of the three-column grid: name, a mini curve and a
  // small status line. Clicking it applies it at once; the card itself shows
  // "Applying…", the check mark or a short error. Only the effect in use is
  // highlighted. User effects have a small "···" menu with "Delete".
  component CurveCard: BorderSurface {
    id: cc
    required property var entry
    property real cardWidth: 0
    property bool canApply: false
    property bool live: false
    // The effect in use has bars moved on top of it.
    property bool adjusted: false
    // idle | running | ok | unconfirmed | error — this card's own apply state.
    property string phase: "idle"
    property bool menuOpen: false
    property bool confirmingDelete: false
    signal applyRequested()
    signal menuToggled()
    signal deleteAsked()
    signal deleteCancelled()
    signal deleteConfirmed()

    readonly property bool isUser: cc.entry.source === "user"
    readonly property real pad: Style.space(8)
    readonly property bool highlighted: cc.live || cc.phase === "ok"

    FontMetrics {
      id: nameMetrics
      font.family: root.fontFamily
      font.pixelSize: Style.font.bodySmall
      font.bold: true
    }
    FontMetrics {
      id: noteMetrics
      font.family: root.fontFamily
      font.pixelSize: Style.font.caption
    }

    width: cc.cardWidth
    implicitHeight: cardCol.implicitHeight + cc.pad * 2
    radius: Style.cornerRadius
    opacity: cc.canApply ? 1.0 : 0.55
    color: cc.highlighted
      ? Style.selectedFillFor(root.foreground, root.accent)
      : ((ccMouse.containsMouse && cc.canApply) ? Style.hoverFillFor(root.foreground, root.accent) : root.cardFill)
    borderSpec: Border.controlSpec(cc.highlighted ? "selected" : "normal", root.foreground, root.accent)
    Behavior on color { ColorAnimation { duration: 120 } }
    Behavior on opacity { NumberAnimation { duration: 120 } }

    MouseArea {
      id: ccMouse
      anchors.fill: parent
      enabled: cc.canApply
      hoverEnabled: true
      cursorShape: cc.canApply ? Qt.PointingHandCursor : Qt.ArrowCursor
      onClicked: cc.applyRequested()
    }

    Column {
      id: cardCol
      x: cc.pad
      y: cc.pad
      width: cc.width - cc.pad * 2
      spacing: Style.space(4)

      Text {
        width: parent.width - (cc.isUser ? Style.space(20) : 0)
        // Two lines reserved, so the cards of a row line up.
        height: Math.ceil(nameMetrics.height) * 2
        text: cc.live && cc.adjusted ? Qcy.tr("{name} (adjusted)", { name: cc.entry.name }) : cc.entry.name
        color: root.foreground
        font.family: root.fontFamily
        font.pixelSize: Style.font.bodySmall
        font.bold: true
        wrapMode: Text.WordWrap
        maximumLineCount: 2
        elide: Text.ElideRight
      }

      EqCurve {
        width: parent.width
        visible: !cc.menuOpen
        bands: cc.entry.bands ? cc.entry.bands : []
        compact: true
      }

      Text {
        width: parent.width
        height: Math.ceil(noteMetrics.height)
        visible: !cc.menuOpen
        text: {
          if (cc.phase === "running") return Qcy.tr("Applying…")
          if (cc.phase === "ok") return Qcy.tr("Applied ✓")
          if (cc.phase === "unconfirmed") return Qcy.tr("Did not confirm")
          if (cc.phase === "error") return Qcy.tr("Error applying")
          if (cc.live) return Qcy.tr("In use")
          return ""
        }
        color: (cc.phase === "error" || cc.phase === "unconfirmed")
          ? Color.urgent
          : ((cc.live || cc.phase === "ok") ? root.accent : Qt.darker(root.foreground, 1.3))
        font.family: root.fontFamily
        font.pixelSize: Style.font.caption
        font.bold: cc.live
        elide: Text.ElideRight

        // The effect's own overall gain, small, at the bottom right.
        Text {
          anchors.right: parent.right
          anchors.verticalCenter: parent.verticalCenter
          text: root.fmtDb(cc.entry.masterGain) + " dB"
          color: Qt.darker(root.foreground, 1.4)
          font.family: root.fontFamily
          font.pixelSize: Style.font.caption * 0.85
        }
      }

      // Small menu, in place of the curve while open.
      Column {
        width: parent.width
        visible: cc.menuOpen
        spacing: Style.space(6)

        Chip {
          visible: !cc.confirmingDelete
          label: Qcy.tr("Delete")
          onClicked: cc.deleteAsked()
        }
        Text {
          visible: cc.confirmingDelete
          text: Qcy.tr("Delete this effect?")
          color: root.foreground
          font.family: root.fontFamily
          font.pixelSize: Style.font.caption
          wrapMode: Text.WordWrap
          width: parent.width
        }
        Row {
          visible: cc.confirmingDelete
          spacing: Style.space(6)
          Chip {
            label: Qcy.tr("Yes")
            onClicked: cc.deleteConfirmed()
          }
          Chip {
            label: Qcy.tr("No")
            onClicked: cc.deleteCancelled()
          }
        }
      }
    }

    // The "···" button of user effects.
    Item {
      visible: cc.isUser
      x: cc.width - cc.pad - width
      y: cc.pad
      width: Style.space(20)
      height: Style.space(20)

      Text {
        anchors.centerIn: parent
        text: "···"
        color: cc.menuOpen ? root.accent : Qt.darker(root.foreground, 1.2)
        font.family: root.fontFamily
        font.pixelSize: Style.font.body
        font.bold: true
      }

      MouseArea {
        anchors.fill: parent
        cursorShape: Qt.PointingHandCursor
        onClicked: cc.menuToggled()
      }
    }
  }

  // Ten tall vertical sliders (positions 0..9, standard frequency table, +-12
  // dB in 0.5 steps). The value in dB is above each one, the frequency below,
  // a line marks zero. It owns no state: it reports edited(index, dB) while
  // dragging and committed() on release; a double click zeroes one bar.
  component BarEditor: Item {
    id: be
    required property var gains
    property bool editable: true
    signal edited(int index, real value)
    signal committed()

    readonly property real plotHeight: Style.space(150)
    readonly property real valueHeight: Math.ceil(Style.font.caption) + Style.space(6)
    readonly property real labelHeight: Math.ceil(Style.font.caption) + Style.space(6)
    readonly property real slotWidth: be.width / 10
    readonly property real knobSize: Style.space(12)

    implicitHeight: be.valueHeight + be.plotHeight + be.labelHeight
    opacity: be.editable ? 1.0 : 0.5

    Repeater {
      model: 10
      Item {
        id: slot
        required property int index
        readonly property real gainDb: Number(be.gains[slot.index]) || 0
        readonly property int hz: root.editorFreqs[slot.index]
        // Vertical travel of the knob's centre, from the plot's middle.
        readonly property real half: plot.height / 2 - be.knobSize / 2
        readonly property real knobY: plot.height / 2 - slot.gainDb / root.editorMaxDb * slot.half

        x: slot.index * be.slotWidth
        width: be.slotWidth
        height: be.implicitHeight

        Text {
          width: parent.width
          height: be.valueHeight
          text: root.fmtDb(slot.gainDb)
          color: slot.gainDb === 0 ? Qt.darker(root.foreground, 1.6) : root.foreground
          font.family: root.fontFamily
          font.pixelSize: Style.font.caption * 0.85
          font.bold: slot.gainDb !== 0
          horizontalAlignment: Text.AlignHCenter
          verticalAlignment: Text.AlignVCenter
        }

        Rectangle {
          id: plot
          y: be.valueHeight
          width: parent.width - Style.space(3)
          anchors.horizontalCenter: parent.horizontalCenter
          height: be.plotHeight
          radius: Style.space(3)
          color: Qt.rgba(root.foreground.r, root.foreground.g, root.foreground.b, 0.08)

          // The rail.
          Rectangle {
            anchors.horizontalCenter: parent.horizontalCenter
            y: be.knobSize / 2
            width: Style.space(4)
            height: parent.height - be.knobSize
            radius: width / 2
            color: Qt.rgba(root.foreground.r, root.foreground.g, root.foreground.b, 0.22)
          }

          // The zero line.
          Rectangle {
            anchors.left: parent.left
            anchors.right: parent.right
            anchors.verticalCenter: parent.verticalCenter
            height: 1
            color: Qt.darker(root.foreground, 1.25)
          }

          // The fill, from zero to the knob.
          Rectangle {
            anchors.horizontalCenter: parent.horizontalCenter
            width: Style.space(4)
            y: Math.min(slot.knobY, plot.height / 2)
            height: Math.abs(slot.knobY - plot.height / 2)
            radius: width / 2
            color: slot.gainDb >= 0 ? root.accent : Qt.darker(root.foreground, 1.4)
          }

          // The knob.
          Rectangle {
            anchors.horizontalCenter: parent.horizontalCenter
            y: slot.knobY - be.knobSize / 2
            width: be.knobSize
            height: be.knobSize
            radius: width / 2
            color: slot.gainDb === 0 ? root.foreground : root.accent
            border.width: 1
            border.color: Qt.darker(root.foreground, 1.6)
          }

          MouseArea {
            id: dragArea
            anchors.fill: parent
            enabled: be.editable
            preventStealing: true
            cursorShape: be.editable ? Qt.SizeVerCursor : Qt.ArrowCursor

            // y is relative to the rail's box, clamped to its ends.
            function setFrom(y) {
              var v = (plot.height / 2 - y) / slot.half * root.editorMaxDb
              be.edited(slot.index, Math.max(-root.editorMaxDb, Math.min(root.editorMaxDb, Math.round(v * 2) / 2)))
            }
            // A press without movement only commits when it is not the first
            // half of a double click, which zeroes and applies once.
            property bool moved: false
            property bool skipRelease: false
            Timer {
              id: clickGate
              interval: 250
              onTriggered: be.committed()
            }
            onPressed: function (mouse) {
              dragArea.moved = false
              dragArea.setFrom(mouse.y)
            }
            onPositionChanged: function (mouse) {
              if (dragArea.pressed) {
                dragArea.moved = true
                dragArea.setFrom(mouse.y)
              }
            }
            onReleased: {
              if (dragArea.skipRelease) {
                dragArea.skipRelease = false   // release closing a double click
              } else if (dragArea.moved) {
                clickGate.stop()
                be.committed()
              } else {
                clickGate.restart()
              }
            }
            onCanceled: {
              clickGate.stop()
              be.committed()
            }
            onDoubleClicked: {
              clickGate.stop()
              dragArea.skipRelease = true
              be.edited(slot.index, 0)
              be.committed()
            }
          }
        }

        Text {
          width: parent.width
          y: be.valueHeight + be.plotHeight + Style.space(3)
          text: slot.hz >= 1000
            ? ((slot.hz % 1000 === 0 ? (slot.hz / 1000) : (slot.hz / 1000).toFixed(1)) + "k")
            : String(slot.hz)
          color: Qt.darker(root.foreground, 1.45)
          font.family: root.fontFamily
          font.pixelSize: Style.font.caption * 0.85
          horizontalAlignment: Text.AlignHCenter
        }
      }
    }
  }

  // A horizontal set of mutually exclusive choices.
  component ChoiceRow: Item {
    id: crow
    required property string label
    property var model: []
    required property string current
    // Read-only rows show the selection but ignore clicks.
    property bool readOnly: false
    signal picked(string value)

    implicitHeight: crowLabel.implicitHeight + Style.space(8) + chipFlow.implicitHeight

    Text {
      id: crowLabel
      text: crow.label
      color: Qt.darker(root.foreground, 1.3)
      font.family: root.fontFamily
      font.pixelSize: Style.font.body
      anchors.left: parent.left
      anchors.top: parent.top
    }

    Flow {
      id: chipFlow
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.top: crowLabel.bottom
      anchors.topMargin: Style.space(8)
      spacing: Style.space(6)

      Repeater {
        model: crow.model
        Chip {
          required property var modelData
          label: modelData.label
          // Compared as strings: the caller's `current` is a string while
          // model values may be numbers, and "0" === 0 is false in JS, which
          // left the intensity pills never appearing selected.
          active: String(crow.current) === String(modelData.value)
          onClicked: if (!crow.readOnly) crow.picked(String(modelData.value))
        }
      }
    }
  }
}
