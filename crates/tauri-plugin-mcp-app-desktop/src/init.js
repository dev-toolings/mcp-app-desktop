// mcp-app-desktop bridge, injected before the page's own scripts in every
// webview of an app started with MCP_APP_DESKTOP_BRIDGE_PORT. It only
// records: console messages, uncaught errors, unhandled rejections and the
// Tauri IPC calls, in a bounded buffer the server drains through `eval`.
// Everything that drives the page is installed later by the server itself.
(function () {
  if (window.__MCP_APP_DESKTOP__) return;
  var MAX = 1000;
  var MAX_TEXT = 4000;
  var state = {
    v: 1,
    // boot identifies this document: a reload or a navigation starts a new
    // buffer, and the server resets its cursor when boot changes.
    boot: Date.now().toString(36) + Math.random().toString(36).slice(2, 8),
    seq: 0,
    logs: [],
  };

  function clip(s) {
    s = String(s);
    return s.length > MAX_TEXT ? s.slice(0, MAX_TEXT) + '…' : s;
  }

  function show(v) {
    if (typeof v === 'string') return v;
    if (v instanceof Error) return v.stack || v.name + ': ' + v.message;
    try {
      var seen = new WeakSet();
      var out = JSON.stringify(v, function (_k, x) {
        if (typeof x === 'bigint') return x.toString() + 'n';
        if (typeof x === 'function') return '[Function ' + (x.name || 'anonymous') + ']';
        if (x && typeof x === 'object') {
          if (seen.has(x)) return '[Circular]';
          seen.add(x);
          if (typeof Element !== 'undefined' && x instanceof Element) return '<' + x.tagName.toLowerCase() + '>';
        }
        return x;
      });
      return out === undefined ? String(v) : out;
    } catch (_e) {
      return String(v);
    }
  }

  function add(source, level, text, location) {
    state.seq++;
    var e = { seq: state.seq, ts: Date.now(), source: source, level: level, text: clip(text) };
    if (location) e.location = location;
    state.logs.push(e);
    if (state.logs.length > MAX) state.logs.splice(0, state.logs.length - MAX);
  }
  state.add = add;

  // drain returns the entries after seq `since` of document `boot` (all of
  // them when the document changed), as a JSON string.
  state.drain = function (boot, since) {
    var from = boot === state.boot ? since : 0;
    var out = [];
    for (var i = 0; i < state.logs.length; i++) if (state.logs[i].seq > from) out.push(state.logs[i]);
    return JSON.stringify({ boot: state.boot, seq: state.seq, entries: out });
  };

  ['log', 'info', 'warn', 'error', 'debug'].forEach(function (level) {
    var orig = console[level];
    if (typeof orig !== 'function') return;
    console[level] = function () {
      try {
        var parts = [];
        for (var i = 0; i < arguments.length; i++) parts.push(show(arguments[i]));
        add('console', level === 'log' ? 'info' : level, parts.join(' '));
      } catch (_e) {}
      return orig.apply(this, arguments);
    };
  });

  window.addEventListener('error', function (ev) {
    var where = ev.filename ? ev.filename + ':' + ev.lineno + ':' + ev.colno : '';
    var msg = ev.error ? show(ev.error) : ev.message;
    add('exception', 'error', msg || 'error', where);
  });
  window.addEventListener('unhandledrejection', function (ev) {
    add('exception', 'error', 'unhandled rejection: ' + show(ev.reason));
  });

  // IPC: Tauri defines __TAURI_INTERNALS__.invoke as a read-only property,
  // but its transport is fetch("ipc://localhost/<cmd>") (http://ipc.localhost
  // on Windows), looked up on window at every call: wrapping fetch records
  // every command with its duration and outcome.
  var IPC_URL = /^(?:ipc:\/\/localhost\/|https?:\/\/ipc\.localhost\/)([^?#]*)/;
  var origFetch = window.fetch;
  if (typeof origFetch === 'function') {
    window.fetch = function (input, init) {
      var url = typeof input === 'string' ? input : input && input.url ? input.url : String(input);
      var m = IPC_URL.exec(url);
      if (!m) return origFetch.apply(this, arguments);
      var cmd = m[1];
      try {
        cmd = decodeURIComponent(cmd);
      } catch (_e) {}
      var quiet = cmd.indexOf('plugin:event|') === 0 || cmd.indexOf('plugin:__TAURI_CHANNEL__') === 0;
      var argText = init && typeof init.body === 'string' ? init.body : '';
      if (argText.length > 300) argText = argText.slice(0, 300) + '…';
      var started = Date.now();
      var p = origFetch.apply(this, arguments);
      p.then(
        function (res) {
          var ok = res.headers.get('Tauri-Response') !== 'error' && res.ok;
          var ms = Date.now() - started;
          if (ok) {
            add('ipc', quiet ? 'debug' : 'info', 'invoke ' + cmd + '(' + argText + ') ok in ' + ms + ' ms');
          } else {
            res
              .clone()
              .text()
              .then(
                function (body) {
                  add('ipc', 'error', 'invoke ' + cmd + '(' + argText + ') failed in ' + ms + ' ms: ' + body);
                },
                function () {
                  add('ipc', 'error', 'invoke ' + cmd + '(' + argText + ') failed in ' + ms + ' ms');
                }
              );
          }
        },
        function (err) {
          add('ipc', 'error', 'invoke ' + cmd + ' transport failed: ' + show(err));
        }
      );
      return p;
    };
  }

  Object.defineProperty(window, '__MCP_APP_DESKTOP__', { value: state, configurable: false, enumerable: false, writable: false });
})();
