pragma Singleton
import QtQuick
import Quickshell
import Quickshell.Io
import Quickshell.Bluetooth

// Shared state for the QCY bar widget. A singleton so a multi-monitor setup
// runs ONE qcyctl process regardless of how many bar surfaces show the widget;
// every surface binds to the same report.
//
// Two sources of truth are combined here:
//   * BlueZ via Quickshell (`device`) — instant, gives a live battery and the
//     connected/disconnected edges we react to.
//   * `qcyctl status --json` (`report`) — a full read of ANC, toggles, EQ, etc.
//     It connects to the low-energy service, which takes a scan, so it is only
//     run on demand (panel open, device connect, after a setter) — never on a timer.
Singleton {
  id: root

  // Pushed by each bar-widget instance from shell.json settings.
  property var settings: ({})

  readonly property string deviceMatch: settings && settings.deviceMatch
    ? String(settings.deviceMatch).toLowerCase() : "qcy"
  readonly property int scanSeconds: settings && settings.scanSeconds
    ? Math.max(5, Number(settings.scanSeconds)) : 20
  readonly property bool refreshOnOpen: settings ? (settings.refreshOnOpen !== false) : true

  // The CLI can be overridden with QCYCTL; otherwise it is the path the
  // install script writes to.
  readonly property string binary: Quickshell.env("QCYCTL")
    || (Quickshell.env("HOME") + "/.local/bin/qcyctl")

  // ---------------------------------------------------------------- device

  readonly property var _devices: Bluetooth.devices ? Bluetooth.devices.values : []

  // The connected earbuds, matched by name. Re-evaluates whenever any device's
  // `connected` flips because the binding reads it.
  readonly property var device: {
    for (var i = 0; i < _devices.length; i++) {
      var d = _devices[i]
      if (!d || !d.connected) continue
      var name = ((d.name || "") + " " + (d.deviceName || "")).toLowerCase()
      if (name.indexOf(deviceMatch) !== -1) return d
    }
    return null
  }

  readonly property bool connected: device !== null

  // Instant battery level from BlueZ (0..1), or -1 when unknown.
  readonly property real bluezBattery: (device && device.batteryAvailable) ? device.battery : -1

  // ------------------------------------------------------------- full state

  property var report: null        // parsed `qcyctl status --json`
  property bool loading: false
  property string error: ""
  property double updatedAt: 0     // Date.now() of the last successful read
  property string busyAction: ""   // label of an in-flight setter, for UI feedback

  // Read the full state from the earbuds. Coalesces concurrent calls.
  function refresh() {
    if (statusProc.running) return
    error = ""
    statusProc.running = true
  }

  // Fire a qcyctl setter, then re-read once the earbuds have settled.
  function set(action, args) {
    busyAction = action
    Quickshell.execDetached([binary].concat(args))
    settleTimer.restart()
  }

  function _applyStatus(text) {
    var trimmed = text ? String(text).trim() : ""
    if (trimmed === "") return
    try {
      report = JSON.parse(trimmed)
      updatedAt = Date.now()
    } catch (e) {
      error = "Could not parse qcyctl output"
    }
  }

  // React to the earbuds connecting or disconnecting.
  onConnectedChanged: {
    if (connected) refresh()
    else report = null
  }

  Timer {
    id: settleTimer
    interval: 2500
    repeat: false
    onTriggered: {
      root.busyAction = ""
      root.refresh()
    }
  }

  Process {
    id: statusProc
    command: [root.binary, "status", "--json", String(root.scanSeconds)]
    onRunningChanged: root.loading = running
    onExited: function (exitCode) {
      if (exitCode !== 0 && root.report === null)
        root.error = "qcyctl failed (exit " + exitCode + "). Is it installed and are the earbuds nearby?"
    }
    stdout: StdioCollector {
      waitForEnd: true
      onStreamFinished: root._applyStatus(text)
    }
  }
}
