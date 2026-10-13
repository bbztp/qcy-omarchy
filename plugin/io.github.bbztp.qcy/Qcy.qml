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

  // ---------------------------------------------------------- translations
  //
  // English is the source language and the key: tr("Refresh"). The
  // bundle comes from `qcyctl i18n bundle --json` (shipped languages plus the
  // user's own in ~/.config/qcy-omarchy/i18n) and maps English text to the
  // active language; a missing entry falls back to the English key itself.
  // Every UI binding that calls tr() re-evaluates when `trRev` changes,
  // because tr() reads it.
  property var strings: ({})
  property int trRev: 0
  property string lang: "en"           // language in use (resolved)
  property string langChoice: "auto"   // saved choice: "auto" or a code
  property var languages: []           // [{code, name, builtin, user}]

  // Decimal separator of the active language ("_decimal" in its file).
  readonly property string decimalSep: {
    var rev = trRev
    var d = strings["_decimal"]
    return (typeof d === "string" && d !== "") ? d : "."
  }

  // `params` fills {name} markers: tr("Updated {n} s ago", { n: 5 }).
  function tr(key, params) {
    var rev = trRev
    var out = key
    if (Object.prototype.hasOwnProperty.call(strings, key)) {
      var v = strings[key]
      if (typeof v === "string" && v !== "") out = v
    }
    if (params) {
      for (var k in params) out = out.split("{" + k + "}").join(String(params[k]))
    }
    return out
  }

  function loadLanguages() {
    runOffline(["i18n", "bundle", "--json"], function (code, out, err) {
      var o = root._parseJson(out)
      root.strings = (code === 0 && o && typeof o === "object" && !Array.isArray(o)) ? o : ({})
      root.trRev++
    })
    runOffline(["i18n", "list", "--json"], function (code, out, err) {
      var j = root._parseJson(out)
      if (code !== 0 || !j) return
      root.languages = Array.isArray(j.languages) ? j.languages : []
      root.langChoice = j.choice ? String(j.choice) : "auto"
      root.lang = j.active ? String(j.active) : "en"
    })
  }

  // `choice` is "auto" or a language code; applies at once.
  function setLanguage(choice) {
    runOffline(["i18n", "set", String(choice)], function (code, out, err) {
      if (code !== 0) root.error = root.tr("Could not change the language")
      root.loadLanguages()
    })
  }

  Component.onCompleted: loadLanguages()

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
  // Model detection from the daemon (vendorId first, name as fallback). A unit
  // outside the verified set runs on the shared controls only.
  readonly property bool modelUnverified: !!(report && report.capabilities
    && report.capabilities.verified === false)
  readonly property string productTitle: (report && report.model && report.model.productTitle)
    ? String(report.model.productTitle) : ""
  // Real product images of the connected model ({left,right,box,icon} as
  // file:// URLs) or {} when unavailable. Loaded from the daemon's cache by a
  // one-shot `qcyctl model image --json`; it uses no BLE, so it never competes
  // with the adapter. Failure is silent: the UI keeps its drawn visuals.
  property var images: ({})
  readonly property string _imageKey: (connected && report && report.model
    && report.model.vendorId !== undefined && report.model.vendorId !== null)
    ? String(report.model.vendorId) : ""
  // Fetch attempts for the current _imageKey (max 3: first, +30 s retry, and
  // manual refresh() while empty); reset whenever the key changes.
  property int _imageTries: 0
  on_ImageKeyChanged: {
    images = ({})
    _imageTries = 0
    imageRetry.stop()
    if (_imageKey === "") { if (imageProc.running) imageProc.running = false; return }
    _loadImages()
  }
  function _loadImages() {
    if (_imageKey === "" || _imageTries >= 3) return
    _imageTries++
    imageRetry.stop()
    if (imageProc.running) imageProc.running = false
    imageProc.command = [binary, "model", "image", "--json", "--vendor", _imageKey]
    imageProc.running = true
  }
  Timer {
    id: imageRetry
    interval: 30000
    repeat: false
    onTriggered: if (root.connected && !imageProc.running && Object.keys(root.images).length === 0) root._loadImages()
  }
  property bool loading: false
  property string error: ""
  // True when the last failure was the daemon's "busy" answer: the earbuds
  // accept one BLE client at a time and another app (the phone) holds them.
  property bool busy: false
  // Age in seconds of a status served from the daemon's cache, or -1 when the
  // report on screen is fresh.
  property int staleAgeSeconds: -1
  readonly property string busyMessage: tr("The earbuds are in use by another app — close it and refresh")
  property double updatedAt: 0     // Date.now() of the last successful read
  property string busyAction: ""   // label of an in-flight setter, for UI feedback

  // True from the moment a click is accepted until its setter has exited.
  // Reads are dropped while this holds, and the UI uses it to ignore clicks
  // that would arrive while a previous setter is still going out.
  readonly property bool settling: setProc.running || _pendingSet !== null || _run !== null

  // Only one qcyctl may touch the LE adapter at a time (see note in connectQCY).
  // Reads are low priority and run in the background; a user action PREEMPTS a
  // read in flight (kills it) and runs right away, so a click never waits behind
  // a slow read. Interrupted reads simply resume on their next tick.
  property bool _wantBattery: false
  property var _pendingSet: null

  // Read the full state from the earbuds. Low priority: if anything else is
  // using the adapter the read is skipped and retried on the next tick.
  function refresh() {
    if (connected && _imageKey !== "" && Object.keys(images).length === 0
        && !imageProc.running) _loadImages()
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
  //
  // `done` (optional) is called once with (exitCode, stdout, stderr) after the
  // setter has exited and both streams have ended; exitCode -1 means the
  // command never ran (replaced by a later click, or the earbuds went away).
  function set(action, args, done) {
    _dropPending("replaced")
    _pendingSet = { action: action, args: args, done: done ? done : null }
    if (statusProc.running || batteryProc.running) {
      if (statusProc.running) { statusProc.signal(9); statusProc.running = false }
      if (batteryProc.running) { batteryProc.signal(9); batteryProc.running = false }
    } else if (!setProc.running) {
      _drain()
    }
  }
  function _dropPending(why) {
    var p = _pendingSet
    _pendingSet = null
    if (p && p.done) {
      try { p.done(-1, "", why) } catch (e) { console.warn("qcy: callback failed:", e) }
    } else if (p) {
      // A plain setter dropped before it ran: let the optimistic UI revert.
      root.setFailed()
    }
  }

  // The run whose callback is waiting for exit + both streams, or null.
  property var _run: null

  function _startSet(action, args, done) {
    error = ""
    busyAction = action
    _run = done ? { done: done, exit: -1, out: "", err: "", haveExit: false, haveOut: false, haveErr: false } : null
    setProc.command = [root.binary].concat(args, [String(root.scanSeconds)])
    setProc.running = true
    if (done) setWatchdog.restart()
  }

  // A setter with a callback that never exits would pin _run, settling and the
  // library phase; kill it and deliver whatever arrived.
  Timer {
    id: setWatchdog
    interval: 60000
    repeat: false
    onTriggered: {
      if (setProc.running) setProc.signal(9)
      root._joinRun(true)
    }
  }

  // ------------------------------------------------------------ find earbuds
  //
  // `qcyctl find --yes --events` plays a beeping tone on the earbuds' audio
  // sink and raises the sink volume gently (a ceiling and a timeout are
  // enforced by the CLI). It is a long-running process: JSON event lines come
  // on stdout, "stop" goes to its stdin and the CLI restores the volume and
  // the music before it exits. The caller must have shown the warning
  // "Remove the earbuds from your ears before starting" and got a "yes".
  property bool findRunning: false
  property string findStatus: "idle"   // idle | starting | playing | stopping
  property int findVolume: 0           // current sink volume in %, 0 = unknown
  property int findMaxVolume: 0
  property string findMessage: ""      // last error, "" when none

  function startFind() {
    if (findProc.running) return
    findMessage = ""
    findVolume = 0
    findStatus = "starting"
    findRunning = true
    findProc.command = [root.binary, "find", "--yes", "--events"]
    findProc.running = true
  }

  // Ask the process to stop and wait for it: it restores volume and music
  // first. SIGTERM (same cleanup) only if it does not answer.
  function stopFind() {
    if (!findProc.running) return
    findStatus = "stopping"
    try { findProc.write("stop\n") } catch (e) { findProc.signal(15) }
    findKillTimer.restart()
  }

  function _findEvent(line) {
    var ev = _parseJson(line)
    if (!ev || typeof ev.event !== "string") return
    if (ev.event === "started") {
      findVolume = ev.volume || 0
      findMaxVolume = ev.maxVolume || 0
      if (findStatus !== "stopping") findStatus = "playing"
    } else if (ev.event === "volume") {
      findVolume = ev.volume || 0
    } else if (ev.event === "error") {
      findMessage = String(ev.message || "")
    }
  }

  Timer {
    id: findKillTimer
    interval: 6000
    repeat: false
    onTriggered: if (findProc.running) findProc.signal(15)
  }

  Process {
    id: findProc
    stdinEnabled: true
    stdout: SplitParser {
      onRead: function (data) { root._findEvent(data) }
    }
    stderr: StdioCollector {
      id: findErr
      waitForEnd: true
    }
    onExited: function (exitCode) {
      findKillTimer.stop()
      if (exitCode !== 0 && root.findMessage === "") {
        root.findMessage = root._lastLine(findErr.text) !== "" ? root._lastLine(findErr.text)
          : tr("Could not play the find tone (code {code})", { code: exitCode })
      }
      root.findRunning = false
      root.findStatus = "idle"
      root.findVolume = 0
    }
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
    if (_run !== null) return
    if (_pendingSet) {
      var s = _pendingSet
      _pendingSet = null
      _startSet(s.action, s.args, s.done)
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
        error = tr("{message} (showing the last reading, from {n} s ago)", { message: busyMessage, n: age })
      } else {
        staleAgeSeconds = -1
        busy = false
      }
    } catch (e) {
      error = tr("Could not read qcyctl's output")
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
      _writeNext = null   // a queued write must not run on the next connection
      _dropPending("disconnected")
    }
  }

  Process {
    id: imageProc
    command: [root.binary, "model", "image", "--json"]
    stdout: StdioCollector {
      waitForEnd: true
      onStreamFinished: {
        var out = {}
        var bad = false
        try {
          var parsed = JSON.parse(text)
          if (parsed.missing && parsed.missing.length > 0) bad = true
          var f = parsed.files || {}
          var names = ["left", "right", "box", "icon"]
          for (var i = 0; i < names.length; i++) {
            var v = f[names[i]]
            if (typeof v === "string" && v.charAt(0) === "/") out[names[i]] = "file://" + encodeURI(v)
          }
        } catch (e) { out = {}; bad = true }
        root.images = out
        if ((bad || Object.keys(out).length === 0) && root._imageTries < 3
            && root.connected) imageRetry.restart()
      }
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
        root.error = tr("Could not read the earbuds (code {code})", { code: exitCode })
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
      if (root._run !== null) {
        // Failure handling waits for the join, so it reads the complete stderr.
        root._run.exit = exitCode
        root._run.haveExit = true
        joinTimer.restart()
        root._joinRun()
      } else {
        root._setFinished(exitCode, setErr.text, false)
      }
      root._drain()
    }
    stdout: StdioCollector {
      waitForEnd: true
      onStreamFinished: {
        if (root._run !== null) {
          root._run.out = text
          root._run.haveOut = true
          root._joinRun()
        }
      }
    }
    stderr: StdioCollector {
      id: setErr
      waitForEnd: true
      onStreamFinished: {
        if (root._run !== null) {
          root._run.err = text
          root._run.haveErr = true
          root._joinRun()
        }
      }
    }
  }

  // Failure bookkeeping for a finished setter. `errText` is the complete stderr
  // (r.err for a run with a callback).
  function _setFinished(exitCode, errText, hasCallback) {
    if (exitCode === 0) return
    var wasBusy = _isBusyText(errText)
    if (wasBusy) {
      busy = true
      error = busyMessage
    } else if (!hasCallback) {
      // A caller with a callback explains the failure itself.
      error = tr("The command failed (code {code})", { code: exitCode })
    }
    setFailed()
    // No immediate re-read when the earbuds are held elsewhere: that would
    // clear the message and only contest the link again.
    if (!wasBusy) refresh()
  }

  // Delivers a setter's result once exit and both streams have all arrived;
  // whichever of the three comes last triggers it. The timer is a safety net
  // for a stream that never reports its end.
  function _joinRun(force) {
    var r = _run
    if (r === null) return
    if (!force && !(r.haveExit && r.haveOut && r.haveErr)) return
    joinTimer.stop()
    setWatchdog.stop()
    _run = null
    _setFinished(r.exit, r.err, true)
    try { r.done(r.exit, r.out, r.err) } catch (e) { console.warn("qcy: callback failed:", e) }
    _drain()
  }

  Timer {
    id: joinTimer
    interval: 3000
    repeat: false
    onTriggered: root._joinRun(true)
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

  // ------------------------------------------------------- curve library
  //
  // `qcyctl library list/seed/delete` are offline (files only, never the
  // daemon), so they run on their own process and may overlap Bluetooth work.
  // `apply` and `compose` talk to the earbuds and go through set(), so
  // they queue with the other setters.

  property var library: []          // parsed `library list --json`
  property bool libraryLoaded: false
  property bool libraryLoading: false
  property string libraryError: ""
  property bool _seeded: false

  // Last library operation, for the panel to show:
  //   kind: apply | compose | save | delete
  //   phase: idle | running | ok | unconfirmed | error
  property var libraryOp: ({ kind: "", phase: "idle", id: "", message: "", backup: "" })

  function _setOp(kind, phase, id, message, backup) {
    libraryOp = { kind: kind, phase: phase, id: id || "", message: message || "", backup: backup || "" }
  }

  function _parseJson(text) {
    var t = text ? String(text).trim() : ""
    if (t === "") return null
    try { return JSON.parse(t) } catch (e) { return null }
  }

  // Last meaningful stderr line, for messages when there is no JSON.
  function _lastLine(text) {
    var lines = String(text || "").split("\n")
    for (var i = lines.length - 1; i >= 0; i--) {
      var l = lines[i].trim()
      if (l !== "") return l
    }
    return ""
  }

  property var _offQueue: []
  property var _offRun: null

  function runOffline(args, done) {
    _offQueue = _offQueue.concat([{ args: args, done: done }])
    _offNext()
  }

  function _offNext() {
    if (offProc.running || _offRun !== null || _offQueue.length === 0) return
    var j = _offQueue[0]
    _offQueue = _offQueue.slice(1)
    _offRun = { done: j.done, exit: -1, out: "", err: "", haveExit: false, haveOut: false, haveErr: false }
    offProc.command = [root.binary].concat(j.args)
    offTimer.restart()
    offProc.running = true
  }

  function _offJoin(force) {
    var r = _offRun
    if (r === null) return
    if (!force && !(r.haveExit && r.haveOut && r.haveErr)) return
    offTimer.stop()
    _offRun = null
    try { if (r.done) r.done(r.exit, r.out, r.err) } catch (e) { console.warn("qcy: callback failed:", e) }
    _offNext()
  }

  Process {
    id: offProc
    onExited: function (exitCode) {
      if (root._offRun !== null) {
        root._offRun.exit = exitCode
        root._offRun.haveExit = true
        root._offJoin()
      } else {
        // The timeout already joined this run; the queue was stuck behind
        // the still-running process.
        root._offNext()
      }
    }
    stdout: StdioCollector {
      waitForEnd: true
      onStreamFinished: {
        if (root._offRun !== null) {
          root._offRun.out = text
          root._offRun.haveOut = true
          root._offJoin()
        }
      }
    }
    stderr: StdioCollector {
      waitForEnd: true
      onStreamFinished: {
        if (root._offRun !== null) {
          root._offRun.err = text
          root._offRun.haveErr = true
          root._offJoin()
        }
      }
    }
  }

  // Offline commands finish in milliseconds; this only guards a binary that
  // never started.
  Timer {
    id: offTimer
    interval: 15000
    repeat: false
    onTriggered: {
      if (offProc.running) offProc.signal(9)
      root._offJoin(true)
    }
  }

  // Reload the list. On a first, empty library, seed it once (idempotent) and
  // read again.
  function libraryReload() {
    libraryLoading = true
    runOffline(["library", "list", "--json"], function (code, out, err) {
      var arr = root._parseJson(out)
      if (code !== 0 || !Array.isArray(arr)) {
        root.libraryLoading = false
        root.libraryError = root._lastLine(err) || tr("Could not read the library")
        return
      }
      if (arr.length === 0 && !root._seeded) {
        root._seeded = true
        root.runOffline(["library", "seed", "--json"], function (c2, o2, e2) {
          root.libraryLoading = false
          // Whatever seed said, show what is there now.
          root.libraryReload()
        })
        return
      }
      root.libraryError = ""
      root.library = arr
      root.libraryLoaded = true
      root.libraryLoading = false
    })
  }

  // ---- writes to the earbuds: apply an effect / apply the equalizer ----
  //
  // Both go to the phone app's "Personalizado" buffer, always allowed. One
  // write is in flight at a time and the newest request wins: a request made
  // while another runs replaces the queued one and starts when the current one
  // is done, so dragging sliders never stacks processes and a card click is
  // never lost. Never passes --allow-protected nor --full-length.

  property bool writeBusy: false
  property var _writeNext: null

  // Applies library curve `id`.
  function libraryApply(id) {
    _writeNext = { kind: "apply", id: String(id) }
    _setOp("apply", "running", String(id), "", "")
    _writeKick()
  }

  // The user's equalizer on top of effect `base` (a curve id, or "none").
  // `gains` is "g1,...,g10" (dB); `master` is a dB number, or null to leave
  // the base's own overall gain alone.
  function libraryCompose(base, gains, master) {
    _writeNext = {
      kind: "compose",
      base: String(base || "none"),
      gains: String(gains),
      master: (master === null || master === undefined) ? null : Number(master)
    }
    _setOp("compose", "running", "", "", "")
    _writeKick()
  }

  function _writeKick() {
    if (writeBusy || _writeNext === null) return
    var req = _writeNext
    _writeNext = null
    writeBusy = true
    var args
    if (req.kind === "apply") {
      args = ["library", "apply", req.id, "--yes", "--json"]
    } else {
      args = ["library", "compose", "--base=" + req.base, "--gains=" + req.gains]
      if (req.master !== null) args.push("--master=" + req.master.toFixed(1))
      args.push("--yes", "--json")
    }
    set(req.kind === "apply" ? "Apply effect" : "Apply equalizer", args, function (code, out, err) {
      root.writeBusy = false
      root._writeDone(req, code, out, err)
      // -1: replaced inside set() by another setter; starting the next write
      // from here would re-enter set(), so let that call finish first.
      if (code === -1) Qt.callLater(root._writeKick)
      else root._writeKick()
    })
  }

  function _writeDone(req, code, out, err) {
    // A newer request is already waiting: its own state is what is on screen.
    var stale = _writeNext !== null
    var j = root._parseJson(out)
    var id = req.kind === "apply" ? req.id : ""
    var backup = j && j.backup ? j.backup : ""
    if (code === 0 && !(j && j.confirmed === false)) {
      if (stale) return
      root._setOp(req.kind, "ok", id, "", backup)
      root.refresh()
      return
    }
    if (stale) return
    if (code === -1) {
      root._setOp(req.kind, "error", id, tr("Cancelled: another action was sent or the earbuds went out of range."), "")
      return
    }
    if (j && j.code === "unconfirmed") {
      root._setOp(req.kind, "unconfirmed", id, j.error || "", backup)
      root.refresh()
      return
    }
    var msg
    if (j && j.code === "busy" || root._isBusyText(err))
      msg = root.busyMessage
    else if (j && j.code === "not_found")
      msg = tr("Earbuds not found (off or out of range).")
    else if (/hold at most|bands, but/i.test((j && j.error ? j.error : "") + " " + err))
      msg = tr("Too many bands: zero some bars or choose another effect.")
    else
      msg = (j && j.error) ? j.error : (root._lastLine(err) || tr("The command failed (code {code})", { code: code }))
    root._setOp(req.kind, "error", id, msg, backup)
  }

  // Saves the equalizer's bars under a new name; never replaces. `done` (optional)
  // gets (ok, message).
  // gains are bars on top of `base` when a base is given ("" or "none": the
  // bars are the whole curve).
  function libraryCreate(name, gains, master, base, done) {
    _setOp("save", "running", "", "", "")
    var args = ["library", "save", name, "--gains=" + gains, "--master=" + Number(master).toFixed(1), "--json"]
    if (base && base !== "none") args.push("--base=" + base)
    runOffline(args,
      function (code, out, err) {
        var j = root._parseJson(out)
        if (code === 0 && j && j.id) {
          root._setOp("save", "ok", j.id, "", "")
          root.libraryReload()
          if (done) done(true, "")
          return
        }
        var msg = /already exists/i.test(err)
          ? tr("An effect with that name already exists. Choose another name.")
          : (root._lastLine(err) || tr("The command failed (code {code})", { code: code }))
        root._setOp("save", "error", "", msg, "")
        if (done) done(false, msg)
      })
  }

  // Moves a user curve to the library's .trash (recoverable by hand).
  function libraryDelete(id) {
    _setOp("delete", "running", id, "", "")
    runOffline(["library", "delete", id, "--yes"], function (code, out, err) {
      if (code === 0) {
        root._setOp("delete", "ok", id, "", "")
        root.libraryReload()
      } else {
        root._setOp("delete", "error", id, root._lastLine(err) || tr("The command failed (code {code})", { code: code }), "")
      }
    })
  }

  function libraryClearOp() {
    if (libraryOp.phase !== "running") _setOp("", "idle", "", "", "")
  }
}
