import QtQuick
import Qt5Compat.GraphicalEffects
import Quickshell.Io
import qs.Commons
import qs.Ui

// QCY earbuds widget: the QCY wordmark in the bar and a popup with active
// noise control, multipoint/LDAC/game toggles, and a readout of
// the earbuds' state (firmware, per-ear battery, EQ curve, balance, auto-off).
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
  readonly property var _chargeGlyphs: ["󰢜", "󰂆", "󰂇", "󰂈", "󰢝", "󰂉", "󰢞", "󰂊", "󰂋", "󰂅"]
  readonly property var _levelGlyphs: ["󰁺", "󰁻", "󰁼", "󰁽", "󰁾", "󰁿", "󰂀", "󰂁", "󰂂", "󰁹"]
  // Shown while the earbuds are disconnected (a known-good Nerd Font glyph).
  readonly property string _earbudsGlyph: "󰂲"

  readonly property real level: {
    if (Qcy.bluezBattery >= 0) return Qcy.bluezBattery
    if (batt) return Math.min(batt.left, batt.right) / 100
    return 0
  }
  readonly property bool charging: batt ? (batt.leftCharging || batt.rightCharging) : false

  readonly property var ancModes: [
    { key: "off", label: "Off" },
    { key: "adaptive", label: "Adaptive" },
    { key: "indoor", label: "Indoor" },
    { key: "commuting", label: "Commuting" },
    { key: "noisy", label: "Noisy" },
    { key: "antiwind", label: "Anti-wind" },
    { key: "transparency", label: "Transparency" }
  ]
  // LDAC is deliberately absent: writing 0x23 reboots this N70 and, unlike
  // multipoint below, the restart is not documented behaviour. Multipoint IS
  // listed because the N70 product notes state the earphones restart every time
  // Dual Device Connection is toggled — that is expected, not a fault.
  readonly property var toggleDefs: [
    { key: "game", label: "Game mode" },
    { key: "sleep", label: "Sleep mode" },
    { key: "wind", label: "Wind detection" },
    { key: "multipoint", label: "Multipoint" }
  ]

  // Notes shown once at the bottom rather than padded into the switch labels,
  // where they blew the two-column layout out.
  readonly property var notes:
    "Multipoint restarts the earbuds — that is expected, not a fault.\n"
    + "Sleep mode disables the touch controls; it has no audible effect."

  function levelGlyph(pct) {
    var idx = Math.max(0, Math.min(9, Math.floor((pct / 100) * 10)))
    return _levelGlyphs[idx]
  }

  function batteryGlyph() {
    var idx = Math.max(0, Math.min(9, Math.floor(level * 10)))
    return charging ? _chargeGlyphs[idx] : _levelGlyphs[idx]
  }

  function heroStatus() {
    if (!Qcy.connected) return "NOT CONNECTED"
    if (Qcy.loading) return "READING…"
    if (rep && rep.firmware && rep.firmware.left) return "FIRMWARE " + rep.firmware.left
    return "CONNECTED"
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

  function updatedLabel() {
    var tick = root.nowTick // binding dependency for the "ago" clock
    if (Qcy.error !== "") return Qcy.error
    if (Qcy.loading) return "Reading earbuds…"
    if (!Qcy.updatedAt) return "Open to read the earbuds' state"
    var s = Math.round((Date.now() - Qcy.updatedAt) / 1000)
    if (s < 5) return "Updated just now"
    if (s < 60) return "Updated " + s + "s ago"
    return "Updated " + Math.round(s / 60) + "m ago"
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
    { label: "Never", value: "never" },
    { label: "15m", value: "15" },
    { label: "30m", value: "30" },
    { label: "60m", value: "60" },
    { label: "90m", value: "90" }
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

  function touchRows() {
    var k = rep ? rep.keys : null
    if (!k || !k.pairs) return []
    var rows = []
    for (var i = 0; i < k.pairs.length; i++) {
      var bits = k.pairs[i].split(":")
      var kid = parseInt(bits[0], 16)
      var fid = parseInt(bits[1], 16)
      rows.push({
        slot: root.keyNames[kid] || ("0x" + bits[0]),
        action: root.funcNames[fid] || ("0x" + bits[1])
      })
    }
    return rows
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
    Qcy.set("Auto power-off", ["auto-off", value])
  }

  Component.onCompleted: Qcy.settings = settings
  onSettingsChanged: Qcy.settings = settings

  // Opening the panel is the display trigger: read battery lightly now and keep
  // it fresh while open; full status re-runs only occasionally.
  onOpenedChanged: {
    if (opened) {
      if (Qcy.connected) Qcy.pollBattery()
      batteryTimer.running = opened
      statusTimer.running = opened
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
    tooltipText: "QCY Earbuds — left: panel · right: re-read"
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
    contentWidth: panel.fittedContentWidth(Style.space(380))
    contentHeight: panel.fittedContentHeight(column.implicitHeight)

    PanelKeyCatcher {
      id: keyCatcher
      anchors.fill: parent
      onCloseRequested: root.close()
      onTextKey: function (t) {
        if (t === "r" || t === "R") Qcy.refresh()
      }

      Column {
        id: column
        anchors.fill: parent
        spacing: Style.space(14)

        // ---------- Tabs ----------
        // A single scrolling column was taller than the panel, so everything
        // below the fold — including the touch-control map — was silently cut
        // off. Splitting the same content into three short tabs keeps every
        // section reachable and makes the panel read horizontally, the way the
        // earbuds' own app is organised.
        Row {
          id: tabBar
          width: parent.width
          spacing: Style.space(6)

          Repeater {
            model: [
                { label: "Device", index: 0 },
                { label: "Sound", index: 1 },
                { label: "Settings", index: 2 }
              ]

              component TabButton: BorderSurface {
                id: tabBtn
                required property string label
                required property int index
                readonly property bool active: root.tab === tabBtn.index

                width: (tabBar.width - Style.space(6) * 2) / 3
                implicitHeight: Style.spacing.controlHeight
                radius: Style.cornerRadius
                color: tabBtn.active
                  ? Style.selectedFillFor(root.foreground, root.accent)
                  : "transparent"
                borderSpec: Border.controlSpec(
                  tabBtn.active ? "selected" : "normal", root.foreground, root.accent)

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
            }
        }

        // ---------- Hero ----------
        // Earbuds in the middle with their charge either side, the way the
        // earbuds' own app presents the device screen. Reading the panel should
        // not mean parsing a column of text lines.
        Column {
          // Tab 0: only this section shows on the active tab.
          visible: root.tab === 0
          width: parent.width
          spacing: Style.space(10)

          Item {
            width: parent.width
            implicitHeight: Math.max(
              Math.max(heroArt.height, heroPillLeft.height),
              heroPillRight.height)

            // A compact charge pill: glyph plus percentage.
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

          Row {
            anchors.horizontalCenter: parent.horizontalCenter
            spacing: Style.space(8)

            Text {
              text: root.modelName
              color: root.foreground
              font.family: root.fontFamily
              font.pixelSize: Style.font.title
              font.bold: true
              anchors.verticalCenter: parent.verticalCenter
            }
            Text {
              text: "·"
              color: Qt.darker(root.foreground, 1.8)
              font.family: root.fontFamily
              font.pixelSize: Style.font.title
              anchors.verticalCenter: parent.verticalCenter
            }
            Text {
              text: root.heroStatus()
              color: Qt.darker(root.foreground, 1.4)
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
              font.bold: true
              font.letterSpacing: 0.8
              anchors.verticalCenter: parent.verticalCenter
            }
          }
        }

// ---------- Case battery ----------
        // Left and right are shown beside the earbuds in the hero; only the
        // case needs its own row, and it is often the one people forget.
        BatteryGauge {
          // Tab 0: only this section shows on the active tab.
          visible: root.tab === 0 && !!(root.batt && root.batt.casePresent)
          width: parent.width
          label: "Case"
          level: root.batt ? root.batt.case : 0
          charging: root.batt ? root.batt.caseCharging : false
        }

        PanelSeparator {
          visible: !!(root.batt && root.batt.casePresent)
          foreground: root.foreground
        }

        // ---------- Noise control ----------
        Column {
          // Tab 0: only this section shows on the active tab.
          visible: root.tab === 0
          width: parent.width
          spacing: Style.space(8)

          PanelSectionHeader {
            text: "NOISE CONTROL"
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
        }

        PanelSeparator { foreground: root.foreground }

        // ---------- Modes ----------
        // Two columns: a single stack of label+switch rows is what made the
        // panel feel like a wall of text. Short labels keep each cell readable.
        Grid {
          // Tab 0: only this section shows on the active tab.
          visible: root.tab === 0
          width: parent.width
          columns: 2
          columnSpacing: Style.space(18)
          rowSpacing: Style.space(12)

          Repeater {
            model: root.toggleDefs
            Item {
              required property var modelData
              // Grid sizes cells for us; taking half the grid width keeps the
              // switch column aligned across both columns.
              width: (parent.width - Style.space(18)) / 2
              implicitHeight: Math.max(modeLabel.implicitHeight, modeSwitch.implicitHeight)

              Text {
                id: modeLabel
                text: modelData.label
                color: root.foreground
                font.family: root.fontFamily
                font.pixelSize: Style.font.body
                anchors.left: parent.left
                anchors.right: modeSwitch.left
                anchors.rightMargin: Style.space(10)
                anchors.verticalCenter: parent.verticalCenter
                elide: Text.ElideRight
              }

              ToggleSwitch {
                id: modeSwitch
                anchors.right: parent.right
                anchors.verticalCenter: parent.verticalCenter
                checked: root.toggleState(modelData.key)
                // Deliberately not bound to `busy`. ToggleSwitch swallows
                // clicks while busy, and Qcy.loading is true for every
                // background read the panel runs — so binding it here made the
                // switches dead most of the time while the ANC chips (which
                // have no such guard) kept working. Qcy.set() queues rather
                // than drops, so overlapping clicks are safe.
                foreground: root.foreground
                accent: root.accent
                onToggled: {
                  root.setToggle(modelData.key, modelData.label, !root.toggleState(modelData.key))
                }
              }
            }
          }
        }

        PanelSeparator { foreground: root.foreground }

        // ---------- Controls ----------
        Column {
          // Tab 1: only this section shows on the active tab.
          visible: root.tab === 1 && !!root.rep
          width: parent.width
          spacing: Style.space(14)

          PanelSectionHeader {
            text: "CONTROLS"
            foreground: root.foreground
            fontFamily: root.fontFamily
          }

          ChoiceRow {
            width: column.width
            label: "Auto power-off"
            model: root.autoOffChoices
            current: root.currentAutoOff()
            onPicked: function (v) { root.setAutoOff(v) }
          }

          SliderRow {
            width: column.width
            label: "Balance"
            minimum: 0
            maximum: 100
            step: 1
            suffix: "%"
            value: root.sliderValue("balance", 50)
            onReleased: function (v) { root.commitSlider("balance", "Balance", function (n) { return ["balance", String(Math.round(n))] }, Math.round(v)) }
          }

          // The device reports its prompt volume as 0..max; the slider follows
          // that range instead of the CLI's 0-255, which this firmware ignores.
          SliderRow {
            width: column.width
            label: "Prompt volume"
            minimum: 0
            maximum: root.promptMax()
            step: 1
            value: root.sliderValue("promptVolume", 6)
            onReleased: function (v) { root.commitSlider("promptVolume", "Prompt volume", function (n) { return ["prompt-vol", String(Math.round(n))] }, Math.round(v)) }
          }
        }

        PanelSeparator {
          visible: !!root.rep
          foreground: root.foreground
        }

        // ---------- Readout ----------
        Column {
          // Tab 1: only this section shows on the active tab.
          visible: root.tab === 1 && !!root.rep
          width: parent.width
          spacing: Style.space(6)

          PanelSeparator { foreground: root.foreground }

          StatRow {
            width: column.width
            label: "EQ preset"
            value: root.rep && root.rep.eq ? ("#" + root.rep.eq.preset) : "—"
          }

          // EQ curve as a small read-only bar chart (±8 dB around the midline).
          Item {
            visible: !!(root.rep && root.rep.eq && root.rep.eq.bands && root.rep.eq.bands.length > 0)
            width: parent.width
            implicitHeight: eqRow.implicitHeight

            Row {
              id: eqRow
              anchors.right: parent.right
              spacing: Style.space(6)

              Repeater {
                model: root.rep && root.rep.eq ? root.rep.eq.bands : []
                Column {
                  required property var modelData
                  spacing: Style.space(3)

                  Item {
                    width: Style.space(12)
                    height: Style.space(38)
                    anchors.horizontalCenter: parent.horizontalCenter

                    Rectangle {
                      anchors.horizontalCenter: parent.horizontalCenter
                      width: parent.width
                      height: Math.max(1, Math.abs(modelData.gain) / 8 * (parent.height / 2))
                      y: modelData.gain >= 0 ? parent.height / 2 - height : parent.height / 2
                      color: root.accent
                      opacity: 0.85
                    }
                    Rectangle {
                      anchors.horizontalCenter: parent.horizontalCenter
                      anchors.verticalCenter: parent.verticalCenter
                      width: parent.width
                      height: 1
                      color: Qt.darker(root.foreground, 1.8)
                    }
                  }

                  Text {
                    anchors.horizontalCenter: parent.horizontalCenter
                    text: modelData.freq >= 1000
                      ? (modelData.freq % 1000 === 0 ? (modelData.freq / 1000) + "k" : (modelData.freq / 1000).toFixed(1) + "k")
                      : String(modelData.freq)
                    color: Qt.darker(root.foreground, 1.5)
                    font.family: root.fontFamily
                    font.pixelSize: Style.font.caption
                  }
                }
              }
            }
          }
        }

        // ---------- Touch controls ----------
        // The remap table as read back from characteristic 000D. Read-only for
        // now: a full gesture editor needs the function-id vocabulary, which
        // is device-side and undocumented.
        Column {
          // Tab 2: only this section shows on the active tab.
          visible: root.tab === 2 && root.touchRows().length > 0
          width: parent.width
          spacing: Style.space(8)

          PanelSectionHeader {
            text: "TOUCH CONTROLS"
            foreground: root.foreground
            fontFamily: root.fontFamily
          }

          // Column-major via Grid: each cell is an independent row that sizes
          // itself, so the action text can elide instead of spilling over its
          // neighbour. The previous version hard-coded the cell width, which is
          // what made the labels overlap.
          Grid {
            width: parent.width
            columns: 2
            columnSpacing: Style.space(20)
            rowSpacing: Style.space(7)

            Repeater {
              model: root.touchRows()

              component TouchCell: Row {
                id: cell
                required property string slot
                required property string action

                width: 150
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
                  width: parent.width - Style.space(64)
                  elide: Text.ElideRight
                }
              }
            }
          }
        }

        PanelSeparator {
          visible: root.touchRows().length > 0
          foreground: root.foreground
        }

// ---------- Device info ----------
        // Firmware and model, plus the caveats that answer "why is there no
        // sound". Both live on the Settings tab, which is where a list of
        // static facts belongs.
        Column {
          visible: root.tab === 2
          width: parent.width
          spacing: Style.space(8)

          PanelSectionHeader {
            text: "DEVICE"
            foreground: root.foreground
            fontFamily: root.fontFamily
          }

          StatRow {
            width: column.width
            label: "Model"
            value: root.modelName
          }
          StatRow {
            width: column.width
            label: "Firmware"
            value: root.rep && root.rep.firmware && root.rep.firmware.left
              ? root.rep.firmware.left : "—"
          }
          StatRow {
            width: column.width
            label: "EQ preset"
            value: root.rep && root.rep.eq ? ("#" + root.rep.eq.preset) : "—"
          }
        }

        PanelSeparator {
          visible: root.tab === 2
          foreground: root.foreground
        }

        // ---------- Actions ----------
        Row {
          spacing: Style.space(8)

          Chip {
            label: Qcy.loading ? "Reading…" : "Refresh"
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

        // Behaviour notes, so the "why is there no sound" questions have an
        // answer in the panel rather than only in the commit log.
        Text {
          text: root.notes
          color: Qt.darker(root.foreground, 1.7)
          font.family: root.fontFamily
          font.pixelSize: Style.font.caption
          font.italic: true
          wrapMode: Text.WordWrap
          lineHeight: 1.25
          width: parent.width
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

  // A label on the left, a value on the right.
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
      anchors.verticalCenter: parent.verticalCenter
    }
    Text {
      id: statValue
      text: stat.value
      color: root.foreground
      font.family: root.fontFamily
      font.pixelSize: Style.font.body
      anchors.right: parent.right
      anchors.verticalCenter: parent.verticalCenter
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

    implicitHeight: gaugeTrack.height
    visible: gauge.present

    Text {
      id: gaugeLabel
      text: gauge.label
      color: Qt.darker(root.foreground, 1.3)
      font.family: root.fontFamily
      font.pixelSize: Style.font.bodySmall
      width: Style.space(44)
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

  // A horizontal set of mutually exclusive choices.
  component ChoiceRow: Item {
    id: crow
    required property string label
    property var model: []
    required property string current
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
          active: crow.current === modelData.value
          onClicked: crow.picked(modelData.value)
        }
      }
    }
  }
}
