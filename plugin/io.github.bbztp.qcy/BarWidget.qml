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
// Panel — Esc: close · r: refresh
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

  // Material Design Icons battery glyphs (Nerd Font), as used by omarchy.power.
  readonly property var _levelGlyphs: ["󰁺", "󰁻", "󰁼", "󰁽", "󰁾", "󰁿", "󰂀", "󰂁", "󰂂", "󰁹"]


  // Detailed noise scenes (Ajustes). The three headline modes on the Status
  // tab are noiseButtons below.
  readonly property var ancModes: [
    { key: "adaptive", label: "Adaptativo" },
    { key: "indoor", label: "Interno" },
    { key: "commuting", label: "Deslocamento" },
    { key: "noisy", label: "Ruidoso" },
    { key: "antiwind", label: "Anti-vento" }
  ]

  // Headline modes: glyphs are Material Design Icons code points (headphones,
  // ear-hearing, volume-high). "Cancelamento" is the adaptive scene; the
  // finer scenes are in Ajustes.
  readonly property var noiseButtons: [
    { key: "adaptive", label: "Cancelamento", glyph: 0xF02CB },
    { key: "transparency", label: "Transparência", glyph: 0xF07E6 },
    { key: "off", label: "Normal", glyph: 0xF057E }
  ]
  readonly property var _cancelScenes: ["adaptive", "indoor", "commuting", "noisy", "antiwind"]

  // LDAC is deliberately absent: writing 0x23 reboots this N70 and, unlike
  // multipoint below, the restart is not documented behaviour. Multipoint IS
  // listed because the N70 product notes state the earphones restart every time
  // Dual Device Connection is toggled — that is expected, not a fault.
  readonly property var toggleDefs: [
    { key: "game", label: "Modo jogo", hint: "Menor latência" },
    { key: "sleep", label: "Modo sono", hint: "Desativa os toques; sem efeito no som" },
    { key: "wind", label: "Detecção de vento", hint: "" },
    { key: "multipoint", label: "Multiponto", hint: "Reinicia os fones; é esperado, não é falha" }
  ]

  // ---- library UI state ----
  // A fill that is only a hint lighter than the panel, from the shell's
  // foreground so it follows the theme.
  readonly property color cardFill: Qt.rgba(foreground.r, foreground.g, foreground.b, 0.06)
  property bool saveOpen: false
  property string pendingDeleteId: ""

  readonly property bool isStale: !!(rep && rep.stale === true)
  readonly property int activeSlot: rep && rep.eq && rep.eq.preset !== null && rep.eq.preset !== undefined
    ? Number(rep.eq.preset) : -1
  // Slots 6 (factory) and 11 (hearing test) are never overwritten.
  readonly property bool slotProtected: activeSlot === 6 || activeSlot === 11
  // A tail band (position 10+) means the device reports a populated 143-byte
  // payload; master-gain then needs the unproven full-length write.
  readonly property bool masterGainLocked: {
    var b = root.eqBandModel
    if (!b) return false
    if (b.length > 10) return true
    for (var i = 0; i < b.length; i++) {
      if (Number(b[i].index) >= 10) return true
    }
    return false
  }
  readonly property bool canApplyLibrary: Qcy.connected && !!rep && activeSlot >= 0
    && !slotProtected && Qcy.libraryOp.phase !== "running"
  readonly property bool canSaveCurrent: Qcy.connected && !!rep && Qcy.libraryOp.phase !== "running"

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

  // The library curve whose bands equal the live ones, if any.
  function entryIsLive(e) {
    return root._eqSig !== "" && root.activeSlot === 0 && entrySig(e) === root._eqSig
  }

  function curveName(id) {
    var l = Qcy.library
    for (var i = 0; i < l.length; i++) if (l[i].id === id) return l[i].name
    return id
  }

  function activeCurveName() {
    var l = Qcy.library
    for (var i = 0; i < l.length; i++) {
      if (entryIsLive(l[i])) return l[i].name
    }
    return eqLabel()
  }

  function sourceLabel(s) {
    if (s === "user") return "SUA"
    if (s === "device-import") return "DO APARELHO"
    if (s === "factory-snapshot") return "DE FÁBRICA"
    return String(s).toUpperCase()
  }

  function reasonText(r) {
    var t = String(r || "")
    if (t.indexOf("positions 10-19") !== -1 || t.indexOf("addressable") !== -1)
      return "Usa bandas nas posições 10–19: a gravação de 143 bytes ainda não foi validada em hardware."
    if (t.indexOf("type") !== -1) return "Tem uma banda de tipo não suportado."
    return t !== "" ? t : "Não pode ser aplicada."
  }

  function nameValid(n) {
    var t = String(n || "").trim()
    return t.length > 0 && t.length <= 40 && t.charAt(0) !== "-"
  }

  function submitSave(n) {
    if (!nameValid(n) || !canSaveCurrent) return
    Qcy.libraryImportDevice(String(n).trim())
    root.saveOpen = false
    nameField.text = ""
  }

  function opText() {
    var op = Qcy.libraryOp
    if (op.phase === "idle") return ""
    var extra = ""
    if (op.backup !== "" && String(op.message).indexOf(op.backup) === -1)
      extra = "\nBackup: " + op.backup
    if (op.kind === "apply") {
      var nm = curveName(op.id)
      if (op.phase === "running") return "Aplicando \u201c" + nm + "\u201d…"
      if (op.phase === "ok") return "Aplicado e confirmado pelo aparelho: " + nm + "." + extra
      if (op.phase === "unconfirmed")
        return "Gravado, mas o aparelho não confirmou." + (op.message !== "" ? "\n" + op.message : "") + extra
      return "Não foi possível aplicar: " + op.message + extra
    }
    if (op.kind === "save") {
      if (op.phase === "running") return "Salvando a curva atual…"
      if (op.phase === "ok") return "Curva salva na biblioteca."
      return "Não foi possível salvar: " + op.message
    }
    if (op.kind === "delete") {
      if (op.phase === "running") return "Excluindo…"
      if (op.phase === "ok") return "Curva excluída (movida para a lixeira da biblioteca)."
      return "Não foi possível excluir: " + op.message
    }
    return ""
  }

  // Opening the Sound tab reads the library from disk (offline, no daemon).
  onTabChanged: {
    root.saveOpen = false
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
      if (!Qcy.connected) return name + " — desconectado"
      if (!batt) return name + " — lendo a bateria…"
      var parts = ["E " + batt.left + "%", "D " + batt.right + "%"]
      if (batt.casePresent) parts.push("estojo " + batt.case + "%")
      return name + " — " + parts.join(" · ")
    }

  function levelGlyph(pct) {
    var idx = Math.max(0, Math.min(9, Math.floor((pct / 100) * 10)))
    return _levelGlyphs[idx]
  }

  function heroStatus() {
    if (!Qcy.connected) return "Desconectado"
    if (isStale) return "Dados em cache há " + Math.max(0, Math.round(Number(rep.ageSeconds) || 0)) + " s (fones em uso por outro app)"
    if (Qcy.loading) return "Lendo…"
    if (!rep) return "Ocioso"
    return "Conectado"
  }

  // The model name BlueZ reports, trimmed of the vendor prefix so the header
  // reads "MeloBuds N70" rather than repeating "QCY".
  readonly property string modelName: {
    var raw = ""
    if (Qcy.device) raw = (Qcy.device.name || Qcy.device.deviceName || "")
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
    return togglesReport[key] === true
  }

  function batteryRows() {
    if (!batt) return []
    var rows = [
      { label: "Left", level: batt.left, charging: batt.leftCharging, present: true },
      { label: "Right", level: batt.right, charging: batt.rightCharging, present: true }
    ]
    if (batt.casePresent) {
      rows.push({ label: "Case", level: batt.case, charging: batt.caseCharging, present: true })
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

  // The daemon reports every band with a non-zero frequency (1..20); the
  // backend patches the full payload it read, so any non-empty report is
  // writable. An empty list means the read failed or returned no EQ data.
  function eqWritable() {
    var b = rep && rep.eq ? rep.eq.bands : null
    return !!b && b.length >= 1 && b.length <= 20
  }

  function eqSetMasterGain(v) {
    if (!root.eqWritable()) return
    var db = Math.max(root.masterGainMin, Math.min(root.masterGainMax, Number(v)))
    if (isNaN(db)) return
    db = Math.round(db * 10) / 10
    Qcy.set("Ganho geral", ["master-gain", db.toFixed(1)])
    // Optimistic: the next full read reconciles.
    root._masterGainDb = db
    root._masterGainSig = String(db)
  }

  function eqBands() {
    return root.eqBandModel
  }

  // Each updater runs on its own: a throw in one must not skip the rest.
  function updateAll() {
    var steps = [updateEqBands, updateEqPreset, updateMasterGain, updateTouchRows]
    for (var i = 0; i < steps.length; i++) {
      try { steps[i]() } catch (e) { console.warn("qcy: update step failed:", e) }
    }
  }
  onRepChanged: updateAll()

  // What this unit actually answered, from the daemon's one-off probe. Falls
  // back to the shape of the report when no probe has run, so the panel is
  // never empty just because capabilities are missing.
  readonly property var caps: (rep && rep.capabilities) ? rep.capabilities : null

  function has(feature) {
    if (caps) return caps[feature] === true
    if (feature === "spatial") return !!(rep && rep.spatial)
    if (feature === "keys") return !!(rep && rep.keys)
    return true
  }

  property bool fitRunning: false

  function startFit() {
    fitRunning = true
    Qcy.set("Teste de vedação", ["fit", "start"])
    // The earbuds play a few seconds of sound while they measure the seal; the
    // panel has no completion event to wait on, so clear the flag on a timer.
    fitTimer.restart()
  }

  Timer {
    id: fitTimer
    interval: 9000
    onTriggered: root.fitRunning = false
  }

  function eqLabel() {
    if (!rep || !rep.eq || rep.eq.preset === null || rep.eq.preset === undefined) return "—"
    var p = rep.eq.preset
    // Slot 11 is the profile this user created with the app's hearing test.
    if (p === 11) return "Personalizado (teste auditivo)"
    if (p === 6) return "Clássico de fábrica (slot 6)"
    // Slot 0 holds whatever was last written there; its name is unknown.
    return "Slot " + p
  }

  function updatedLabel() {
    var tick = root.nowTick // binding dependency for the "ago" clock
    if (Qcy.error !== "") return Qcy.error
    if (Qcy.loading) return "Lendo os fones…"
    if (!Qcy.updatedAt) return "Abra para ler o estado dos fones"
    var s = Math.round((Date.now() - Qcy.updatedAt) / 1000)
    if (s < 5) return "Atualizado agora"
    if (s < 60) return "Atualizado há " + s + " s"
    return "Atualizado há " + Math.round(s / 60) + " min"
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
    { label: "Baixa", value: 0 },
    { label: "Média", value: 1 },
    { label: "Alta", value: 2 }
  ]

  function setAncStrength(value) {
    var next = Object.assign({}, optimistic)
    next["ancLevel"] = value
    optimistic = next
    // The scene has to be re-sent with the new strength; the earbuds treat the
    // third ANC parameter as part of the same command.
    Qcy.set("Intensidade ANC", ["anc", currentAnc(), "--level", String(value)])
  }

  // ---- sliders -------------------------------------------------------------
  // The value shown is the optimistic one while the setter is in flight, so the
  // knob does not snap back to the last read value for the second or two the
  // round-trip takes.

  function sliderValue(key, fallback) {
    if (optimistic[key] !== undefined) return optimistic[key]
    var v = rep && rep[key] !== undefined && rep[key] !== null ? rep[key] : fallback
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
    { label: "Nunca", value: "never" },
    { label: "15 min", value: "15" },
    { label: "30 min", value: "30" },
    { label: "60 min", value: "60" },
    { label: "90 min", value: "90" }
  ]

  // Touch-control remap table, decoded from the pairs reported for characteristic
  // 000D. Ids follow Quicky's KeyID/FuncID vocabulary; an id outside it is shown
  // as-is rather than guessed at.
  readonly property var funcNames: ({
    0x00: "Disabled", 0x01: "Play / pause", 0x02: "Previous", 0x03: "Next",
    0x04: "Voice assistant", 0x05: "Volume up", 0x06: "Volume down",
    0x07: "Game mode", 0x08: "Answer call", 0x09: "Reject call",
    0x0a: "Hold call", 0x0b: "Redial"
  })

  readonly property var keyNames: ({
    0x01: "L tap", 0x02: "R tap", 0x03: "L double", 0x04: "R double",
    0x05: "L triple", 0x06: "R triple", 0x07: "L quad", 0x08: "R quad",
    0x09: "L hold", 0x0a: "R hold",
    0x15: "L tap (call)", 0x16: "R tap (call)", 0x17: "L double (call)",
    0x18: "R double (call)", 0x19: "L triple (call)", 0x1a: "R triple (call)",
    0x1b: "L quad (call)", 0x1c: "R quad (call)", 0x1d: "L hold (call)",
    0x1e: "R hold (call)"
  })

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
    var rows = []
    for (var i = 0; i < pairs.length; i++) {
      var bits = String(pairs[i]).split(":")
      var kid = parseInt(bits[0], 16)
      var fid = parseInt(bits[1], 16)
      rows.push({
        slot: root.keyNames[kid] || ("0x" + bits[0]),
        action: root.funcNames[fid] || ("0x" + bits[1])
      })
    }
    root.touchRowModel = rows
  }

  function touchRows() {
    return root.touchRowModel
  }

  function promptMax() {
    var p = rep ? rep.promptVolume : null
    if (p && p.max) return p.max
    return 15
  }

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
    Qcy.set("Desligamento automático", ["auto-off", value])
  }

  Component.onCompleted: {
    Qcy.settings = settings
    root.updateAll()
  }
  onSettingsChanged: Qcy.settings = settings

  // Opening the panel is the display trigger: read battery lightly now and keep
  // it fresh while open; full status re-runs only occasionally.
  onOpenedChanged: {
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
    function onSetFailed() {
      // Drop the optimistic master gain; the refresh Qcy starts re-reads the
      // real value, which the cleared signature lets through.
      root._masterGainSig = ""
      Qcy.refresh()
    }
    function onConnectedChanged() {
      root.optimistic = ({})
      if (!Qcy.connected) root.close()
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
          // Status (device + noise mode), Som (live curve + library) and
          // Ajustes (everything else). Short tabs keep every section reachable.
          Row {
            id: tabBar
            width: parent.width
            spacing: Style.space(6)

            Repeater {
              model: [
                { label: "Status", index: 0 },
                { label: "Som", index: 1 },
                { label: "Ajustes", index: 2 }
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

            // Earbuds in the middle with their charge either side.
            Item {
              width: parent.width
              implicitHeight: Math.max(
                Math.max(heroArt.height, heroPillLeft.height),
                heroPillRight.height)

              Image {
                id: heroArt
                anchors.centerIn: parent
                width: 176
                height: 112
                source: "assets/earbuds.svg"
                sourceSize: Qt.size(176, 112)
                fillMode: Image.PreserveAspectFit
                opacity: Qcy.connected ? 1.0 : 0.4
              }

              ChargePill {
                id: heroPillLeft
                anchors.right: heroArt.left
                anchors.rightMargin: Style.space(10)
                anchors.verticalCenter: heroArt.verticalCenter
                visible: root.batt !== null
                glyph: root.batt ? root.levelGlyph(root.batt.left) : ""
                level: root.batt ? root.batt.left : 0
                charging: root.batt ? root.batt.leftCharging : false
              }

              ChargePill {
                id: heroPillRight
                anchors.left: heroArt.right
                anchors.leftMargin: Style.space(10)
                anchors.verticalCenter: heroArt.verticalCenter
                visible: root.batt !== null
                glyph: root.batt ? root.levelGlyph(root.batt.right) : ""
                level: root.batt ? root.batt.right : 0
                charging: root.batt ? root.batt.rightCharging : false
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

            // Battery card: the case is the one people forget; left and right
            // are repeated as gauges so the three read the same way.
            Card {
              width: parent.width
              visible: !!root.batt

              BatteryGauge {
                width: parent.width
                label: "Esq."
                level: root.batt ? root.batt.left : 0
                charging: root.batt ? root.batt.leftCharging : false
              }
              BatteryGauge {
                width: parent.width
                label: "Dir."
                level: root.batt ? root.batt.right : 0
                charging: root.batt ? root.batt.rightCharging : false
              }
              BatteryGauge {
                width: parent.width
                present: !!(root.batt && root.batt.casePresent)
                label: "Estojo"
                level: root.batt ? root.batt.case : 0
                charging: root.batt ? root.batt.caseCharging : false
              }
            }

            // Noise mode: three big buttons, only when the unit answers ANC.
            Column {
              width: parent.width
              spacing: Style.space(8)
              visible: root.has("anc")

              PanelSectionHeader {
                text: "MODO DE RUÍDO"
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

            // ---- Live curve ----
            Card {
              width: parent.width
              visible: !!root.rep

              Item {
                width: parent.width
                implicitHeight: liveTitle.implicitHeight

                Text {
                  id: liveTitle
                  text: "CURVA ATUAL"
                  color: Qt.darker(root.foreground, 1.35)
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.caption
                  font.bold: true
                  font.letterSpacing: 1.1
                  anchors.left: parent.left
                  anchors.verticalCenter: parent.verticalCenter
                }
                Text {
                  text: root.activeCurveName()
                  color: root.foreground
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.body
                  font.bold: true
                  anchors.left: liveTitle.right
                  anchors.leftMargin: Style.space(10)
                  anchors.right: parent.right
                  anchors.verticalCenter: parent.verticalCenter
                  elide: Text.ElideLeft
                  horizontalAlignment: Text.AlignRight
                }
              }

              EqCurve {
                width: parent.width
                bands: root.eqBands()
              }

              SliderRow {
                width: parent.width
                enabled: !root.masterGainLocked
                opacity: root.masterGainLocked ? 0.5 : 1.0
                label: "Ganho geral"
                minimum: root.masterGainMin
                maximum: root.masterGainMax
                step: 0.5
                integer: false
                value: root.masterGainDb()
                suffix: " dB"
                onReleased: function (v) { root.eqSetMasterGain(v) }
              }

              Text {
                width: parent.width
                visible: root.masterGainLocked
                text: "Ajuste de ganho geral desativado: a gravação completa de 143 bytes ainda não foi validada em hardware."
                color: Qt.darker(root.foreground, 1.5)
                font.family: root.fontFamily
                font.pixelSize: Style.font.caption
                wrapMode: Text.WordWrap
              }
            }

            Text {
              width: parent.width
              visible: !root.rep
              text: Qcy.loading ? "Lendo os fones…" : "Abra o painel com os fones conectados para ver a curva atual."
              color: Qt.darker(root.foreground, 1.5)
              font.family: root.fontFamily
              font.pixelSize: Style.font.bodySmall
              wrapMode: Text.WordWrap
            }

            // ---- Protected slot warning ----
            Card {
              width: parent.width
              visible: root.slotProtected

              Text {
                width: parent.width
                text: "Efeito de fábrica ou seu custom está ativo (slot " + root.activeSlot
                  + "). Para não sobrescrever, a biblioteca não aplica enquanto o slot 6 ou 11 estiver ativo."
                color: Color.urgent
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
                wrapMode: Text.WordWrap
              }
            }

            // ---- Library ----
            Column {
              width: parent.width
              spacing: Style.space(8)

              PanelSectionHeader {
                text: "BIBLIOTECA"
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
                  ? "Carregando a biblioteca…"
                  : "A biblioteca está vazia. Salve a curva atual abaixo."
                color: Qt.darker(root.foreground, 1.5)
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
                wrapMode: Text.WordWrap
              }

              Grid {
                id: libGrid
                width: parent.width
                columns: 2
                columnSpacing: Style.space(8)
                rowSpacing: Style.space(8)

                Repeater {
                  model: Qcy.library
                  CurveCard {
                    required property var modelData
                    entry: modelData
                    cardWidth: (libGrid.width - Style.space(8)) / 2
                    canApply: root.canApplyLibrary
                    live: root.entryIsLive(modelData)
                    phase: Qcy.libraryOp.kind === "apply" && Qcy.libraryOp.id === modelData.id
                      ? Qcy.libraryOp.phase : "idle"
                    confirmingDelete: root.pendingDeleteId === modelData.id
                    deleteBlocked: Qcy.libraryOp.phase === "running"
                    onApplyRequested: {
                      root.pendingDeleteId = ""
                      Qcy.libraryApply(modelData.id)
                    }
                    onDeleteAsked: root.pendingDeleteId = modelData.id
                    onDeleteCancelled: root.pendingDeleteId = ""
                    onDeleteConfirmed: {
                      root.pendingDeleteId = ""
                      Qcy.libraryDelete(modelData.id)
                    }
                  }
                }
              }

              // Result of the last library operation, with the backup path
              // when the write was sent but not confirmed.
              Text {
                width: parent.width
                visible: root.opText() !== ""
                text: root.opText()
                color: Qcy.libraryOp.phase === "error" ? Color.urgent : root.foreground
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
                wrapMode: Text.WrapAnywhere
              }
            }

            // ---- Save current curve ----
            Column {
              width: parent.width
              spacing: Style.space(8)

              Chip {
                visible: !root.saveOpen
                opacity: root.canSaveCurrent ? 1.0 : 0.5
                label: "Salvar curva atual na biblioteca"
                onClicked: {
                  if (!root.canSaveCurrent) return
                  root.saveOpen = true
                  nameField.forceActiveFocus()
                }
              }

              Column {
                width: parent.width
                spacing: Style.space(8)
                visible: root.saveOpen

                TextField {
                  id: nameField
                  width: parent.width
                  placeholderText: "Nome da curva"
                  maximumLength: 40
                  foreground: root.foreground
                  accent: root.accent
                  onAccepted: root.submitSave(text)
                  Keys.onEscapePressed: root.saveOpen = false
                }

                Row {
                  spacing: Style.space(8)
                  Chip {
                    label: "Salvar"
                    active: root.nameValid(nameField.text)
                    onClicked: root.submitSave(nameField.text)
                  }
                  Chip {
                    label: "Cancelar"
                    onClicked: root.saveOpen = false
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
                text: "CENAS DE CANCELAMENTO"
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
                label: "Intensidade"
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
                label: "Áudio espacial"
                hint: ""
                checked: root.toggleState("spatial")
                onToggled: root.setToggle("spatial", "Áudio espacial", !root.toggleState("spatial"))
              }
            }

            // ---- Audio ----
            Card {
              width: parent.width

              SliderRow {
                width: parent.width
                label: "Balanço"
                minimum: 0
                maximum: 100
                step: 1
                suffix: "%"
                value: root.sliderValue("balance", 50)
                onReleased: function (v) { root.commitSlider("balance", "Balanço", function (n) { return ["balance", String(Math.round(n))] }, Math.round(v)) }
              }

              // The device reports its prompt volume as 0..max; the slider
              // follows that range instead of the CLI's 0-255.
              SliderRow {
                width: parent.width
                visible: root.has("promptVol")
                label: "Volume dos avisos"
                minimum: 0
                maximum: root.promptMax()
                step: 1
                value: root.sliderValue("promptVolume", 6)
                onReleased: function (v) { root.commitSlider("promptVolume", "Volume dos avisos", function (n) { return ["prompt-vol", String(Math.round(n))] }, Math.round(v)) }
              }

              ChoiceRow {
                width: parent.width
                label: "Desligamento automático"
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
                  text: "Teste de vedação das pontas"
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
                  label: root.fitRunning ? "Tocando…" : "Iniciar"
                  active: root.fitRunning
                  onClicked: {
                    if (Qcy.settling) return
                    root.startFit()
                  }
                }
              }
            }

            // ---- Touch controls (read-only) ----
            Column {
              visible: root.touchRows().length > 0
              width: parent.width
              spacing: Style.space(8)

              PanelSectionHeader {
                text: "CONTROLES DE TOQUE"
                foreground: root.foreground
                fontFamily: root.fontFamily
              }

              Grid {
                id: touchGrid
                width: parent.width
                columns: 2
                columnSpacing: Style.space(20)
                rowSpacing: Style.space(7)

                Repeater {
                  model: root.touchRows()
                  TouchCell {
                    required property var modelData
                    slot: modelData.slot
                    action: modelData.action
                    cellWidth: (touchGrid.width - Style.space(20)) / 2
                  }
                }
              }
            }

            // ---- Device ----
            Column {
              width: parent.width
              spacing: Style.space(8)

              PanelSectionHeader {
                text: "APARELHO"
                foreground: root.foreground
                fontFamily: root.fontFamily
              }

              StatRow {
                width: parent.width
                label: "Modelo"
                value: root.modelName
              }
              StatRow {
                width: parent.width
                label: "Firmware"
                value: root.rep && root.rep.firmware && root.rep.firmware.left
                  ? root.rep.firmware.left : "—"
              }
            }
          }

          // ---------- Footer (all tabs) ----------
          Row {
            spacing: Style.space(8)

            Chip {
              label: Qcy.loading ? "Lendo…" : "Atualizar"
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

  // A horizontal charge gauge: label, a proportional bar, the percentage and a
  // charging glyph. Reads at a glance the way the earbud's own LEDs do, which
  // plain "Left 100%" rows do not.
  component BatteryGauge: Item {
    id: gauge
    required property string label
    required property int level
    property bool charging: false
    property bool present: true

    readonly property color fillColor: gauge.level <= 15
      ? Color.urgent
      : (gauge.level <= 30 ? root.accent : root.foreground)

    // Sized by its text, not by the track. `implicitHeight: gaugeTrack.height`
    // left the whole row 6px tall while the label and percentage are ~13px, so
    // both spilled above and below into the neighbouring sections.
    implicitHeight: Math.max(gaugeTrack.height, gaugeLabel.implicitHeight, gaugePct.implicitHeight)
    visible: gauge.present

    Text {
      id: gaugeLabel
      text: gauge.label
      color: Qt.darker(root.foreground, 1.3)
      font.family: root.fontFamily
      font.pixelSize: Style.font.bodySmall
      width: Style.space(52)
      anchors.left: parent.left
      anchors.verticalCenter: parent.verticalCenter
    }

    Rectangle {
      id: gaugeTrack
      anchors.left: gaugeLabel.right
      anchors.leftMargin: Style.space(10)
      anchors.right: gaugePct.left
      anchors.rightMargin: Style.space(10)
      anchors.verticalCenter: parent.verticalCenter
      height: Math.max(6, Math.round(Style.spacing.controlHeight * 0.22))
      radius: height / 2
      color: Qt.darker(root.foreground, 2.2)

      Rectangle {
        anchors.left: parent.left
        anchors.top: parent.top
        anchors.bottom: parent.bottom
        anchors.leftMargin: 1
        anchors.topMargin: 1
        anchors.bottomMargin: 1
        width: Math.max(1, (parent.width - 2) * Math.max(0, Math.min(100, gauge.level)) / 100)
        radius: parent.radius
        color: gauge.fillColor

        // A breathing highlight while charging, so the state reads without
        // having to read the glyph.
        SequentialAnimation on opacity {
          running: gauge.charging
          loops: Animation.Infinite
          NumberAnimation { from: 0.55; to: 1.0; duration: 900; easing.type: Easing.InOutQuad }
          NumberAnimation { from: 1.0; to: 0.55; duration: 900; easing.type: Easing.InOutQuad }
        }
      }
    }

    Text {
      id: gaugePct
      text: gauge.level + "%"
      color: root.foreground
      font.family: root.fontFamily
      font.pixelSize: Style.font.bodySmall
      font.bold: true
      anchors.right: gaugeCharge.left
      anchors.rightMargin: gauge.charging ? Style.space(4) : 0
      anchors.verticalCenter: parent.verticalCenter
    }

    Text {
      id: gaugeCharge
      text: "󰂄"
      color: root.accent
      font.family: root.fontFamily
      font.pixelSize: Style.font.bodySmall
      visible: gauge.charging
      anchors.right: parent.right
      anchors.verticalCenter: parent.verticalCenter
    }
  }

  // The EQ response as a signed curve with a zero line, the axis labels and the
  // per-band dots. Read-only — see the note above, writing a preset overwrites
  // the slot in place.
  component EqCurve: Item {
    id: curve
    required property var bands
    // Mini version for library cards: no frequency labels, shorter plot.
    property bool compact: false

    readonly property real maxGain: 10
    // The plot stops short of the bottom so the band labels have room INSIDE
    // this item. They used to be anchored to the delegate's bottom edge, which
    // put them ~2px past `height` and on top of whatever followed.
    readonly property real plotHeight: Style.spacing.controlHeight * (curve.compact ? 0.9 : 1.6)
    readonly property int labelHeight: curve.compact ? 0 : Math.ceil(Style.font.caption * 0.85) + Style.space(2)
    implicitHeight: plotHeight + labelHeight

    Rectangle {
      id: zeroLine
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.verticalCenter: parent.verticalCenter
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
        height: curve.plotHeight

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
      color: root.foreground
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

  component ChargePill: BorderSurface {
    id: pill
    required property string glyph
    required property int level
    property bool charging: false
    readonly property color pillColor: pill.level <= 15
      ? Color.urgent
      : (pill.level <= 30 ? root.accent : root.foreground)
    implicitWidth: pillRow.implicitWidth + Style.space(20)
    implicitHeight: pillRow.implicitHeight + Style.space(10)
    radius: Style.cornerRadius
    color: "transparent"
    borderSpec: Border.controlSpec("normal", root.foreground, root.accent)
    Row {
      id: pillRow
      anchors.centerIn: parent
      spacing: Style.space(6)
      Text {
        text: pill.glyph
        color: pill.pillColor
        font.family: root.fontFamily
        font.pixelSize: Style.font.body
      }
      Text {
        text: pill.level + "%"
        color: root.foreground
        font.family: root.fontFamily
        font.pixelSize: Style.font.body
        font.bold: true
      }
    }
  }

  // One row of the remap table. The width comes from the grid rather than being
  // hard-coded, so the action text gets whatever room the panel has and elides
  // instead of colliding with its neighbour.
  component TouchCell: Row {
    id: cell
    required property string slot
    required property string action
    property real cellWidth: 150
    width: cell.cellWidth
    spacing: Style.space(8)
    Text {
      text: cell.slot
      color: Qt.darker(root.foreground, 1.4)
      font.family: root.fontFamily
      font.pixelSize: Style.font.bodySmall
      font.bold: true
      font.letterSpacing: 0.4
      width: Style.space(56)
    }
    Text {
      text: cell.action
      color: root.foreground
      font.family: root.fontFamily
      font.pixelSize: Style.font.bodySmall
      width: Math.max(0, parent.width - Style.space(64))
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

  // One library curve. Clicking applies it; it is disabled (with the reason in
  // a small line) when it cannot be written or the active slot is protected.
  component CurveCard: BorderSurface {
    id: cc
    required property var entry
    property real cardWidth: 0
    property bool canApply: false
    property bool live: false
    // idle | running | ok | unconfirmed | error — this card's own apply state.
    property string phase: "idle"
    property bool confirmingDelete: false
    property bool deleteBlocked: false
    signal applyRequested()
    signal deleteAsked()
    signal deleteCancelled()
    signal deleteConfirmed()

    readonly property bool entryApplyable: cc.entry.applyable === true
    readonly property bool usable: cc.canApply && cc.entryApplyable
    readonly property bool isUser: cc.entry.source === "user"
    readonly property real master: Number(cc.entry.masterGain) || 0

    width: cc.cardWidth
    implicitHeight: ccBody.implicitHeight + Style.space(24)
    radius: Style.cornerRadius
    opacity: (cc.usable || cc.phase !== "idle" || cc.live) ? 1.0 : 0.55
    color: (cc.live || cc.phase === "ok")
      ? Style.selectedFillFor(root.foreground, root.accent)
      : ((ccMouse.containsMouse && cc.usable) ? Style.hoverFillFor(root.foreground, root.accent) : root.cardFill)
    borderSpec: Border.controlSpec((cc.live || cc.phase === "ok") ? "selected" : "normal", root.foreground, root.accent)
    Behavior on color { ColorAnimation { duration: 120 } }
    Behavior on opacity { NumberAnimation { duration: 120 } }

    MouseArea {
      id: ccMouse
      anchors.fill: parent
      enabled: cc.usable
      hoverEnabled: true
      cursorShape: cc.usable ? Qt.PointingHandCursor : Qt.ArrowCursor
      onClicked: cc.applyRequested()
    }

    Column {
      id: ccBody
      x: Style.space(12)
      y: Style.space(12)
      width: cc.width - Style.space(24)
      spacing: Style.space(6)

      Text {
        width: parent.width
        text: cc.entry.name
        color: root.foreground
        font.family: root.fontFamily
        font.pixelSize: Style.font.body
        font.bold: true
        elide: Text.ElideRight
      }

      Text {
        width: parent.width
        text: root.sourceLabel(cc.entry.source) + " · " + cc.entry.nBands + " bandas · "
          + (cc.master >= 0 ? "+" : "") + cc.master.toFixed(1) + " dB"
        color: Qt.darker(root.foreground, 1.5)
        font.family: root.fontFamily
        font.pixelSize: Style.font.caption
        elide: Text.ElideRight
      }

      EqCurve {
        width: parent.width
        bands: cc.entry.bands
        compact: true
      }

      Text {
        width: parent.width
        visible: text !== ""
        text: {
          if (cc.phase === "running") return "Aplicando…"
          if (cc.phase === "ok") return "Aplicado (confirmado)"
          if (cc.phase === "unconfirmed") return "Gravado, mas o aparelho não confirmou"
          if (cc.phase === "error") return "Falhou"
          if (!cc.entryApplyable) return root.reasonText(cc.entry.applyableReason)
          if (cc.live) return "Em uso"
          return ""
        }
        color: (cc.phase === "error" || cc.phase === "unconfirmed") ? Color.urgent : Qt.darker(root.foreground, 1.3)
        font.family: root.fontFamily
        font.pixelSize: Style.font.caption
        wrapMode: Text.WordWrap
      }

      Row {
        visible: cc.isUser && cc.phase !== "running"
        spacing: Style.space(6)

        Chip {
          visible: !cc.confirmingDelete
          opacity: cc.deleteBlocked ? 0.5 : 1.0
          label: "Excluir"
          onClicked: if (!cc.deleteBlocked) cc.deleteAsked()
        }
        Text {
          visible: cc.confirmingDelete
          anchors.verticalCenter: parent.verticalCenter
          text: "Excluir?"
          color: root.foreground
          font.family: root.fontFamily
          font.pixelSize: Style.font.bodySmall
        }
        Chip {
          visible: cc.confirmingDelete
          label: "Sim"
          onClicked: cc.deleteConfirmed()
        }
        Chip {
          visible: cc.confirmingDelete
          label: "Não"
          onClicked: cc.deleteCancelled()
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
