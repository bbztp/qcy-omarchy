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

  // ---- curve editor state ----
  // Ten bars at the slot 0 layout (the same table `library save` writes).
  readonly property var editorFreqs: [31, 62, 125, 250, 500, 1000, 2000, 4000, 8000, 16000]
  readonly property real editorMaxDb: 8
  property bool editorOpen: false
  property var editGains: [0, 0, 0, 0, 0, 0, 0, 0, 0, 0]
  property real editMaster: 0
  property string editorSource: ""
  // Non-empty: the bars are shown but nothing can be edited, saved or applied.
  property string editorBlocked: ""
  property string editorNote: ""
  // "Ouvir enquanto edita": each release applies the draft to the earbuds.
  property bool listenLive: false

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

  // Same rule as the backend Slug (qcycore/presets.go): the draft owns this id.
  function nameSlug(n) {
    var fold = { "á": "a", "à": "a", "â": "a", "ã": "a", "ä": "a",
                 "é": "e", "è": "e", "ê": "e", "ë": "e",
                 "í": "i", "ì": "i", "î": "i", "ï": "i",
                 "ó": "o", "ò": "o", "ô": "o", "õ": "o", "ö": "o",
                 "ú": "u", "ù": "u", "û": "u", "ü": "u", "ç": "c", "ñ": "n" }
    var s = String(n || "").trim().toLowerCase()
    var out = ""
    var dash = true
    for (var i = 0; i < s.length; i++) {
      var c = fold[s.charAt(i)] || s.charAt(i)
      if ((c >= "a" && c <= "z") || (c >= "0" && c <= "9")) {
        out += c
        dash = false
      } else if (!dash) {
        out += "-"
        dash = true
      }
    }
    return out.slice(0, 40).replace(/^-+|-+$/g, "")
  }

  function nameReserved(n) {
    return nameSlug(n) === Qcy.draftId
  }

  function nameValid(n) {
    var t = String(n || "").trim()
    return t.length > 0 && t.length <= 40 && t.charAt(0) !== "-" && !nameReserved(t)
  }

  function submitSave(n) {
    if (!nameValid(n) || !canSaveCurrent) return
    Qcy.libraryImportDevice(String(n).trim())
    root.closeSave()
  }

  // Closing the name field hands the keyboard back to the panel's key catcher.
  function closeSave() {
    root.saveOpen = false
    nameField.text = ""
    Qt.callLater(function () { keyCatcher.forceActiveFocus() })
  }

  // ---- curve editor ----

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

  // Needs the earbuds, a known non-protected active slot and an editable
  // curve. Deliberately does not look at libraryOp.phase: the draft request is
  // queued by Qcy (last one wins), so a release during an apply is not lost.
  readonly property bool editorHwOk: Qcy.connected && !!rep && activeSlot >= 0
    && !slotProtected && editorBlocked === ""
  readonly property bool editorCanSave: editorBlocked === "" && Qcy.libraryOp.phase !== "running"

  // bands: array of {index?, freq, gain, q?, type?}; isCard says it came from a
  // library entry (an empty list then means a raw payload with no bands).
  function openEditor(bands, master, label, isCard) {
    var g = [0, 0, 0, 0, 0, 0, 0, 0, 0, 0]
    var blocked = ""
    var substituted = false
    var list = bands ? bands : []
    if (list.length > 10) blocked = "Esta curva usa bandas nas posições 10–19; o editor só trabalha com as 10 primeiras e a gravação de 143 bytes não foi validada em hardware."
    if (isCard && list.length === 0) blocked = "Esta curva não tem bandas editáveis."
    for (var i = 0; i < list.length && blocked === ""; i++) {
      var b = list[i]
      var idx = (b.index !== undefined && b.index !== null) ? Number(b.index) : i
      if (idx >= 10) {
        blocked = "Esta curva usa bandas nas posições 10–19; o editor só trabalha com as 10 primeiras e a gravação de 143 bytes não foi validada em hardware."
        break
      }
      if (idx < 0) continue
      g[idx] = root.clampGain(b.gain)
      if (b.freq !== undefined && Number(b.freq) !== root.editorFreqs[idx]) substituted = true
      if (b.q !== undefined && b.q !== null && Number(b.q) !== 100) substituted = true
      if (b.type !== undefined && b.type !== null && Number(b.type) !== 0) substituted = true
    }
    var m = Number(master)
    if (isNaN(m)) m = 0
    listenTimer.stop()
    root.listenLive = false
    root.editGains = g
    root.editMaster = Math.max(root.masterGainMin, Math.min(root.masterGainMax, m))
    root.editorSource = label
    root.editorBlocked = blocked
    root.editorNote = substituted
      ? "As frequências e o Q da curva de origem serão trocados pela tabela padrão (31 Hz a 16 kHz) ao salvar ou aplicar."
      : ""
    root.editorOpen = true
  }

  function openEditorLive() {
    var b = root.eqBands()
    if (!root.rep || !b || b.length === 0) {
      root.openEditor([], 0, "Nova curva", false)
      return
    }
    root.openEditor(b, root.masterGainDb(), root.activeCurveName(), false)
  }

  function openEditorFromEntry(e) {
    root.openEditor(e.bands, Number(e.masterGain) || 0, e.name, true)
  }

  function closeEditor() {
    listenTimer.stop()
    root.listenLive = false
    root.editorOpen = false
    editNameField.text = ""
    Qt.callLater(function () { keyCatcher.forceActiveFocus() })
  }

  // Stops "listen" mode and any pending debounce (tab change, panel close).
  function stopListening() {
    // A release is still pending: send the last value instead of dropping it.
    if (listenTimer.running && root.editorHwOk) root.applyDraft()
    listenTimer.stop()
    root.listenLive = false
  }

  function editSetGain(i, v) {
    if (root.editorBlocked !== "") return
    var nv = Math.round(root.clampGain(v) * 2) / 2 + 0
    if (Number(root.editGains[i]) === nv) return
    var g = root.editGains.slice()
    g[i] = nv
    root.editGains = g
  }

  function editSetMaster(v) {
    if (root.editorBlocked !== "") return
    var n = Number(v)
    if (isNaN(n)) return
    root.editMaster = Math.round(Math.max(root.masterGainMin, Math.min(root.masterGainMax, n)) * 2) / 2 + 0
  }

  function editZeroAll() {
    if (root.editorBlocked !== "") return
    root.editGains = [0, 0, 0, 0, 0, 0, 0, 0, 0, 0]
    root.editMaster = 0
    root.scheduleListen()
  }

  // Called when a bar or the master slider is released.
  function scheduleListen() {
    if (root.listenLive && root.editorHwOk) listenTimer.restart()
  }

  // One draft write+apply, now (the "Aplicar" button and the debounce).
  function applyDraft() {
    listenTimer.stop()
    if (!root.editorHwOk) return
    Qcy.libraryDraftApply(root.editGainsCsv(), root.editMaster)
  }

  function submitEditorSave(n) {
    if (!nameValid(n) || !root.editorCanSave) return
    Qcy.libraryCreate(String(n).trim(), root.editGainsCsv(), root.editMaster, function (ok) {
      if (ok) editNameField.text = ""
    })
    Qt.callLater(function () { keyCatcher.forceActiveFocus() })
  }

  // The editor shows the result of its own operations (draft apply, save).
  function editorShowsOp() {
    var op = Qcy.libraryOp
    return root.editorOpen && op.phase !== "idle"
      && (op.id === Qcy.draftId || op.kind === "save")
  }

  Timer {
    id: listenTimer
    interval: 400
    repeat: false
    onTriggered: root.applyDraft()
  }

  onEditorHwOkChanged: if (!root.editorHwOk) listenTimer.stop()

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
      if (op.phase === "running") return "Salvando a curva…"
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
    root.stopListening()
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
    return slotLabel()
  }

  // "Efeito (slot N)" when the backend names the slot (eq.effect), else "Slot N".
  function slotLabel() {
    if (!rep || !rep.eq || rep.eq.preset === null || rep.eq.preset === undefined) return "—"
    var p = rep.eq.preset
    var eff = rep.eq.effect
    if (eff !== undefined && eff !== null && eff !== "") return eff + " (slot " + p + ")"
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
    0x00: "Desativado", 0x01: "Tocar/pausar", 0x02: "Anterior", 0x03: "Próxima",
    0x04: "Assistente de voz", 0x05: "Volume +", 0x06: "Volume −",
    0x07: "Modo jogo", 0x08: "Atender", 0x09: "Rejeitar",
    0x0a: "Reter chamada", 0x0b: "Rediscar"
  })

  readonly property var keyNames: ({
    0x01: "Esq. toque", 0x02: "Dir. toque", 0x03: "Esq. duplo", 0x04: "Dir. duplo",
    0x05: "Esq. triplo", 0x06: "Dir. triplo", 0x07: "Esq. quádruplo", 0x08: "Dir. quádruplo",
    0x09: "Esq. segurar", 0x0a: "Dir. segurar",
    0x15: "Esq. toque (chamada)", 0x16: "Dir. toque (chamada)", 0x17: "Esq. duplo (chamada)",
    0x18: "Dir. duplo (chamada)", 0x19: "Esq. triplo (chamada)", 0x1a: "Dir. triplo (chamada)",
    0x1b: "Esq. quádruplo (chamada)", 0x1c: "Dir. quádruplo (chamada)",
    0x1d: "Esq. segurar (chamada)", 0x1e: "Dir. segurar (chamada)"
  })

  readonly property var gestureNames: ["Toque", "Duplo", "Triplo", "Quádruplo", "Segurar"]

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
        byKey[key] = { gesture: root.gestureNames[gi] + (call ? " (chamada)" : ""),
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
    if (!opened) root.stopListening()
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

            // Three charge rings: left, right, case.
            Row {
              id: ringRow
              width: parent.width
              spacing: Style.space(8)

              BatteryRing {
                width: (ringRow.width - Style.space(8) * 2) / 3
                label: "Esq."
                valid: Qcy.connected && !!root.batt
                level: root.batt ? root.batt.left : 0
                charging: root.batt ? root.batt.leftCharging : false
              }
              BatteryRing {
                width: (ringRow.width - Style.space(8) * 2) / 3
                label: "Dir."
                valid: Qcy.connected && !!root.batt
                level: root.batt ? root.batt.right : 0
                charging: root.batt ? root.batt.rightCharging : false
              }
              BatteryRing {
                width: (ringRow.width - Style.space(8) * 2) / 3
                label: "Estojo"
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
                implicitHeight: liveTitle.implicitHeight + Style.space(6)

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

            // ---- Protected slot warning: one quiet strip, not repeated per curve ----
            BorderSurface {
              width: parent.width
              visible: root.slotProtected
              implicitHeight: warnRow.implicitHeight + Style.space(20)
              radius: Style.cornerRadius
              color: "transparent"
              borderSpec: Border.controlSpec("selected", root.foreground, root.accent)

              Row {
                id: warnRow
                x: Style.space(12)
                y: Style.space(10)
                width: parent.width - Style.space(24)
                spacing: Style.space(10)

                Text {
                  id: warnIcon
                  text: String.fromCodePoint(0xF033E)
                  color: root.accent
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.title
                }
                Text {
                  width: parent.width - warnIcon.implicitWidth - Style.space(10)
                  text: "Efeito de fábrica ativo: " + root.slotLabel() + ". "
                    + "Para não sobrescrevê-lo, a biblioteca não aplica curvas enquanto o slot 6 ou 11 estiver ativo."
                  color: Qt.darker(root.foreground, 1.15)
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.bodySmall
                  wrapMode: Text.WordWrap
                }
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

              Column {
                id: libList
                width: parent.width
                spacing: Style.space(8)

                Repeater {
                  model: Qcy.library
                  CurveCard {
                    required property var modelData
                    entry: modelData
                    cardWidth: libList.width
                    slotProtected: root.slotProtected
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
                    onEditRequested: {
                      root.pendingDeleteId = ""
                      root.openEditorFromEntry(modelData)
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
                visible: root.opText() !== "" && !root.editorShowsOp()
                text: root.opText()
                color: Qcy.libraryOp.phase === "error" ? Color.urgent : root.foreground
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
                wrapMode: Text.WrapAnywhere
              }
            }

            // ---- Edit / save, side by side ----
            Row {
              width: parent.width
              spacing: Style.space(8)

              Chip {
                width: (parent.width - Style.space(8)) / 2
                label: "Editar / Nova curva"
                active: root.editorOpen
                onClicked: {
                  if (root.editorOpen) root.closeEditor()
                  else root.openEditorLive()
                }
              }
              Chip {
                width: (parent.width - Style.space(8)) / 2
                opacity: root.canSaveCurrent ? 1.0 : 0.5
                label: "Salvar curva atual"
                active: root.saveOpen
                onClicked: {
                  if (!root.canSaveCurrent) return
                  if (root.saveOpen) { root.closeSave(); return }
                  root.saveOpen = true
                  nameField.forceActiveFocus()
                }
              }
            }

            // ---- Curve editor ----
            Card {
              width: parent.width
              visible: root.editorOpen

              Item {
                width: parent.width
                implicitHeight: edTitle.implicitHeight

                Text {
                  id: edTitle
                  text: "EDITOR DE CURVA"
                  color: Qt.darker(root.foreground, 1.35)
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.caption
                  font.bold: true
                  font.letterSpacing: 1.1
                  anchors.left: parent.left
                  anchors.verticalCenter: parent.verticalCenter
                }
                Text {
                  text: root.editorSource
                  color: root.foreground
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.body
                  font.bold: true
                  anchors.left: edTitle.right
                  anchors.leftMargin: Style.space(10)
                  anchors.right: parent.right
                  anchors.verticalCenter: parent.verticalCenter
                  elide: Text.ElideRight
                  horizontalAlignment: Text.AlignRight
                }
              }

              Text {
                width: parent.width
                visible: root.editorBlocked !== ""
                text: root.editorBlocked
                color: Color.urgent
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
                wrapMode: Text.WordWrap
              }

              Text {
                width: parent.width
                visible: root.slotProtected
                text: root.slotLabel() + " (protegido) está ativo: você pode editar e salvar, mas não ouvir nem aplicar."
                color: Color.urgent
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
                wrapMode: Text.WordWrap
              }

              Text {
                width: parent.width
                visible: root.editorNote !== ""
                text: root.editorNote
                color: Qt.darker(root.foreground, 1.5)
                font.family: root.fontFamily
                font.pixelSize: Style.font.caption
                wrapMode: Text.WordWrap
              }

              BarEditor {
                width: parent.width
                gains: root.editGains
                editable: root.editorBlocked === ""
                onEdited: function (i, v) { root.editSetGain(i, v) }
                onCommitted: root.scheduleListen()
              }

              Text {
                width: parent.width
                text: "Arraste uma barra (±8 dB, passos de 0,5 dB). Duplo clique zera a barra."
                color: Qt.darker(root.foreground, 1.5)
                font.family: root.fontFamily
                font.pixelSize: Style.font.caption
                wrapMode: Text.WordWrap
              }

              SliderRow {
                width: parent.width
                enabled: root.editorBlocked === ""
                opacity: root.editorBlocked === "" ? 1.0 : 0.5
                label: "Ganho geral"
                minimum: root.masterGainMin
                maximum: root.masterGainMax
                step: 0.5
                integer: false
                value: root.editMaster
                suffix: " dB"
                onMoved: function (v) { root.editSetMaster(v) }
                onReleased: function (v) {
                  root.editSetMaster(v)
                  root.scheduleListen()
                }
              }

              ToggleRow {
                width: parent.width
                opacity: root.editorHwOk ? 1.0 : 0.5
                label: "Ouvir enquanto edita"
                hint: "Ao soltar uma barra, aplica o rascunho nos fones (sobrescreve o slot 0)"
                checked: root.listenLive
                onToggled: {
                  if (root.listenLive) root.stopListening()
                  else if (root.editorHwOk) root.listenLive = true
                }
              }

              Flow {
                width: parent.width
                spacing: Style.space(8)

                Chip {
                  visible: !root.listenLive
                  opacity: root.editorHwOk && Qcy.libraryOp.phase !== "running" ? 1.0 : 0.5
                  label: "Aplicar"
                  onClicked: {
                    if (root.editorHwOk && Qcy.libraryOp.phase !== "running") root.applyDraft()
                  }
                }
                Chip {
                  opacity: root.editorBlocked === "" ? 1.0 : 0.5
                  label: "Zerar tudo"
                  onClicked: root.editZeroAll()
                }
                Chip {
                  label: "Fechar"
                  onClicked: root.closeEditor()
                }
              }

              Text {
                width: parent.width
                visible: root.editorShowsOp()
                text: root.opText()
                color: Qcy.libraryOp.phase === "error" ? Color.urgent : root.foreground
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
                wrapMode: Text.WrapAnywhere
              }

              TextField {
                id: editNameField
                width: parent.width
                enabled: root.editorBlocked === ""
                placeholderText: "Nome da nova curva"
                maximumLength: 40
                foreground: root.foreground
                accent: root.accent
                onAccepted: root.submitEditorSave(text)
                Keys.onEscapePressed: {
                  text = ""
                  keyCatcher.forceActiveFocus()
                }
              }

              Text {
                width: parent.width
                visible: root.nameReserved(editNameField.text)
                text: "Nome reservado para o rascunho"
                color: Color.urgent
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
                wrapMode: Text.WordWrap
              }

              Chip {
                label: "Salvar na biblioteca"
                active: root.nameValid(editNameField.text) && root.editorCanSave
                opacity: root.editorCanSave ? 1.0 : 0.5
                onClicked: root.submitEditorSave(editNameField.text)
              }
            }

            // ---- Save current curve ----
            Column {
              width: parent.width
              spacing: Style.space(8)

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
                  Keys.onEscapePressed: root.closeSave()
                }

                Text {
                  width: parent.width
                  visible: root.nameReserved(nameField.text)
                  text: "Nome reservado para o rascunho"
                  color: Color.urgent
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.bodySmall
                  wrapMode: Text.WordWrap
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
                    onClicked: root.closeSave()
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

              TouchCell {
                width: parent.width
                header: true
                gesture: ""
                leftText: "ESQUERDO"
                rightText: "DIREITO"
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
    // Mini version for library cards: no frequency labels, shorter plot.
    property bool compact: false

    readonly property real maxGain: 10
    // The plot stops short of the bottom so the band labels have room INSIDE
    // this item. They used to be anchored to the delegate's bottom edge, which
    // put them ~2px past `height` and on top of whatever followed.
    readonly property real plotHeight: Style.spacing.controlHeight * (curve.compact ? 1.1 : 1.6)
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

  // One library curve, one wide row: name and a small info line on the left, a
  // mini curve and a compact "Editar" on the right. Clicking the row applies it;
  // it is dimmed (and says why, in a small line) when it cannot be written.
  // The protected-slot warning lives in one strip above the list, not here.
  component CurveCard: BorderSurface {
    id: cc
    required property var entry
    property real cardWidth: 0
    property bool canApply: false
    property bool slotProtected: false
    property bool live: false
    // idle | running | ok | unconfirmed | error — this card's own apply state.
    property string phase: "idle"
    property bool confirmingDelete: false
    property bool deleteBlocked: false
    signal applyRequested()
    signal editRequested()
    signal deleteAsked()
    signal deleteCancelled()
    signal deleteConfirmed()

    readonly property bool entryApplyable: cc.entry.applyable === true
    readonly property bool usable: cc.canApply && cc.entryApplyable
    readonly property bool isUser: cc.entry.source === "user"
    readonly property real master: Number(cc.entry.masterGain) || 0
    readonly property real sideWidth: Style.space(110)

    width: cc.cardWidth
    implicitHeight: Math.max(textCol.implicitHeight, sideCol.implicitHeight) + Style.space(24)
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
      id: textCol
      x: Style.space(12)
      y: Style.space(12)
      width: cc.width - Style.space(24) - cc.sideWidth - Style.space(10)
      spacing: Style.space(4)

      Text {
        width: parent.width
        text: cc.entry.name
        color: root.foreground
        font.family: root.fontFamily
        font.pixelSize: Style.font.body
        font.bold: true
        wrapMode: Text.WordWrap
        maximumLineCount: 2
        elide: Text.ElideRight
      }

      Text {
        width: parent.width
        text: (cc.entry.id === "rascunho" ? "RASCUNHO · " : "")
          + root.sourceLabel(cc.entry.source) + " · " + cc.entry.nBands + " bandas · "
          + (cc.master >= 0 ? "+" : "") + cc.master.toFixed(1) + " dB"
        color: Qt.darker(root.foreground, 1.5)
        font.family: root.fontFamily
        font.pixelSize: Style.font.caption
        elide: Text.ElideRight
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
        visible: cc.phase !== "running" && (cc.isUser || cc.confirmingDelete)
        spacing: Style.space(6)

        Chip {
          visible: cc.isUser && !cc.confirmingDelete
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

    Column {
      id: sideCol
      x: cc.width - Style.space(12) - cc.sideWidth
      y: Style.space(12)
      width: cc.sideWidth
      spacing: Style.space(6)

      EqCurve {
        width: parent.width
        bands: cc.entry.bands
        compact: true
      }

      Chip {
        visible: cc.phase !== "running"
        width: parent.width
        label: "Editar"
        onClicked: cc.editRequested()
      }
    }
  }

  // Ten draggable vertical bars (positions 0..9, slot 0 frequency table). The
  // gain is above each bar, the frequency below. It owns no state: it reports
  // edited(index, dB) while dragging and committed() on release.
  component BarEditor: Item {
    id: be
    required property var gains
    property bool editable: true
    signal edited(int index, real value)
    signal committed()

    readonly property real plotHeight: Style.spacing.controlHeight * 3
    readonly property real valueHeight: Math.ceil(Style.font.caption) + Style.space(4)
    readonly property real labelHeight: Math.ceil(Style.font.caption) + Style.space(4)
    readonly property real slotWidth: be.width / 10

    implicitHeight: be.valueHeight + be.plotHeight + be.labelHeight
    opacity: be.editable ? 1.0 : 0.5

    Repeater {
      model: 10
      Item {
        id: slot
        required property int index
        readonly property real gainDb: Number(be.gains[slot.index]) || 0
        readonly property int hz: root.editorFreqs[slot.index]

        x: slot.index * be.slotWidth
        width: be.slotWidth
        height: be.implicitHeight

        Text {
          width: parent.width
          height: be.valueHeight
          text: (slot.gainDb > 0 ? "+" : "") + slot.gainDb.toFixed(1)
          color: slot.gainDb === 0 ? Qt.darker(root.foreground, 1.6) : root.foreground
          font.family: root.fontFamily
          font.pixelSize: Style.font.caption * 0.85
          horizontalAlignment: Text.AlignHCenter
          verticalAlignment: Text.AlignVCenter
        }

        Rectangle {
          id: plot
          y: be.valueHeight
          width: parent.width - Style.space(4)
          anchors.horizontalCenter: parent.horizontalCenter
          height: be.plotHeight
          radius: 2
          color: root.cardFill

          Rectangle {
            anchors.left: parent.left
            anchors.right: parent.right
            anchors.verticalCenter: parent.verticalCenter
            height: 1
            color: Qt.darker(root.foreground, 1.9)
          }

          Rectangle {
            readonly property real barHeight: Math.abs(slot.gainDb) / root.editorMaxDb * (plot.height / 2)
            anchors.horizontalCenter: parent.horizontalCenter
            width: Math.max(2, parent.width - Style.space(8))
            height: Math.max(1, barHeight)
            y: slot.gainDb >= 0 ? plot.height / 2 - height : plot.height / 2
            radius: 2
            color: slot.gainDb >= 0 ? root.accent : Qt.darker(root.foreground, 1.5)
          }

          MouseArea {
            id: dragArea
            anchors.fill: parent
            enabled: be.editable
            preventStealing: true
            cursorShape: be.editable ? Qt.SizeVerCursor : Qt.ArrowCursor

            function setFrom(y) {
              var v = (plot.height / 2 - y) / (plot.height / 2) * root.editorMaxDb
              be.edited(slot.index, Math.max(-root.editorMaxDb, Math.min(root.editorMaxDb, Math.round(v * 2) / 2)))
            }
            onPressed: function (mouse) { dragArea.setFrom(mouse.y) }
            onPositionChanged: function (mouse) { if (dragArea.pressed) dragArea.setFrom(mouse.y) }
            onReleased: be.committed()
            onCanceled: be.committed()
            onDoubleClicked: {
              be.edited(slot.index, 0)
              be.committed()
            }
          }
        }

        Text {
          width: parent.width
          y: be.valueHeight + be.plotHeight + Style.space(2)
          text: slot.hz >= 1000
            ? ((slot.hz % 1000 === 0 ? (slot.hz / 1000) : (slot.hz / 1000).toFixed(1)) + "k")
            : String(slot.hz)
          color: Qt.darker(root.foreground, 1.75)
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
