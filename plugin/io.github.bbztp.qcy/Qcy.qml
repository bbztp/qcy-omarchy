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
  readonly property string busyMessage: "Os fones est\u00e3o em uso por outro app \u2014 feche-o e atualize"
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
        error = busyMessage + " (mostrando a \u00faltima leitura, de " + age + " s atr\u00e1s)"
      } else {
        staleAgeSeconds = -1
        busy = false
      }
    } catch (e) {
      error = "N\u00e3o foi poss\u00edvel ler a sa\u00edda do qcyctl"
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
      _dropPending("disconnected")
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
        root.error = "N\u00e3o foi poss\u00edvel ler os fones (c\u00f3digo " + exitCode + ")"
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
      error = "O comando falhou (c\u00f3digo " + exitCode + ")"
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
  // `apply` and `import-device` talk to the earbuds and go through set(), so
  // they queue with the other setters.

  property var library: []          // parsed `library list --json`
  property bool libraryLoaded: false
  property bool libraryLoading: false
  property string libraryError: ""
  property bool _seeded: false

  // Last library operation, for the panel to show:
  //   kind: apply | save | delete
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
        root.libraryError = root._lastLine(err) || "Não foi possível ler a biblioteca"
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

  // Writes the curve to slot 0 and makes it active. Never passes
  // --allow-protected nor --full-length: the backend refuses what is unsafe.
  function libraryApply(id) {
    if (libraryOp.phase === "running") return
    _applyRun(id, null)
  }

  // `after` (optional) runs once on every terminal path, before any queued
  // draft is started. The caller has already checked libraryOp.phase.
  function _applyRun(id, after) {
    _setOp("apply", "running", id, "", "")
    function finish() {
      try { if (after) after() } catch (e) { console.warn("qcy: callback failed:", e) }
      root._draftKick()
    }
    set("Aplicar curva", ["library", "apply", id, "--yes", "--json"], function (code, out, err) {
      var j = root._parseJson(out)
      if (code === 0 && j && j.confirmed === true) {
        root._setOp("apply", "ok", id, "", j.backup || "")
        root.refresh()
        finish()
        return
      }
      if (code === -1) {
        root._setOp("apply", "error", id, "Cancelado: outra ação foi enviada ou os fones saíram de alcance.", "")
        finish()
        return
      }
      var backup = j && j.backup ? j.backup : ""
      if (j && j.code === "unconfirmed") {
        root._setOp("apply", "unconfirmed", id, j.error || "", backup)
        root.refresh()
        finish()
        return
      }
      var msg
      if (j && j.code === "protected_slot")
        msg = "O slot ativo é protegido (6 ou 11); nada foi gravado."
      else if (j && j.code === "full_length_required")
        msg = "Esta curva precisa do formato de 143 bytes, ainda não validado; nada foi gravado."
      else if (j && j.code === "busy" || root._isBusyText(err))
        msg = root.busyMessage
      else if (j && j.code === "not_found")
        msg = "Fones não encontrados (desligados ou fora de alcance)."
      else
        msg = (j && j.error) ? j.error : (root._lastLine(err) || ("O comando falhou (código " + code + ")"))
      root._setOp("apply", "error", id, msg, backup)
      finish()
    })
  }

  // ---- curve editor ----
  //
  // gains is the "g1,...,g10" string (dB), master a dB number; both come
  // already clamped and rounded by the editor, the backend validates again.

  readonly property string draftId: "rascunho"
  property bool draftBusy: false
  property var _draftNext: null       // latest queued request; last one wins

  function _hasCurve(id) {
    for (var i = 0; i < library.length; i++) if (library[i].id === id) return true
    return false
  }

  // Saves the draft curve ("rascunho", created or replaced) and applies it.
  // Never more than one in flight: a request made meanwhile replaces the
  // queued one and runs when the current one is done.
  function libraryDraftApply(gains, master) {
    _draftNext = { gains: String(gains), master: Number(master) }
    _draftKick()
  }

  function _draftKick() {
    if (draftBusy || _draftNext === null) return
    if (libraryOp.phase === "running") return   // _applyRun's finish() kicks again
    var req = _draftNext
    _draftNext = null
    draftBusy = true
    _setOp("apply", "running", draftId, "", "")
    _draftSave(req, _hasCurve(draftId), true)
  }

  function _draftSave(req, replace, mayRetry) {
    var args = ["library", "save", draftId, "--gains=" + req.gains, "--master=" + req.master.toFixed(1),
                "--note=Rascunho do editor", "--json"]
    if (replace) args.push("--replace")
    runOffline(args, function (code, out, err) {
      if (code === 0) {
        root.libraryReload()
        root._applyRun(root.draftId, function () { root.draftBusy = false })
        return
      }
      // The list may have been stale: flip create/replace once.
      if (mayRetry && !replace && /already exists/i.test(err)) { root._draftSave(req, true, false); return }
      if (mayRetry && replace && /no such file|not exist/i.test(err)) { root._draftSave(req, false, false); return }
      root.draftBusy = false
      root._setOp("apply", "error", root.draftId, "Não foi possível salvar o rascunho: "
        + (root._lastLine(err) || ("código " + code)), "")
      root._draftKick()
    })
  }

  // Saves the editor's bars under a new name; never replaces.
  function libraryCreate(name, gains, master, done) {
    if (libraryOp.phase === "running") return
    _setOp("save", "running", "", "", "")
    runOffline(["library", "save", name, "--gains=" + gains, "--master=" + Number(master).toFixed(1), "--json"],
      function (code, out, err) {
        var j = root._parseJson(out)
        if (code === 0 && j && j.id) {
          root._setOp("save", "ok", j.id, "", "")
          root.libraryReload()
          if (done) done(true)
          root._draftKick()
          return
        }
        var msg = /already exists/i.test(err)
          ? "Já existe uma curva com esse nome. Escolha outro nome."
          : (root._lastLine(err) || ("O comando falhou (código " + code + ")"))
        root._setOp("save", "error", "", msg, "")
        if (done) done(false)
        root._draftKick()
      })
  }

  // Saves the EQ the earbuds are using now as a new library curve.
  function libraryImportDevice(name) {
    if (libraryOp.phase === "running") return
    _setOp("save", "running", "", "", "")
    set("Salvar curva", ["library", "import-device", name, "--json"], function (code, out, err) {
      var j = root._parseJson(out)
      if (code === 0 && j && j.id) {
        root._setOp("save", "ok", j.id, "", "")
        root.libraryReload()
        root._draftKick()
        return
      }
      var msg = code === -1
        ? "Cancelado: outra ação foi enviada ou os fones saíram de alcance."
        : (root._isBusyText(err) ? root.busyMessage : (root._lastLine(err) || ("O comando falhou (código " + code + ")")))
      root._setOp("save", "error", "", msg, "")
      root._draftKick()
    })
  }

  // Moves a user curve to the library's .trash (recoverable by hand).
  function libraryDelete(id) {
    if (libraryOp.phase === "running") return
    _setOp("delete", "running", id, "", "")
    runOffline(["library", "delete", id, "--yes"], function (code, out, err) {
      if (code === 0) {
        root._setOp("delete", "ok", id, "", "")
        root.libraryReload()
      } else {
        root._setOp("delete", "error", id, root._lastLine(err) || ("O comando falhou (código " + code + ")"), "")
      }
      root._draftKick()
    })
  }

  function libraryClearOp() {
    if (libraryOp.phase !== "running") _setOp("", "idle", "", "", "")
  }
}
