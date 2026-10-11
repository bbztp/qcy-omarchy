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
  // LDAC and multipoint are deliberately absent: writing their opcodes reboots
  // this N70, so the backend refuses them and there is no point offering a
  // switch that resets the earbuds. Both are still read back in the status
  // report, so a future verified opcode can restore them here.
  readonly property var toggleDefs: [
    { key: "game", label: "Game mode (low latency)" },
    { key: "sleep", label: "Sleep mode" },
    { key: "wind", label: "Wind noise reduction" }
  ]

  function batteryGlyph() {
    var idx = Math.max(0, Math.min(9, Math.floor(level * 10)))
    return charging ? _chargeGlyphs[idx] : _levelGlyphs[idx]
  }

  function heroStatus() {
    if (!Qcy.connected) return "NOT CONNECTED"
    if (Qcy.loading) return "READING…"
    if (rep && rep.firmware) return "FIRMWARE " + rep.firmware.left
    return "CONNECTED"
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
      { label: "Left", level: batt.left, charging: batt.leftCharging },
      { label: "Right", level: batt.right, charging: batt.rightCharging }
    ]
    if (batt.casePresent) rows.push({ label: "Case", level: batt.case, charging: batt.caseCharging })
    return rows
  }

  function fmtAutoOff(a) {
    if (!a) return "—"
    if (a.minutes === null || a.minutes === undefined) return "Never"
    return a.minutes + " min"
  }
  function fmtBalance(b) {
    if (b === undefined || b === null) return "—"
    if (b < 45) return "Left"
    if (b > 55) return "Right"
    return "Center"
  }
  function fmtPrompt(p) {
    if (!p) return "—"
    return p.value + (p.max ? " / " + p.max : "")
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

        // ---------- Hero ----------
        Item {
          width: parent.width
          implicitHeight: Math.max(heroIcon.implicitHeight, heroLabels.implicitHeight)

          Text {
            id: heroIcon
            text: Qcy.connected ? root.batteryGlyph() : root._earbudsGlyph
            color: root.foreground
            font.family: root.fontFamily
            font.pixelSize: Style.font.display
            opacity: Qcy.connected ? 1.0 : 0.5
          }

          Column {
            id: heroLabels
            anchors.left: heroIcon.right
            anchors.leftMargin: Style.space(14)
            anchors.right: parent.right
            anchors.verticalCenter: parent.verticalCenter
            spacing: Style.space(2)

            Text {
              text: "QCY Earbuds"
              color: root.foreground
              font.family: root.fontFamily
              font.pixelSize: Style.font.title
              font.bold: true
              elide: Text.ElideRight
              width: parent.width
            }
            Text {
              text: root.heroStatus()
              color: Qt.darker(root.foreground, 1.4)
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
              font.bold: true
              font.letterSpacing: 1.1
              elide: Text.ElideRight
              width: parent.width
            }
          }
        }

        // ---------- Battery ----------
        Column {
          visible: root.batteryRows().length > 0
          width: parent.width
          spacing: Style.space(8)

          PanelSectionHeader {
            text: "BATTERY"
            foreground: root.foreground
            fontFamily: root.fontFamily
          }

          Repeater {
            model: root.batteryRows()
            StatRow {
              required property var modelData
              width: column.width
              label: modelData.label
              value: modelData.level + "%" + (modelData.charging ? "  󰂄 charging" : "")
            }
          }
        }

        PanelSeparator {
          visible: root.batteryRows().length > 0
          foreground: root.foreground
        }

        // ---------- Noise control ----------
        Column {
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

        // ---------- Toggles ----------
        Column {
          width: parent.width
          spacing: Style.space(10)

          Repeater {
            model: root.toggleDefs
            Item {
              required property var modelData
              width: column.width
              implicitHeight: Math.max(toggleLabel.implicitHeight, switchControl.implicitHeight)

              Text {
                id: toggleLabel
                text: modelData.label
                color: root.foreground
                font.family: root.fontFamily
                font.pixelSize: Style.font.body
                anchors.left: parent.left
                anchors.verticalCenter: parent.verticalCenter
              }

              ToggleSwitch {
                id: switchControl
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

        // ---------- Readout ----------
        Column {
          visible: !!root.rep
          width: parent.width
          spacing: Style.space(6)

          PanelSeparator { foreground: root.foreground }

          StatRow {
            width: column.width
            label: "Auto power-off"
            value: root.fmtAutoOff(root.rep ? root.rep.autoOff : null)
          }
          StatRow {
            width: column.width
            label: "Balance"
            value: root.fmtBalance(root.rep ? root.rep.balance : null)
          }
          StatRow {
            width: column.width
            label: "Prompt volume"
            value: root.fmtPrompt(root.rep ? root.rep.promptVolume : null)
          }
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
}
