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

  // Emitted when a setter exits non-zero, so views can drop optimistic values;
  // the device may or may not have applied the change, only a read tells.
  signal setFailed()

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
  // True when the last failure was the daemon's "busy" answer: the earbuds
  // accept one BLE client at a time and another app (the phone) holds them.
  property bool busy: false
  // Age in seconds of a status served from the daemon's cache, or -1 when the
  // report on screen is fresh.
  property int staleAgeSeconds: -1
  readonly property string busyMessage: "Earbuds are in use by another app \u2014 close it and refresh"
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
  //
  // A click that arrives while a setter is still going out REPLACES the queued
  // one instead of being dropped: the first click of a session also has to pay
  // the daemon's lazy LE connect, and silently swallowing the clicks made during
  // that window is what made the panel feel stuck. Last click wins.
  function set(action, args) {
    _pendingSet = { action: action, args: args }
    if (statusProc.running || batteryProc.running) {
      if (statusProc.running) { statusProc.signal(9); statusProc.running = false }
      if (batteryProc.running) { batteryProc.signal(9); batteryProc.running = false }
    } else if (!setProc.running) {
      _drain()
    }
  }
  function _startSet(action, args) {
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
      if (report && report.stale === true) {
        // Cached reading: the earbuds are held elsewhere right now.
        var age = Math.max(0, Math.round(Number(report.ageSeconds) || 0))
        staleAgeSeconds = age
        busy = true
        error = busyMessage + " (showing the last reading, " + age + " s old)"
      } else {
        staleAgeSeconds = -1
        busy = false
      }
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

  // Short retry after a failed full read, on top of the slow panel poll: the
  // earbuds are often busy right after connect. Backoff 5s, 10s, 20s, 30s (cap),
  // giving up after _retryMax attempts until the next connect/manual refresh.
  // When the daemon says the earbuds are in use by another app, hammering them
  // only keeps the link contested: a single spaced retry (30 s) is enough.
  property int _retryCount: 0
  readonly property int _retryMax: 6
  readonly property int _busyRetryMs: 30000

  function _scheduleRetry(wasBusy) {
    if (wasBusy) {
      if (_retryCount >= 1) return
      _retryCount = 1
      retryTimer.interval = _busyRetryMs
      retryTimer.restart()
      return
    }
    if (_retryCount >= _retryMax) return
    _retryCount++
    retryTimer.interval = Math.min(30000, 5000 * Math.pow(2, _retryCount - 1))
    retryTimer.restart()
  }

  // The daemon's busy error reaches qcyctl's stderr verbatim.
  function _isBusyText(text) {
    return !!text && String(text).indexOf("in use by another app") !== -1
  }

  Timer {
    id: retryTimer
    repeat: false
    onTriggered: {
      if (!root.connected) return
      // Adapter busy: look again later without spending an attempt.
      if (statusProc.running || setProc.running || batteryProc.running) restart()
      else root.refresh()
    }
  }

  // Detect state on connect/disconnect edges: clear stale reports on drop, read
  // on (re)connect.
  onConnectedChanged: {
    retryTimer.stop()
    _retryCount = 0
    if (connected) {
      refresh()
      pollBattery()
    } else {
      report = null
      battery = null
      error = ""
      busy = false
      staleAgeSeconds = -1
      _wantBattery = false
      _pendingSet = null
    }
  }

  Process {
    id: statusProc
    command: [root.binary, "status", "--json", String(root.scanSeconds)]
    onRunningChanged: root.loading = running
    onExited: function (exitCode) {
      // A read killed to make way for a click always exits non-zero, so the
      // failure is only worth reporting when no setter is taking over. This
      // used to be tracked with a flag that other callbacks had to clear.
      var failed = exitCode !== 0 && !root.settling && root.connected
      var wasBusy = failed && root._isBusyText(statusErr.text)
      if (wasBusy) {
        root.busy = true
        root.error = root.busyMessage
      } else if (failed && root.report === null) {
        root.busy = false
        root.error = "Could not read the earbuds (exit " + exitCode + ")"
      }
      if (failed) root._scheduleRetry(wasBusy)
      else if (exitCode === 0 && !root.busy) root._retryCount = 0
      else if (exitCode === 0 && root.staleAgeSeconds >= 0) root._scheduleRetry(true)
      root._drain()
    }
    stdout: StdioCollector {
      waitForEnd: true
      onStreamFinished: root._applyStatus(text)
    }
    stderr: StdioCollector {
      id: statusErr
      waitForEnd: true
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
        var wasBusy = root._isBusyText(setErr.text)
        if (wasBusy) {
          root.busy = true
          root.error = root.busyMessage
        } else {
          root.error = "Command failed (exit " + exitCode + ")"
        }
        root.setFailed()
        // No immediate re-read when the earbuds are held elsewhere: that would
        // clear the message and only contest the link again.
        if (!wasBusy) root.refresh()
      }
      root._drain()
    }
    stdout: StdioCollector { waitForEnd: true }
    stderr: StdioCollector { id: setErr; waitForEnd: true }
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