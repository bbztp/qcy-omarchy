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

  Component.onCompleted: _setupStart()

  // Pushed by each bar-widget instance from shell.json settings.
  property var settings: ({})

  readonly property string deviceMatch: settings && settings.deviceMatch
    ? String(settings.deviceMatch).toLowerCase() : "qcy"
  readonly property int scanSeconds: settings && settings.scanSeconds
    ? Math.max(5, Number(settings.scanSeconds)) : 10

  // ----------------------------------------------------------------- setup
  //
  // The plugin prepares its own binaries on first use (scripts/bootstrap.sh
  // downloads the matching release, or builds with Go) into
  // ~/.local/share/qcy-omarchy/bin/<version>, with stable qcyctl/qcyctld links
  // beside it. The shell watches ~/.config/omarchy/plugins recursively and
  // reloads every plugin on any write below it, so nothing is ever written
  // under the plugin directory: downloads, builds and the install all happen in
  // the data directory. `omarchy plugin remove` has no hook and does not delete
  // those binaries; the daemon exits on its own once the plugin directory is
  // gone, and "Remove QCY data…" deletes them.
  //
  // setupState: "checking" (looking for a matching qcyctl), "preparing"
  // (bootstrap running), "ready", "error". No other process of the plugin calls
  // the binary before "ready".
  property string setupState: "checking"
  property string setupStep: ""        // last STEP text from the bootstrap
  property string setupMessage: ""     // error text when setupState is "error"
  property string _bootstrapBinary: "" // qcyctl path the bootstrap reported (BIN line)
  // False while "Remove QCY data…" runs between the daemon stop and the delete,
  // so no poll, refresh or setter can start the daemon again (all of them gate
  // on `ready`; the removal steps themselves bypass it in _offNext).
  property bool _removing: false
  readonly property bool ready: setupState === "ready" && !_removing

  readonly property string _home: Quickshell.env("HOME")
  readonly property string dataDir: (Quickshell.env("XDG_DATA_HOME")
    ? Quickshell.env("XDG_DATA_HOME") : (_home + "/.local/share")) + "/qcy-omarchy"
  readonly property string _dataBinary: dataDir + "/bin/qcyctl"

  // The binary in use: QCYCTL, else the data dir install; after the check it
  // becomes whichever matching candidate was found (~/.local/bin, PATH).
  property string binary: Quickshell.env("QCYCTL") || _dataBinary

  // Directory the plugin was loaded from.
  readonly property string pluginDir: {
    var u = String(Qt.resolvedUrl("."))
    u = u.replace(/^file:\/\//, "")
    try { u = decodeURIComponent(u) } catch (e) {}
    return u.replace(/\/+$/, "")
  }
  property string pluginVersion: ""

  function _setupStart() {
    setupState = "checking"
    setupMessage = ""
    setupStep = ""
    if (Quickshell.env("QCYCTL")) { _setupReady(Quickshell.env("QCYCTL")); return }
    manifestProc.running = true
  }

  function _setupReady(path) {
    binary = path
    setupState = "ready"
    setupMessage = ""
    setupStep = ""
    loadLanguages()
    daemonRefresh()
    _offNext()
    if (connected) refresh()
  }

  function _setupPrepare() {
    if (bootstrapProc.running) return
    setupState = "preparing"
    setupMessage = ""
    setupStep = tr("Starting")
    bootstrapProc.command = ["bash", pluginDir + "/scripts/bootstrap.sh"]
    bootstrapProc.running = true
  }

  // "Try again" in the panel.
  function setupRetry() {
    if (bootstrapProc.running || manifestProc.running || probeProc.running) return
    _setupStart()
  }

  function _bootstrapLine(line) {
    var l = String(line).trim()
    if (l.indexOf("STEP ") === 0) {
      var rest = l.substring(5)
      var sp = rest.indexOf(" ")
      var text = sp >= 0 ? rest.substring(sp + 1) : rest
      if (text !== "") setupStep = text
    } else if (l.indexOf("BIN /") === 0) {
      _bootstrapBinary = l.substring(4)
    } else if (l.indexOf("ERROR ") === 0) {
      setupMessage = l.substring(6)
    }
  }

  Process {
    id: manifestProc
    command: ["cat", root.pluginDir + "/manifest.json"]
    stdout: StdioCollector {
      waitForEnd: true
      onStreamFinished: {
        var v = ""
        try { v = String(JSON.parse(text).version || "") } catch (e) {}
        root.pluginVersion = v
      }
    }
    onExited: function (exitCode) {
      if (exitCode !== 0 || root.pluginVersion === "") {
        root.setupState = "error"
        root.setupMessage = root.tr("Could not read the plugin version")
        return
      }
      probeProc.running = true
    }
  }

  // Prints the first candidate whose `version` matches the plugin's.
  Process {
    id: probeProc
    property string found: ""
    command: ["sh", "-c",
      "v=\"$1\"; shift; for c in \"$@\"; do case \"$c\" in /*) [ -x \"$c\" ] || continue ;; "
      + "*) c=\"$(command -v \"$c\" 2>/dev/null)\" || continue ;; esac; "
      + "o=\"$(\"$c\" version 2>/dev/null | head -n 1)\"; "
      + "if [ \"$o\" = \"qcyctl $v\" ]; then printf '%s\\n' \"$c\"; exit 0; fi; done; exit 1",
      "qcyprobe", root.pluginVersion, root._dataBinary,
      root._home + "/.local/bin/qcyctl", "qcyctl"]
    stdout: StdioCollector {
      waitForEnd: true
      onStreamFinished: probeProc.found = String(text).trim().split("\n")[0]
    }
    onExited: function (exitCode) {
      if (exitCode === 0 && probeProc.found !== "") {
        // Keep the daemon's plugin-dir record current (data dir only; no
        // download, no write under the plugin directory).
        markProc.command = ["bash", root.pluginDir + "/scripts/bootstrap.sh", "--mark-plugin"]
        markProc.running = true
        root._setupReady(probeProc.found)
      }
      else root._setupPrepare()
      probeProc.found = ""
    }
  }

  Process {
    id: markProc
  }

  // `omarchy plugin update` replaces the files and rescans, but this singleton
  // survives the reload, so its setup would never run again and the QML would
  // keep calling the old binary. manifest.json is watched read-only (nothing is
  // written under the plugin directory); when its version differs from the one
  // the setup ran with, or the setup is in "error", the setup starts over. The
  // debounce covers git writing several files; a setup already in flight is not
  // duplicated, the check is retried until it ends.
  FileView {
    id: manifestWatch
    path: root.pluginDir + "/manifest.json"
    watchChanges: true
    onFileChanged: manifestDebounce.restart()
    onLoaded: {
      var v = ""
      try { v = String(JSON.parse(text()).version || "") } catch (e) {}
      if (v === "" || Quickshell.env("QCYCTL")) return
      if (v === root.pluginVersion && root.setupState !== "error") return
      if (root._removing || bootstrapProc.running || manifestProc.running || probeProc.running) {
        manifestDebounce.restart()
        return
      }
      root._setupStart()
    }
  }

  Timer {
    id: manifestDebounce
    interval: 2000
    onTriggered: manifestWatch.reload()
  }

  Process {
    id: bootstrapProc
    stdout: SplitParser {
      onRead: function (data) { root._bootstrapLine(data) }
    }
    onExited: function (exitCode) {
      if (exitCode === 0) {
        root._setupReady(root._bootstrapBinary !== "" ? root._bootstrapBinary : root._dataBinary)
      } else {
        root.setupState = "error"
        if (root.setupMessage === "")
          root.setupMessage = root.tr("Could not prepare qcyctl (code {code})", { code: exitCode })
      }
    }
  }

  // ---------------------------------------------------------------- daemon
  //
  // `qcyctl daemon status --json`; "available" is false when this qcyctl has no
  // such command, and the Settings section then says so.
  property var daemon: null
  property bool daemonAvailable: true
  property bool daemonBusy: false      // an install/uninstall/removal is running
  property string daemonError: ""

  function daemonRefresh(done) {
    runOffline(["daemon", "status", "--json"], function (code, out, err) {
      var j = root._parseJson(out)
      if (code === 0 && j && typeof j === "object") {
        root.daemon = j
        root.daemonAvailable = true
      } else {
        root.daemon = null
        root.daemonAvailable = false
      }
      if (done) done()
    })
  }

  // After enabling/disabling the service the state is read back until it shows
  // what was asked (the daemon needs a moment to listen), a few times at most.
  property var _daemonWant: null      // { enable: bool } while settling
  property int _daemonTries: 0

  function _daemonSettled() {
    if (!_daemonWant) return true
    var d = daemon
    if (!d) return false
    // socket enabled: systemd holds the socket and starts the daemon on demand
    return _daemonWant.enable ? (!!d.serviceEnabled && (!!d.socketActive || !!d.running)) : !d.serviceEnabled
  }

  function _daemonSettle() {
    daemonRefresh(function () {
      if (root._daemonSettled() || root._daemonTries >= 8) {
        root._daemonWant = null
        return
      }
      root._daemonTries++
      daemonSettleTimer.restart()
    })
  }

  Timer {
    id: daemonSettleTimer
    interval: 700
    repeat: false
    onTriggered: root._daemonSettle()
  }

  // "Plugin x · Daemon y · qcyctl z" for the end of Settings.
  readonly property string versionsLine: {
    var dv = (daemon && daemon.running) ? (daemon.version || "?")
      : ((daemon && daemon.socketActive) ? tr("on demand") : tr("stopped"))
    var cv = (daemon && daemon.cliVersion) ? daemon.cliVersion : "?"
    return tr("Plugin {plugin} · Daemon {daemon} · qcyctl {cli}",
      { plugin: pluginVersion || "?", daemon: dv, cli: cv })
  }

  // The running daemon is older than this qcyctl (an update replaced the
  // binaries but not the process; a 0.1.5 daemon publishes no version at all). Display only: nothing restarts by itself.
  readonly property bool daemonOutdated: !!(daemon && daemon.running && daemon.cliVersion
    && (!daemon.version || daemon.version !== daemon.cliVersion))

  function daemonSetService(enable) {
    if (daemonBusy) return
    daemonBusy = true
    daemonError = ""
    runOffline(["daemon", enable ? "install-service" : "disable-service", "--json"],
      function (code, out, err) {
        root.daemonBusy = false
        if (code !== 0) root.daemonError = root._lastLine(err) || root.tr("The command failed (code {code})", { code: code })
        root._daemonWant = code === 0 ? { enable: enable } : null
        root._daemonTries = 0
        root._daemonSettle()
      })
  }

  // The three directories the plugin owns (XDG-aware like the Go side), each
  // absolute, free of "..", and ending in exactly "qcy-omarchy".
  function _ownedDirs() {
    var h = _home
    if (!h || h.charAt(0) !== "/" || h === "/") return []
    h = h.replace(/\/+$/, "")
    var cfg = Quickshell.env("XDG_CONFIG_HOME") || (h + "/.config")
    var cache = Quickshell.env("XDG_CACHE_HOME") || (h + "/.cache")
    var xs = [dataDir, cfg.replace(/\/+$/, "") + "/qcy-omarchy", cache.replace(/\/+$/, "") + "/qcy-omarchy"]
    return xs.filter(function (p) {
      return p.charAt(0) === "/" && /\/qcy-omarchy$/.test(p) && p.indexOf("..") === -1
    })
  }

  // Stops the daemon and the service, then deletes the data (which includes the
  // binaries in <data>/bin), config and cache.
  function removeData() {
    if (daemonBusy) return
    var dirs = _ownedDirs()
    if (dirs.length !== 3) { daemonError = tr("Refusing to remove: unexpected home directory"); return }
    daemonBusy = true
    daemonError = ""
    _removing = true
    // The daemon is asked to stop first: it runs from the binaries that are
    // about to be deleted (<data>/bin is inside the data directory).
    runOffline(["daemon", "stop"], function () {
      root.runOffline(["daemon", "uninstall-service", "--json"], function () {
        removeProc.command = ["rm", "-rf", "--"].concat(dirs)
        removeProc.running = true
      })
    })
  }

  Process {
    id: removeProc
    onExited: function (exitCode) {
      root.daemonBusy = false
      root._removing = false
      if (exitCode !== 0) {
        root.daemonError = root.tr("The command failed (code {code})", { code: exitCode })
        return
      }
      root.daemon = null
      root.setupState = "error"
      root.setupMessage = root.tr("QCY data removed. Try again to prepare it anew.")
    }
  }

  // ---------------------------------------------------------------- device

  readonly property var _devices: Bluetooth.devices ? Bluetooth.devices.values : []

  // The connected earbuds, matched by name. Re-evaluates whenever any device's
  // `connected` flips because the binding reads it.
  readonly property var device: {
    for (var i = 0; i < _devices.length; i++) {
      var d = _devices[i]
      if (!d || !d.connected) continue
      // Only a paired/bonded device: while the daemon holds a BLE session BlueZ
      // adds a session-only LE object with the same name (Paired and Bonded
      // false) that flips with every session and is not the audio device.
      // (a build exposing none of the properties keeps the old behaviour)
      var known = d.paired !== undefined || d.bonded !== undefined || d.trusted !== undefined
      if (known && !d.paired && !d.bonded && !d.trusted) continue
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
  // Model detection from the daemon (vendorId first, name as fallback). What a
  // model offers is what its panel declares; `tested` is informational only.
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
    if (!ready || _imageKey === "" || _imageTries >= 3) return
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
  property double _loadingSince: 0
  onLoadingChanged: _loadingSince = loading ? Date.now() : 0
  property string error: ""
  // True when the last failure was the daemon's "busy" answer: the earbuds
  // accept one BLE client at a time and another app (the phone) holds them.
  property bool busy: false
  // Age in seconds of a status served from the daemon's cache, or -1 when the
  // report on screen is fresh.
  property int staleAgeSeconds: -1
  readonly property string busyMessage: tr("The earbuds are in use by another app — check that no app on this or another device (phone, tablet, computer) is open, then refresh")
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
  // `auto` is true for the retry timer, which must keep counting its backoff.
  function refresh(auto) {
    if (!ready) {
      // Never silent for a manual refresh: say why nothing is read.
      if (auto !== true && !_removing)
        error = setupState === "error" && setupMessage !== "" ? setupMessage : tr("Preparing QCY…")
      return
    }
    // "Reading…" with nothing running that could end it: release it.
    if (loading && !statusProc.running && !setProc.running && _pendingSet === null
        && _run === null && _loadingSince > 0 && Date.now() - _loadingSince > _readWatchdogMs) {
      _refreshQueued = false
      loading = false
    }
    if (connected && _imageKey !== "" && Object.keys(images).length === 0
        && !imageProc.running) _loadImages()
    if (statusProc.running) {
      // A read that outlived its watchdog is killed with a visible error (the
      // button is clickable again); otherwise the click shows "loading".
      if (Date.now() - _statusStartedAt > _readWatchdogMs) _readTimedOut()
      return
    }
    if (setProc.running || _pendingSet !== null || _run !== null) {
      // A change is going out: read right after it, and say so meanwhile.
      _refreshQueued = true
      loading = true
      return
    }
    if (batteryProc.running) { batteryProc.signal(9); batteryProc.running = false }
    // A manual refresh always gets a full attempt, whatever the retry history.
    if (auto !== true) _retryCount = 0
    retryTimer.stop()
    error = ""
    _statusKilled = false
    _statusStartedAt = Date.now()
    statusProc.running = true
  }

  // True while the panel is open (set by the bar widget): the retry loop for a
  // busy/stale/unreadable state only runs then.
  property bool panelOpen: false
  property bool _refreshQueued: false
  property bool _statusKilled: false
  property bool _batteryKilled: false
  property real _statusStartedAt: 0
  readonly property int _readWatchdogMs: 25000
  readonly property int _periodicRetryMs: 20000
  readonly property string _timeoutMessage: tr("The earbuds did not answer in time")

  onPanelOpenChanged: {
    if (!panelOpen) { retryTimer.stop(); return }
    if (ready && (busy || error !== "") && !statusProc.running) refresh(true)
  }

  // A read that never ended: kill it, say so, and keep trying while open.
  function _readTimedOut() {
    if (statusProc.running) {
      _statusKilled = true
      statusProc.signal(9)
      statusProc.running = false
    }
    if (batteryProc.running) {
      _batteryKilled = true
      batteryProc.signal(9)
      batteryProc.running = false
    }
    error = _timeoutMessage
    _scheduleRetry(false)
    _drain()
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
    if (!ready) {
      if (done) { try { done(-1, "", "not ready") } catch (e) {} } else root.setFailed()
      return
    }
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
    // Plain setters get a short leash; callback runs (long operations) keep 60 s.
    setWatchdog.interval = done ? 60000 : _readWatchdogMs
    setWatchdog.restart()
  }

  // A setter with a callback that never exits would pin _run, settling and the
  // library phase; kill it and deliver whatever arrived.
  Timer {
    id: setWatchdog
    interval: 60000
    repeat: false
    onTriggered: {
      if (setProc.running) setProc.signal(9)
      if (root._run === null) {
        // Plain setter: onExited reports the failure; make sure the panel frees.
        setProc.running = false
        if (root._pendingSet === null) {
          root.error = root._timeoutMessage
          root.setFailed()
        }
        root._drain()
        return
      }
      root._joinRun(true)
    }
  }

  // ------------------------------------------------------------ find earbuds
  //
  // `qcyctl find --yes --events` plays a beeping tone on the earbuds' audio
  // sink at full sink volume and a loud tone, so it carries across a room (the
  // timeout is enforced by the CLI). It is a long-running process: JSON event lines come
  // on stdout, "stop" goes to its stdin and the CLI restores the volume and
  // the music before it exits. The caller must have shown the warning
  // "Remove the earbuds from your ears before starting" and got a "yes".
  property bool findRunning: false
  property string findStatus: "idle"   // idle | starting | playing | stopping
  property int findVolume: 0           // current sink volume in %, 0 = unknown
  property int findMaxVolume: 0
  property string findMessage: ""      // last error, "" when none

  function startFind() {
    if (!ready || findProc.running) return
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

  // ----------------------------------------------------------- hearing test
  //
  // `qcyctl hearing run --events` is a long-running process like find: JSON
  // lines on stdout, answers as lines on stdin, "abort" to stop (it restores
  // the volume and the music before it exits). It never writes to the
  // earbuds: the result is stored in a file that `hearing apply` writes later,
  // on an explicit confirmation (applyHearing).
  property bool hearingRunning: false
  property string hearingStatus: "idle"  // idle | starting | running | stopping
  property string hearingPhase: ""       // phase name of the last phase event
  property int hearingIndex: 0           // index of the tone waiting for an answer, 0 = none
  property string hearingEar: ""         // left | right
  property int hearingFreq: 0
  property real hearingProgress: 0
  property string hearingMessage: ""     // last error, "" when none
  property string hearingWarning: ""     // last warning, "" when none
  property var hearingResult: null       // { left, right, floorLeft, floorRight, masterGain, outFile }
  property string hearingApplyState: "idle"  // idle | running | ok | failed
  property string hearingApplyText: ""

  // `volume` is the reference sink volume in percent (80, 90 or 100).
  function startHearing(volume) {
    if (!ready || hearingProc.running || hearingApplyProc.running) return
    hearingMessage = ""
    hearingWarning = ""
    hearingResult = null
    hearingApplyState = "idle"
    hearingApplyText = ""
    hearingPhase = ""
    hearingIndex = 0
    hearingEar = ""
    hearingFreq = 0
    hearingProgress = 0
    hearingStatus = "starting"
    hearingRunning = true
    hearingProc.command = [root.binary, "hearing", "run", "--events",
      "--sink-volume", String(Math.round(Number(volume) || 90)) + "%"]
    hearingProc.running = true
  }

  function _hearingSend(line) {
    if (!hearingProc.running || hearingStatus === "stopping") return
    try { hearingProc.write(line + "\n") } catch (e) { /* the exit handler cleans up */ }
  }

  // Answer the tone being played; ignored when no tone is waiting.
  function answerHearing(heard) {
    if (hearingPhase !== "testing" || hearingIndex <= 0) return
    var idx = hearingIndex
    hearingIndex = 0
    _hearingSend((heard ? "heard " : "not-heard ") + idx)
  }

  function repeatHearing() {
    if (hearingPhase !== "testing" || hearingIndex <= 0) return
    _hearingSend("repeat")
  }

  // Ask the process to stop and wait for `stopped`; SIGTERM (same cleanup)
  // only if it does not answer.
  function stopHearing() {
    if (!hearingProc.running) return
    hearingStatus = "stopping"
    hearingIndex = 0
    try { hearingProc.write("abort\n") } catch (e) { hearingProc.signal(15) }
    hearingKillTimer.restart()
  }

  function _hearingEvent(line) {
    var ev = _parseJson(line)
    if (!ev || typeof ev.event !== "string") return
    if (ev.event === "phase") {
      hearingPhase = String(ev.name || "")
      if (hearingPhase !== "testing") hearingIndex = 0
      if (hearingStatus === "starting") hearingStatus = "running"
    } else if (ev.event === "tone") {
      hearingIndex = Number(ev.index) || 0
      hearingEar = String(ev.ear || "")
      hearingFreq = Number(ev.freq) || 0
      hearingProgress = Math.max(0, Math.min(1, Number(ev.progress) || 0))
      hearingWarning = ""
    } else if (ev.event === "answer") {
      hearingIndex = 0
    } else if (ev.event === "result") {
      var f = ev.floor || {}
      hearingResult = {
        left: Array.isArray(ev.left) ? ev.left : [],
        right: Array.isArray(ev.right) ? ev.right : [],
        floorLeft: Array.isArray(f.left) ? f.left : [],
        floorRight: Array.isArray(f.right) ? f.right : [],
        masterGain: Number(ev.masterGain) || 0,
        outFile: String(ev.outFile || "")
      }
      hearingProgress = 1
    } else if (ev.event === "warning") {
      hearingWarning = String(ev.message || "")
    } else if (ev.event === "error") {
      hearingMessage = String(ev.message || "")
    }
  }

  Timer {
    id: hearingKillTimer
    interval: 6000
    repeat: false
    onTriggered: if (hearingProc.running) hearingProc.signal(15)
  }

  Process {
    id: hearingProc
    stdinEnabled: true
    stdout: SplitParser {
      onRead: function (data) { root._hearingEvent(data) }
    }
    stderr: StdioCollector {
      id: hearingErr
      waitForEnd: true
    }
    onExited: function (exitCode) {
      hearingKillTimer.stop()
      if (exitCode !== 0 && root.hearingMessage === "") {
        root.hearingMessage = root._lastLine(hearingErr.text) !== "" ? root._lastLine(hearingErr.text)
          : tr("The hearing test stopped (code {code})", { code: exitCode })
      }
      root.hearingRunning = false
      root.hearingStatus = "idle"
      root.hearingIndex = 0
    }
  }

  // Writes the stored result to the earbuds (slot 11). `acceptFloor` adds
  // --accept-floor and must come from an explicit user choice. The run made
  // by the plugin always uses the default baseline, so --baseline is never
  // passed here.
  function applyHearing(acceptFloor) {
    var r = hearingResult
    if (!ready || !r || r.outFile === "" || hearingApplyProc.running || hearingProc.running) return
    hearingApplyState = "running"
    hearingApplyText = ""
    var cmd = [root.binary, "hearing", "apply", r.outFile, "--yes"]
    if (acceptFloor) cmd.push("--accept-floor")
    hearingApplyProc.command = cmd
    hearingApplyProc.running = true
    hearingApplyKill.restart()
  }

  // Forget the result; the file is removed only when it is under the cache
  // directory the CLI writes to.
  function discardHearing() {
    var r = hearingResult
    hearingResult = null
    hearingApplyState = "idle"
    hearingApplyText = ""
    hearingMessage = ""
    hearingWarning = ""
    hearingPhase = ""
    hearingProgress = 0
    if (r && _hearingFileOk(r.outFile) && !hearingRmProc.running) {
      hearingRmProc.command = ["rm", "-f", "--", r.outFile]
      hearingRmProc.running = true
    }
  }

  function _hearingFileOk(path) {
    var dir = Quickshell.env("HOME") + "/.cache/qcy-omarchy/"
    var p = String(path || "")
    return p.indexOf(dir + "hearing-") === 0 && p.slice(-5) === ".json"
      && p.slice(dir.length).indexOf("/") === -1 && p.indexOf("..") === -1
  }

  Timer {
    id: hearingApplyKill
    interval: 90000
    repeat: false
    onTriggered: if (hearingApplyProc.running) hearingApplyProc.signal(15)
  }

  Process {
    id: hearingRmProc
  }

  Process {
    id: hearingApplyProc
    stdout: StdioCollector {
      id: hearingApplyOut
      waitForEnd: true
    }
    stderr: StdioCollector {
      id: hearingApplyErr
      waitForEnd: true
    }
    onExited: function (exitCode) {
      hearingApplyKill.stop()
      var all = String(hearingApplyOut.text || "") + "\n" + String(hearingApplyErr.text || "")
      var unconfirmed = all.indexOf("NOT CONFIRMED") !== -1 || all.indexOf("MIXED STATE") !== -1
      if (exitCode === 0 && !unconfirmed) {
        root.hearingApplyState = "ok"
        root.hearingApplyText = tr("Saved and confirmed")
      } else {
        // The lines that carry the verdict and the restore command, in full.
        var keep = []
        var lines = all.split("\n")
        for (var i = 0; i < lines.length; i++) {
          var l = lines[i].trim()
          if (l === "") continue
          if (/NOT CONFIRMED|MIXED STATE|restore|refused|error|failed|could not/i.test(l)) keep.push(l)
        }
        if (keep.length === 0) keep.push(root._lastLine(all))
        root.hearingApplyState = "failed"
        root.hearingApplyText = keep.slice(-4).join("\n")
        if (root.hearingApplyText === "")
          root.hearingApplyText = tr("Could not save the result (code {code})", { code: exitCode })
      }
      // The earbuds and the library changed (or may have): read them again.
      root.libraryReload()
      root.refresh()
    }
  }

  // --------------------------------------------------------- battery (light)

  property var battery: null        // parsed `qcyctl battery --json`
  property bool batteryLoading: false

  // One light battery read; used on panel open and on a slow poll while open.
  function pollBattery() {
    if (!ready || batteryProc.running) return
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
    if (_refreshQueued) {
      _refreshQueued = false
      loading = false
      refresh(true)
      if (statusProc.running) return
    }
    if (_wantBattery && connected) pollBattery()
  }

  function _applyStatus(text) {
    if (setProc.running || _pendingSet) return
    var trimmed = text ? String(text).trim() : ""
    if (trimmed === "") return
    try {
      report = JSON.parse(trimmed)
      Qt.callLater(root._maybeImportHearing)
      updatedAt = Date.now()
      if (report && report.stale === true) {
        // Cached reading: the earbuds are held elsewhere right now. The data
        // is as old as the daemon says, not as old as this answer.
        var age = Math.max(0, Math.round(Number(report.ageSeconds) || 0))
        updatedAt = Date.now() - age * 1000
        staleAgeSeconds = age
        busy = true
        error = tr("{message} (showing the last reading, from {n} s ago)", { message: busyMessage, n: age })
      } else {
        staleAgeSeconds = -1
        busy = false
        error = ""
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
    if (wasBusy && panelOpen) {
      // Panel open: keep looking every ~20 s until the earbuds are free.
      retryTimer.interval = _periodicRetryMs
      retryTimer.restart()
      return
    }
    if (wasBusy) {
      if (_retryCount >= 1) return
      _retryCount = 1
      retryTimer.interval = _busyRetryMs
      retryTimer.restart()
      return
    }
    if (_retryCount >= _retryMax) {
      if (panelOpen) {
        retryTimer.interval = _periodicRetryMs
        retryTimer.restart()
      }
      return
    }
    _retryCount++
    retryTimer.interval = Math.min(30000, 5000 * Math.pow(2, _retryCount - 1))
    retryTimer.restart()
  }

  // The daemon's busy error reaches qcyctl's stderr verbatim.
  function _isBusyText(text) {
    if (!text) return false
    var t = String(text)
    // busy: "in use by another app"; not_found: "held by an app open on this or
    // another device". Both mean the earbuds are held elsewhere.
    return t.indexOf("another app") !== -1 || t.indexOf("another device") !== -1
  }

  Timer {
    id: retryTimer
    repeat: false
    onTriggered: {
      // While busy the BlueZ link flag says nothing about the BLE link: keep going.
      if (!root.connected && !root.busy) return
      // Adapter busy: look again later without spending an attempt.
      if (statusProc.running || setProc.running || batteryProc.running) restart()
      else root.refresh(true)
    }
  }

  // Detect state on connect/disconnect edges: clear stale reports on drop, read
  // on (re)connect.
  onReadyChanged: {
    if (ready) { error = ""; refresh() }
  }

  onConnectedChanged: {
    retryTimer.stop()
    _retryCount = 0
    if (connected) {
      refresh()
      pollBattery()
    } else {
      report = null
      battery = null
      updatedAt = 0
      error = ""
      busy = false
      staleAgeSeconds = -1
      _wantBattery = false
      _refreshQueued = false
      loading = false
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
    onRunningChanged: {
      root.loading = running || root._refreshQueued
      if (running) statusWatchdog.restart(); else statusWatchdog.stop()
    }
    onExited: function (exitCode) {
      if (root._statusKilled) {
        // Killed by the watchdog, which already reported it.
        root._statusKilled = false
        root._drain()
        return
      }
      // A read killed to make way for a click always exits non-zero, so the
      // failure is only worth reporting when no setter is taking over. This
      // used to be tracked with a flag that other callbacks had to clear.
      // Any failed read is reported, connected or not (a silent failure left
      // the old "Updated N min ago" on screen).
      var failed = exitCode !== 0 && !root.settling
      var wasBusy = failed && root._isBusyText(statusErr.text)
      if (wasBusy) {
        root.busy = true
        root.error = root.busyMessage
      } else if (failed) {
        root.busy = false
        root.error = tr("Could not read the earbuds (code {code})", { code: exitCode })
      }
      if (wasBusy && root.report !== null) {
        // Keep the last reading on screen, but say it is old.
        var ageS = Math.max(0, Math.round((Date.now() - (root.updatedAt || Date.now())) / 1000))
        root.staleAgeSeconds = ageS
        root.error = tr("{message} (showing the last reading, from {n} s ago)", { message: root.busyMessage, n: ageS })
      }
      if (failed && root.report === null) root.updatedAt = 0
      if (failed && !wasBusy) root.staleAgeSeconds = -1
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

  Timer {
    id: statusWatchdog
    interval: root._readWatchdogMs
    repeat: false
    onTriggered: root._readTimedOut()
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
    if (!wasBusy) {
      var keep = error
      refresh()
      // refresh() clears the error; the failure must stay visible until the
      // re-read lands.
      if (keep !== "") error = keep
    }
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
    onRunningChanged: {
      root.batteryLoading = running
      if (running) batteryWatchdog.restart(); else batteryWatchdog.stop()
    }
    onExited: function (exitCode) {
      root._batteryKilled = false
      root._drain()
    }
    stdout: StdioCollector {
      waitForEnd: true
      onStreamFinished: root._applyBattery(text)
    }
  }

  Timer {
    id: batteryWatchdog
    interval: root._readWatchdogMs
    repeat: false
    onTriggered: root._readTimedOut()
  }

  // ------------------------------------------------- generic panel functions
  //
  // `qcyctl fn list --json`: the controls the model's panel declares that no
  // other command covers (toggles, option lists, reset-type commands). The
  // list is empty for a model that declares none, and the section stays hidden.
  // It talks to the daemon, which serialises it with the other requests, so it
  // runs on the offline queue and never touches the adapter itself.
  property var functions: []        // [{cmdId, type, kind, label, options, state, text, restart, confirm, danger}]
  property string _functionsKey: ""
  property bool functionsLoading: false

  function loadFunctions(force) {
    var vendor = (report && report.model && report.model.vendorId !== undefined)
      ? String(report.model.vendorId) : ""
    if (!connected || vendor === "") {
      if (functions.length > 0) functions = []
      _functionsKey = ""
      return
    }
    if (!force && _functionsKey === vendor) return
    if (functionsLoading) return
    _functionsKey = vendor
    functionsLoading = true
    runOffline(["fn", "list", "--json"], function (code, out, err) {
      functionsLoading = false
      var list = []
      if (code === 0) {
        try {
          var j = JSON.parse(String(out || "").trim())
          if (Array.isArray(j)) list = j
        } catch (e) { /* keep the section hidden */ }
      }
      functions = list
    })
  }

  onReportChanged: loadFunctions(false)

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
    if (!(ready || _removing) || offProc.running || _offRun !== null || _offQueue.length === 0) return
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

  // Slot 11 (the hearing experience) active on the earbuds, with both ears read
  // back (status eq.ears): make the "Experiência auditiva" card from them, as if
  // the test had been done here. `import-device` never writes to the earbuds;
  // --expect-slot makes it save nothing if the active slot moved, and
  // --require-ears makes it save nothing unless both ears are there (no card
  // with half of the result).
  //
  // Without eq.ears nothing happens and no flag is set: an older daemon never
  // reports them (so nothing ever runs), a current one may report them on the
  // next status. A card from a previous version has no per-ear curves: when the
  // ears are reported it is replaced (the old copy goes to the backups
  // directory), once per session unless the command fails.
  property bool _hearingImportTried: false
  property bool _hearingMigrateTried: false
  function _reportHasEars() {
    var eq = report ? report.eq : null
    var e = eq ? eq.ears : null
    return !!e && Array.isArray(e.left) && Array.isArray(e.right) && e.left.length === 8 && e.right.length === 8
  }
  property bool _hearingLibraryLoadAsked: false
  function _maybeImportHearing() {
    if (!ready || !connected || busy || report === null) return
    if (hearingRunning || hearingApplyState === "running") return
    var eq = report.eq
    if (!eq || eq.preset === null || eq.preset === undefined || Number(eq.preset) !== 11) return
    if (!_reportHasEars()) return
    // The library is loaded lazily (Sound tab); load it now so the import does
    // not wait for the user to open that tab. libraryReload() calls back here.
    if (!libraryLoaded) {
      if (!_hearingLibraryLoadAsked) { _hearingLibraryLoadAsked = true; libraryReload() }
      return
    }
    var entry = null
    for (var i = 0; i < library.length; i++) {
      if (library[i].id === "experiencia-auditiva") { entry = library[i]; break }
    }
    if (entry !== null) {
      // Seen present: if the user deletes it later, it is not brought back.
      _hearingImportTried = true
      if (entry.ears || _hearingMigrateTried || entry.source !== "device-import" || settling) return
      _hearingMigrateTried = true
      runOffline(["library", "import-device", "Experiência auditiva", "--expect-slot", "11", "--require-ears", "--replace", "--backup", "--json"],
        root._hearingImportDone)
      return
    }
    if (_hearingImportTried || settling) return
    _hearingImportTried = true
    runOffline(["library", "import-device", "Experiência auditiva", "--expect-slot", "11", "--require-ears", "--json"],
      root._hearingImportDone)
  }
  function _hearingImportDone(code, out, err) {
    if (code === 0) { root.libraryReload(); return }
    // Transient failure (busy, timeout, slot moved, ears not read): try again
    // later, a few times only, spaced out.
    if (++root._hearingImportFails < 3) hearingImportRetry.restart()
  }
  property int _hearingImportFails: 0
  Timer {
    id: hearingImportRetry
    interval: 30000
    repeat: false
    onTriggered: { root._hearingImportTried = false; root._hearingMigrateTried = false; root._maybeImportHearing() }
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
      root._maybeImportHearing()
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
      msg = tr("Earbuds not found: off, out of range, or an app open on this or another device is using them.")
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

  // Saves what the earbuds report as slot 11 (read only, nothing is written to
  // them; `--expect-slot 11` saves nothing if the active slot moved). mode
  // "replace" overwrites the hearing-experience entry (backup: copy the old one
  // to the backups first); mode "new" stores a user curve called `name`.
  function libraryImportDevice(name, mode, backup, done, slot) {
    var args = ["library", "import-device", name, "--expect-slot", String(slot === undefined ? 11 : slot), "--json"]
    if (mode === "replace") {
      args.push("--replace")
      if (backup) args.push("--backup")
    } else {
      args.push("--user")
    }
    runOffline(args, function (code, out, err) {
      if (code === 0) {
        root.libraryReload()
        if (done) done(true, "")
        return
      }
      var clash = /already exists/i.test(err)
      var msg = clash
        ? tr("An effect with that name already exists. Choose another name.")
        : (root._lastLine(err) || tr("The command failed (code {code})", { code: code }))
      if (done) done(false, msg, clash)
    })
  }

  // Does the EQ the earbuds report match this library entry? The rule lives in
  // the CLI (`library matches`); done(ok, match).
  // `slot` is the active slot and `ears` the status eq.ears (both optional): a
  // hearing-test entry only matches slot 11, and with the ears both are compared.
  function libraryMatches(id, master, bands, done, slot, ears) {
    var args = ["library", "matches", id, "--master", Number(master).toFixed(2), "--bands", JSON.stringify(bands)]
    if (slot !== undefined && slot !== null && slot >= 0) args.push("--slot", String(slot))
    if (ears) args.push("--ears", JSON.stringify(ears))
    runOffline(args,
      function (code, out, err) {
        var j = root._parseJson(out)
        if (done) done(code === 0 && !!j, !!(j && j.match))
      })
  }

  // Which of the user's own curves is the EQ the earbuds report? done(ok, id)
  // with id "" when none is.
  function libraryMatchesAnyUser(master, bands, done) {
    runOffline(["library", "matches", "--any-user", "--master", Number(master).toFixed(2), "--bands", JSON.stringify(bands)],
      function (code, out, err) {
        var j = root._parseJson(out)
        if (done) done(code === 0 && !!j, j && j.match && j.id ? String(j.id) : "")
      })
  }

  // Moves a user curve to the library's .trash (recoverable by hand).
  function libraryDelete(id) {
    if (id === "experiencia-auditiva") _hearingImportTried = true
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
