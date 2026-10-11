pragma Singleton
import QtQuick
import Quickshell
import Quickshell.Io
import Quickshell.Bluetooth

// Shared state for the QCY bar widget. A singleton so a multi-monitor setup
// runs ONE qcyctl process regardless of how many bar surfaces show the widget;
// every surface binds to the same reports.
//
// Three sources of truth:
//   * BlueZ via Quickshell (`device`) — instant, gives a live battery and the
//     connected/disconnected edges we react to.
//   * `qcyctl battery --json` (`battery`) — one light GATT read (no query
//     chain), polled while the panel is open.
//   * `qcyctl status --json` (`report`) — a full read of ANC, toggles, EQ, etc.
//     It scans, so it only runs on demand: on (re)connect, manual refresh, and
//     an occasional periodic poll while the panel is open.
//
// Setters are fire-and-forget: the UI goes optimistic immediately and a setter
// never triggers a full status read, so clicks apply without making you wait
// for a scan. The next full read (reconnect/periodic/manual) reconciles.
Singleton {
  id: root

  // Pushed by each bar-widget instance from shell.json settings.
  property var settings: ({})

  readonly property string deviceMatch: settings && settings.deviceMatch
    ? String(settings.deviceMatch).toLowerCase() : "qcy"
  readonly property int scanSeconds: settings && settings.scanSeconds
    ? Math.max(5, Number(settings.scanSeconds)) : 10

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

  // True from the moment a click is accepted until its setter has exited.
  // Reads are dropped while this holds, and the UI uses it to ignore clicks
  // that would arrive while a previous setter is still going out.
  readonly property bool settling: setProc.running || _pendingSet !== null

  // Only one qcyctl may touch the LE adapter at a time (see note in connectQCY).
  // Reads are low priority and run in the background; a user action PREEMPTS a
  // read in flight (kills it) and runs right away, so a click never waits behind
  // a slow read. Interrupted reads simply resume on their next tick.
  property bool _wantBattery: false
  property var _pendingSet: null
  property bool _preempting: false

  // Read the full state from the earbuds. Low priority: if anything else is
  // using the adapter the read is skipped and retried on the next tick.
  function refresh() {
    if (statusProc.running || setProc.running || batteryProc.running) return
    error = ""
    statusProc.running = true
  }

  // Fire a qcyctl setter with priority. The UI already shows the change
  // optimistically; a read in flight is killed, and its onExited — which fires
  // only once the process is truly gone — starts the setter, so the two never
  // overlap on the adapter.
  function set(action, args) {
    if (setProc.running) return
    _pendingSet = { action: action, args: args }
    if (statusProc.running || batteryProc.running) {
      _preempting = true
      if (statusProc.running) { statusProc.signal(9); statusProc.running = false }
      if (batteryProc.running) { batteryProc.signal(9); batteryProc.running = false }
    } else {
      _drain()
    }
  }
  function _startSet(action, args) {
    _preempting = false
    error = ""
    busyAction = action
    setProc.command = [root.binary].concat(args, [String(root.scanSeconds)])
    setProc.running = true
  }

  // --------------------------------------------------------- battery (light)

  property var battery: null        // parsed `qcyctl battery --json`
  property bool batteryLoading: false

  // One light battery read; used on panel open and on a slow poll while open.
  function pollBattery() {
    if (batteryProc.running) return
    if (statusProc.running || setProc.running) { _wantBattery = true; return }
    _wantBattery = false
    batteryProc.running = true
  }

  // Run the highest-priority queued work once the adapter is free.
  function _drain() {
    if (statusProc.running || setProc.running || batteryProc.running) return
    if (_pendingSet) {
      var s = _pendingSet
      _pendingSet = null
      _startSet(s.action, s.args)
      return
    }
    if (_wantBattery && connected) pollBattery()
  }

  function _applyStatus(text) {
    if (setProc.running || _pendingSet) return
    var trimmed = text ? String(text).trim() : ""
    if (trimmed === "") return
    try {
      report = JSON.parse(trimmed)
      updatedAt = Date.now()
    } catch (e) {
      error = "Could not parse qcyctl output"
    }
  }

  function _applyBattery(text) {
    if (setProc.running || _pendingSet) return
    var trimmed = text ? String(text).trim() : ""
    if (trimmed === "") return
    try {
      battery = JSON.parse(trimmed)
    } catch (e) {
      /* keep the last good reading */
    }
  }

  // Detect state on connect/disconnect edges: clear stale reports on drop, read
  // on (re)connect.
  onConnectedChanged: {
    if (connected) {
      refresh()
      pollBattery()
    } else {
      report = null
      battery = null
      error = ""
      _wantBattery = false
      _pendingSet = null
    }
  }

  Process {
    id: statusProc
    command: [root.binary, "status", "--json", String(root.scanSeconds)]
    onRunningChanged: root.loading = running
    onExited: function (exitCode) {
      var preempted = root._preempting
      root._preempting = false
      if (!preempted && exitCode !== 0 && root.report === null && root.connected)
        root.error = "Could not read the earbuds (exit " + exitCode + "). They may be busy; retrying."
      root._drain()
    }
    stdout: StdioCollector {
      waitForEnd: true
      onStreamFinished: root._applyStatus(text)
    }
  }

  // Setter runner: only a failure triggers a re-read (the optimistic UI holds
  // otherwise until the next full read).
  Process {
    id: setProc
    onRunningChanged: {
      if (!running) root.busyAction = ""
    }
    onExited: function (exitCode) {
      if (exitCode !== 0) {
        root.error = "Command failed (exit " + exitCode + ")"
        root.refresh()
      }
      root._drain()
    }
    stdout: StdioCollector { waitForEnd: true }
    stderr: StdioCollector { waitForEnd: true }
  }

  Process {
    id: batteryProc
    command: [root.binary, "battery", "--json", String(root.scanSeconds)]
    onRunningChanged: root.batteryLoading = running
    onExited: function (exitCode) {
      root._drain()
    }
    stdout: StdioCollector {
      waitForEnd: true
      onStreamFinished: root._applyBattery(text)
    }
  }
}