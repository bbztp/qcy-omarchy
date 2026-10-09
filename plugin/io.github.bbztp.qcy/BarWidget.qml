import QtQuick
import Quickshell.Io
import qs.Commons
import qs.Ui

// QCY earbuds widget: a battery button in the bar and a popup with active
// noise control, multipoint/LDAC/game toggles, find-my-buds, and a readout of
// the earbuds' state (firmware, per-ear battery, EQ curve, balance, auto-off).
//
// Bar button — left click: panel · right click: toggle percentage · middle click: refresh
// Panel — Esc: close · r: refresh · f: find my earbuds
Panel {
  id: root
  moduleName: "io.github.bbztp.qcy"
  ipcTarget: "io.github.bbztp.qcy"
  // Own the single IpcHandler the target permits so refresh() can be routed.
  manageIpc: false

  readonly property color foreground: bar ? bar.foreground : Color.foreground
  readonly property color accent: Color.accent
  readonly property string fontFamily: bar ? bar.fontFamily : Style.font.family
  readonly property bool vertical: bar ? bar.vertical : false
  readonly property bool showPercentage: setting("showPercentage", false)

  // Optimistic toggle state while a setter is in flight, cleared when a fresh
  // report lands. Lets the switch throw immediately without waiting a scan.
  property var optimistic: ({})
  property int nowTick: 0

  readonly property var rep: Qcy.report
  readonly property var batt: rep ? rep.battery : null
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
  readonly property var toggleDefs: [
    { key: "ldac", label: "LDAC (hi-res audio)" },
    { key: "multipoint", label: "Multipoint" },
    { key: "game", label: "Game mode (low latency)" },
    { key: "sleep", label: "Sleep mode" },
    { key: "wind", label: "Wind noise reduction" }
  ]

  function batteryGlyph() {
    var idx = Math.max(0, Math.min(9, Math.floor(level * 10)))
    return charging ? _chargeGlyphs[idx] : _levelGlyphs[idx]
  }

  function barText() {
    if (!Qcy.connected) return _earbudsGlyph
    var g = batteryGlyph()
    if (showPercentage && !vertical) return Math.round(level * 100) + "% " + g
    return g
  }

  function heroStatus() {
    if (!Qcy.connected) return "NOT CONNECTED"
    if (Qcy.loading) return "READING…"
    if (rep && rep.firmware) return "FIRMWARE " + rep.firmware.left
    return "CONNECTED"
  }

  function currentAnc() {
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

  function togglePercentage() {
    root.settings = Object.assign({}, root.settings, { showPercentage: !root.showPercentage })
    Qcy.settings = root.settings
    if (bar && bar.shell && typeof bar.shell.updateEntryInline === "function")
      bar.shell.updateEntryInline(root.moduleName, root.settings)
  }

  function setToggle(key, label, on) {
    var next = Object.assign({}, optimistic)
    next[key] = on
    optimistic = next
    Qcy.set(label, [key, on ? "on" : "off"])
  }

  Component.onCompleted: Qcy.settings = settings
  onSettingsChanged: Qcy.settings = settings

  onOpenedChanged: {
    if (opened && Qcy.refreshOnOpen && Qcy.connected) Qcy.refresh()
  }

  // A fresh report supersedes any optimistic toggle state.
  Connections {
    target: Qcy
    function onUpdatedAtChanged() { root.optimistic = ({}) }
    function onConnectedChanged() { root.optimistic = ({}) }
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
    function togglePercentage() { root.togglePercentage() }
  }

  BarIconButton {
    id: button
    anchors.fill: parent
    bar: root.bar
    text: root.barText()
    slotSize: Style.bar.iconSlot * (root.showPercentage && !root.vertical && Qcy.connected ? 2 : 1)
    onPressed: function (b) {
      if (b === Qt.RightButton) root.togglePercentage()
      else if (b === Qt.MiddleButton) Qcy.refresh()
      else root.toggle()
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
        else if (t === "f" || t === "F") Qcy.set("Find my earbuds", ["find"])
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
                onClicked: Qcy.set("ANC " + modelData.label, ["anc", modelData.key])
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
                busy: Qcy.busyAction !== "" || Qcy.loading
                foreground: root.foreground
                accent: root.accent
                onToggled: root.setToggle(modelData.key, modelData.label, !root.toggleState(modelData.key))
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
            label: "Find my earbuds"
            onClicked: Qcy.set("Find my earbuds", ["find"])
          }
          Chip {
            label: Qcy.loading ? "Reading…" : "Refresh"
            active: Qcy.loading
            onClicked: {
              if (!Qcy.loading) Qcy.refresh()
            }
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
