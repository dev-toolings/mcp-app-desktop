# mcp-app-desktop

An MCP server dedicated to designing, driving and debugging **desktop
applications built with Electron, Tauri or GPUI** from an agent (Claude Code,
Codex, any MCP harness). It recognises the framework by itself and adapts its
tool list to the sessions it holds. The page side is
[ghostchrome](https://github.com/dev-toolings/ghostchrome)'s renderer engine
(accessibility snapshots with refs, strict locators, mutation diffs,
console/network observers); the host side launches a project, a `.app` or an
executable in isolation, runs the project's own dev loop with automatic
re-attach after restarts, tears process trees down, and talks to the
framework's own side: the Node inspector of Electron's main process, the
commands and events of a Tauri app, or the commands a GPUI app registers.

The server reaches each kind of app through the channel it actually has:

```
  agent (MCP client)
     │ stdio
     ▼
  mcp-app-desktop ── internal/mcp       common tools + framework tools
       │                                (tools/list_changed as sessions open/close)
       ├── internal/desktop   launch / attach / dev loop / logs / detection
       │
       ├── internal/driver    one Window interface, two backends:
       │      │
       │      ├── cdpwin ── CDP ────────────►  Electron renderers
       │      │                                Tauri on Windows (WebView2)
       │      │
       │      └── tauri ── loopback bridge ─►  tauri-plugin-mcp-app-desktop
       │                   (NDJSON, token)     Tauri on macOS / Linux (WKWebView,
       │                          │            WebKitGTK: no DevTools protocol)
       │                          │
       │                          └─────────►  gpui-mcp-app-desktop
       │                                       GPUI (no webview: native agent)
       │
       ├── Node inspector ──────────────────►  Electron main process (eval_main)
       │
       └── internal/flow      Maestro-style YAML flows (run_flow, CLI)
```

Electron and WebView2 expose the Chrome DevTools Protocol, so they get the
full engine. WebKit webviews do not: there, a page agent injected by the
bridge reads the accessibility tree and acts with DOM events, behind the same
tool contract (see [Tauri](#tauri)). A GPUI app has no webview at all: its
bridge speaks the same protocol and runs the agent's operations natively on
the window's accessibility tree, with real GPUI input events (see
[GPUI](#gpui)).

## Install

As a Claude Code plugin (MCP server + skill, with the binary for macOS
universal and Windows x64 bundled: nothing else to install):

```
/plugin install mcp-app-desktop --marketplace dev-toolings/mcp-app-desktop
```

On older Claude Code versions: `/plugin marketplace add dev-toolings/mcp-app-desktop`,
then `/plugin install mcp-app-desktop@dev-toolings`.

For other MCP hosts, download the archive of your platform from the
[GitHub releases](https://github.com/dev-toolings/mcp-app-desktop/releases)
(checksums in `SHA256SUMS`) and register `mcp-app-desktop mcp` over stdio, or
build from source:

From source, with the CLI installer (also registers Codex and writes the skill
to `~/.claude/skills` and `~/.codex/skills`; do not combine it with the
plugin, or Claude Code gets the server and the skill twice):

```bash
go build -o bin/mcp-app-desktop ./cmd/mcp-app-desktop
./bin/mcp-app-desktop install --dry-run   # prints every step, writes nothing
./bin/mcp-app-desktop install             # copy to ~/.local/bin, register, install skills
./bin/mcp-app-desktop install --uninstall # remove registrations, skills and the copy
```

`install` copies the binary to `~/.local/bin/mcp-app-desktop` (`--dest DIR`, or
`--bin PATH` to register an executable in place), registers it with Claude
Code (`claude mcp add --scope user`) and with Codex when its CLI is on PATH
(`--no-codex` to skip), replacing an existing entry, and writes the skill to
`~/.claude/skills/mcp-app-desktop` and `~/.codex/skills/mcp-app-desktop`.
A registration, skill or binary left by the former name `mcp-electron` is
removed on the way, so an agent never runs both. `--uninstall` reverses all
of it and verifies with `claude mcp get` /
`codex mcp get` that the entry is gone. `--dry-run` applies to both and never
writes, copies, removes or registers anything.

Manual registration: run `mcp-app-desktop mcp` over stdio. Flags: `--artifacts
DIR` for screenshots and flow outputs (default
`~/Library/Caches/mcp-app-desktop/sessions`), `--max-sessions N`,
`--idle-timeout D` (see below), `--static-tools` (list every tool from the
start, for a host that ignores `tools/list_changed`; also
`MCP_APP_DESKTOP_STATIC_TOOLS=1`). `stop` deletes the session's artifacts
(`keep_artifacts: true` keeps them), and a starting server removes session
directories left there for more than 24 hours by a server that died.

## Resource control

Every session is a whole process tree (Electron: main, GPU, renderers,
helpers; Tauri: the app and its WebKit processes, or the whole `tauri dev`
toolchain), so the server keeps them bounded:

- **Session cap**: 3 open sessions by default (`--max-sessions`); `launch`
  and `attach` refuse beyond it and name the sessions to stop.
- **Idle stop**: a `target` launch with no tool or flow access for 30 minutes
  (`--idle-timeout`, `0` disables) is stopped and the reason is logged.
  A dev `command` session (the developer's working app) and attached apps
  are never stopped for idleness.
- **Teardown**: `stop`, the idle timer, and the server's own exit (stdin
  closed, SIGTERM) terminate the full process tree top-down (SIGTERM, then
  SIGKILL after 10 s), including helpers that re-parented themselves, and
  delete the private profile.
- **Dev commands outlive the server**: a `command` session writes its output
  to files and a state file under `<cache>/mcp-app-desktop/dev/`. When the
  server exits it releases the app instead of stopping it, and the next
  `launch` of the same command in the same directory adopts it (same
  process, same ports) instead of starting a second copy. Only `stop` ends
  it. A recycled pid is never adopted (pid, process group and command line
  must all match). Not on Windows.
- **Parallel calls**: each Claude or Codex session runs its own server
  (stdio), so servers never wait for each other. Inside one server, up to 32
  tool calls run at once, sessions are independent, and the actions of one
  session (click, type, press, wait_for, navigate…) take turns: an action
  that finds another one still running after 15 s answers « session busy »,
  naming it and its duration, instead of queueing behind a stuck window.
  `snapshot`, `screenshot`, `eval`, `status`, `logs` and `trace` never wait
  for an action, so they show what holds it; `status` prints the busy action.
- **Measurement**: `status` reports each session's live process count,
  resident memory and CPU share (one `ps` scan, at most once per second) and
  warns above 2 GB.
- **Bounded records**: the descendant record forgets processes that exited.
- **Bounded buffers**: process output rings (3000 / 5000 lines, lines cut at
  a fixed width), main-process console, per-window observer history (1000
  events), per-window request table (2000 entries, oldest finished dropped),
  `logs` answers (2000 entries max).
- **Bounded captures**: full-page screenshots refuse above 32 Mpx; use the
  viewport, an element, or scroll in steps.
- **Light polling**: process tree rescanned every 5 s, debugging endpoint
  probed every 3 s while healthy (every second while restarting), waits poll
  the DOM at the engine's intervals and stop as soon as the calling tool's
  context is cancelled.
- **Bridge limits** (Tauri): 8 connections, 64 requests in flight, 32 MiB
  per message; the page agent keeps 1000 log entries per document, drained
  by the server every 2 s.
- **Tests**: the engine suite launches one headless Chrome for the whole
  package; the end-to-end suites are opt-in.

## Tools

| Group | Tools |
|---|---|
| Session | `launch`, `attach`, `status`, `windows`, `logs`, `stop`, `trace` |
| Page | `snapshot`, `click`, `type`, `fill_form`, `select`, `press`, `hover`, `drag`, `scroll`, `wheel`, `upload`, `wait_for`, `screenshot`, `eval`, `navigate`, `reload`, `dialog` |
| Electron | `eval_main` |
| Tauri | `invoke`, `emit` |
| GPUI | `invoke` |
| Flows | `run_flow` |

The tool list is dynamic: the framework tools are listed only while a session
of their framework is open, and the server sends `tools/list_changed` when
that changes (`--static-tools` lists everything; a framework tool called
without its session explains what to launch).

Full reference: [plugin/skills/mcp-app-desktop/references/tools.md](plugin/skills/mcp-app-desktop/references/tools.md).
Agent-facing procedure: [plugin/skills/mcp-app-desktop/SKILL.md](plugin/skills/mcp-app-desktop/SKILL.md).

### Targets

`@N` refs from the last `snapshot`, a unique CSS selector, or
`getByRole("button", { name: "Save" })`, `getByText("Save")`,
`getByLabel("Email")`. Names and texts may be regexes
(`getByRole("button", { name: /^Mode : / })`) or exact (`exact: true`).
`getByText` keeps the innermost matching element and prefers an exact match,
so a button wrapped in nested divs is still unique. Actions wait for the
target to be visible and enabled, then return a diff of the interactive
elements; `click` first hovers a per-row button that only shows on `:hover`
(`hover_first`). `wait_for` with `state: hidden` holds when no element
matching the target is visible.

Pass `modifiers: ["cmd"]` for Cmd-click or `modifiers: ["shift"]` for
Shift-click. The list accepts the `press` aliases for Cmd, Ctrl, Shift and Alt
(including `option`), plus `ControlOrMeta` for Cmd on macOS and Ctrl elsewhere.
Omit the list or pass `[]` for a plain click. Modified clicks on a GPUI or Tauri
bridge require rebuilding the app with a crate that advertises support.

## Launching

The framework comes from the target (`node_modules/electron`, a
`src-tauri/tauri.conf.json`, `Electron Framework.framework` in a bundle, the
`__TAURI_INTERNALS__` marker in a binary, a `Cargo.toml` depending on
`gpui-mcp-app-desktop`, that crate's name and `MCP_APP_DESKTOP_BRIDGE_PORT`
in a binary) or, for a dev command, from whatever answers first: the DevTools
port or the bridge, whose hello tells Tauri from GPUI. `framework:
electron|tauri|gpui` forces it.

- **Electron project directory** (`package.json` + `node_modules/electron`):
  runs the local Electron binary with `.` as the app. The app gets a private
  `--user-data-dir` under `/tmp` and `--remote-debugging-port` /
  `--inspect` on free loopback ports.
- **Tauri project directory** (`src-tauri/tauri.conf.json`, the project root
  or `src-tauri` itself): runs `tauri dev` with the project's package manager
  (`bun run tauri dev` when `package.json` has a `tauri` script, `bunx tauri
  dev` / `npx` / `pnpm exec`… after its lockfile, `cargo tauri dev`
  otherwise). The first build can take minutes: the startup wait defaults to
  15 minutes for a Tauri dev command.
- **GPUI project directory** (a Cargo package depending on
  `gpui-mcp-app-desktop`, or a workspace with one such member): runs the
  command its `[package.metadata.mcp-app-desktop] dev-command` (or
  `[workspace.metadata…]`) names, e.g. `["make", "run"]`, else `cargo run`,
  with `--features <f>` when the bridge is an optional dependency that
  feature turns on (`automation` preferred). The manifest is read with
  `cargo metadata --no-deps`. Startup waits up to 15 minutes, as for Tauri.
- **`.app` bundle or executable**: Electron, Tauri (a debug build with the
  plugin) or GPUI (a build with the bridge), recognised as above.
- **Dev command** (`command: ["bun","run","dev"]`): the server exports
  `MCP_APP_DESKTOP_CDP_PORT`, `MCP_APP_DESKTOP_INSPECT_PORT`,
  `MCP_APP_DESKTOP_BRIDGE_PORT` and `MCP_APP_DESKTOP_BRIDGE_TOKEN` (on
  Windows also `WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS` with the DevTools
  port). An Electron dev script must pass the first two to Electron, e.g.
  `electron --remote-debugging-port=$MCP_APP_DESKTOP_CDP_PORT --inspect=127.0.0.1:$MCP_APP_DESKTOP_INSPECT_PORT .`;
  a Tauri or GPUI app reads the bridge variables through its bridge crate,
  nothing to pass.
  `${MCP_APP_DESKTOP_CDP_PORT}` and the others expand inside `command` and
  `env`; the former `MCP_ELECTRON_CDP_PORT` / `MCP_ELECTRON_INSPECT_PORT`
  are still exported and expanded. When the dev loop restarts the app, the
  session reports `restarting`, then re-attaches on its own; `status` shows
  the restart count.
- **Private profile** (a dev command, a built Tauri or GPUI app): Electron has
  `--user-data-dir`; a Tauri app keeps its data under its bundle identifier,
  and many apps read their data directory from a variable of their own. A
  launch that names `${MCP_APP_DESKTOP_PROFILE_DIR}` or
  `${MCP_APP_DESKTOP_PROFILE_ID}` in `command`, `args` or `env` gets a
  private directory (0700) and a unique identifier segment
  (`mcp` + 12 hexadecimal digits), e.g.
  `command: ["bun","run","tauri","dev","--config","{\"identifier\":\"com.example.app.${MCP_APP_DESKTOP_PROFILE_ID}\"}"]`
  with `env: {"APP_DATA_DIR": "${MCP_APP_DESKTOP_PROFILE_DIR}/data"}`. `stop`
  deletes the directory and the entries of the system's application data
  directories whose name ends with that segment (the app data, cache,
  WebKit and saved state of that identifier), and lists them in
  `profile_removed`; an identifier it did not generate is never matched. The
  profile follows a dev command across servers (the one that adopts the
  command removes it), is kept when the app could not be stopped, and is
  removed at the next launch of the same command when the command died
  without `stop`. Nothing is created for a launch that names neither
  placeholder; an Electron target refuses them (it has `isolate`).
- **Attach**: `attach` with `http://127.0.0.1:PORT` of an Electron app
  started with `--remote-debugging-port`, of a Tauri app on Windows started
  with WebView2's, or the bridge port of a Tauri or GPUI app (`token` when
  the app was given one). A dev script that opens the bridge itself can write
  `{"endpoint": "127.0.0.1:PORT", "token": "…"}` to a file of mode 0600:
  `attach` with `bridge_file` reads it, so the token goes from the file to
  the bridge without entering the conversation (a symbolic link, or a file
  other users can read, is refused). The trace never keeps a `token`
  argument, nor the `env` values of a `launch` whose name tells a secret
  (`TOKEN`, `SECRET`, `PASSWORD`, `API_KEY`…). Attached apps are never killed. Attaching to an endpoint a
  session already drives returns that session, which follows the app's
  restarts on the same port by itself.
- **Hidden windows** (CDP): every window gets focus emulation (plus
  `Page.setWebLifecycleState active`), so a minimised, hidden or covered
  window reads as visible and focused and keeps its animation frames and
  timers at full rate; the window is never raised and OS focus is never
  taken. The emulation belongs to the connection and ends with it.

Apps that relocate their own data directory need their variable in `env`.

## Tauri

On macOS (WKWebView) and Linux (WebKitGTK) the webview has no DevTools
protocol, so the app registers a small plugin in its **debug builds**:

```toml
# src-tauri/Cargo.toml
[dependencies]
tauri-plugin-mcp-app-desktop = { git = "https://github.com/dev-toolings/mcp-app-desktop" }
```

```rust
let mut builder = tauri::Builder::default();
#[cfg(debug_assertions)]
{ builder = builder.plugin(tauri_plugin_mcp_app_desktop::init()); }
```

The plugin is inert unless the server launched the app (it sets
`MCP_APP_DESKTOP_BRIDGE_PORT`): it then listens on that loopback port only,
requires the token the server generated, lists the webviews, evaluates
scripts, takes native screenshots (macOS) and records the console, uncaught
errors and IPC calls (`logs` source `ipc`). Details:
[crates/tauri-plugin-mcp-app-desktop](crates/tauri-plugin-mcp-app-desktop/README.md).
On Windows nothing is needed: WebView2 opens its DevTools port from the
environment and the session prefers CDP even when the plugin is also there.

What differs from CDP when driving through the bridge:

- Actions are DOM events (`isTrusted: false`): an app that checks
  `isTrusted`, or a style that relies on CSS `:hover`, does not react as to a
  real mouse. Typing uses `insertText` and native value setters, so React and
  Vue inputs update; keys fire `keydown`/`keyup` and their default action
  (Enter submits, Tab moves focus).
- `screenshot`: viewport or element, PNG or JPEG (WebP falls back to JPEG),
  macOS only; `full_page` is CDP only.
- `eval` works under a strict CSP (no `eval`), and a native Tauri dialog
  (plugin `dialog`) is not a JavaScript dialog: stub it with `invoke` or in
  Rust.
- `invoke` / `emit` call the app's commands and events from a window, with
  its capabilities. They also work on Windows over CDP.

## GPUI

A GPUI app has no webview and no JavaScript. It calls
`gpui-mcp-app-desktop` in its debug builds (the bridge refuses to open in a release build):

```toml
# Cargo.toml
[dependencies]
gpui-mcp-app-desktop = { git = "https://github.com/dev-toolings/mcp-app-desktop", optional = true }

[features]
automation = ["dep:gpui-mcp-app-desktop"]

# optional: how launch runs the project (default: cargo run --features automation)
[package.metadata.mcp-app-desktop]
dev-command = ["make", "run"]
```

```rust
application().run(|cx| {
    #[cfg(feature = "automation")]
    gpui_mcp_app_desktop::init(cx);
    // optional: what invoke can call (the stand-in for eval)
    #[cfg(feature = "automation")]
    gpui_mcp_app_desktop::register(cx, "state", |_args, _window, _cx| Ok(serde_json::json!({})));
    // …
});
```

Like the Tauri plugin, the bridge is inert unless `MCP_APP_DESKTOP_BRIDGE_PORT`
is set, then listens on that loopback port only with the server's token, and
prints `[gpui-mcp-app-desktop] bridge listening on 127.0.0.1:PORT` on stderr.
It reads the window through its accessibility tree: **only elements with an
`.id(...)` and a `.role(...)` appear in a snapshot** (gpui-component controls
set both); give them an `.aria_label(...)` when their text is not their name.
Details: [crates/gpui-mcp-app-desktop](crates/gpui-mcp-app-desktop/src/lib.rs).

What differs:

- Actions are real GPUI input events (mouse, keys, text input), so hover and
  focus behave as with a person; targets are `@ref`, `getByRole`, `getByText`
  or `getByLabel` (no CSS: a CSS selector is refused).
- No JavaScript: `eval`, `navigate`, `reload`, `select`, `upload` and `emit`
  fail with what to do instead. `invoke` calls a command the app registered
  with `gpui_mcp_app_desktop::register`, on the main thread with the window
  asked for; an unknown command answers the list of registered ones.
- `logs`: the process output (stdout, stderr); there is no console.
- `screenshot`: the whole window (physical pixels), PNG or JPEG; no element
  target, no `full_page`.
- A window covered by others, minimised or hidden is read and captured all the
  same: macOS draws no frame for it (its accessibility tree would never start),
  so the bridge draws the frames it needs itself; they show once the window
  does.
- A named textbox shows its name in the rendered snapshot, as for the other
  frameworks; `wait_for` with `text` also matches what was typed.

## Flows

Maestro syntax, desktop subset: `tapOn`, `doubleTapOn`, `inputText`,
`eraseText`, `pressKey`, `assertVisible`, `assertNotVisible`, `scroll`,
`scrollUntilVisible`, `waitForAnimationToEnd`, `extendedWaitUntil`,
`takeScreenshot`, `evalJs`, `assertJs`, `evalMain`, `assertMain`,
`repeat`, `runFlow`. Selectors: text, `role` + `text`, `label`, `id`,
`css`, `ref`. Element-based `tapOn` and `doubleTapOn` accept the same
`modifiers` list as `click`. See [plugin/skills/mcp-app-desktop/references/flows.md](plugin/skills/mcp-app-desktop/references/flows.md)
and [examples/flows/escape-create-agent.yaml](examples/flows/escape-create-agent.yaml).

CI:

```bash
mcp-app-desktop run-flow examples/flows/escape-create-agent.yaml \
  --target path/to/app --env ESCAPE_TEST_DATA=$(mktemp -d /tmp/esc-XXXX)
```

## Development

```bash
go test -short ./...        # unit tests (no browser)
go test ./...               # engine tests launch a headless system Chrome

# Electron: any Electron project directory
MCP_APP_DESKTOP_E2E_TARGET=path/to/electron-app \
go test -count=1 -v -run 'TestEndToEndElectron|TestE2E' ./internal/mcp/ ./internal/desktop/ ./cmd/mcp-app-desktop/

# Tauri: build the fixture (debug, with the plugin), then run its suites
(cd examples/tauri-fixture/src-tauri && cargo build)
MCP_APP_DESKTOP_E2E_TAURI=1 \
go test -count=1 -v -run 'Tauri' ./internal/mcp/ ./internal/desktop/

# GPUI: a binary built with gpui-mcp-app-desktop (launched with a private
# CODESCAPE_PROFILE, the variable codescape reads for its data)
MCP_APP_DESKTOP_E2E_GPUI=path/to/codescape \
go test -count=1 -v -run 'GPUI' ./internal/mcp/
```

`MCP_APP_DESKTOP_E2E_ENV` (`KEY=VALUE,…`, `@tmp` for a fresh directory per
scenario) and `MCP_APP_DESKTOP_E2E_FLOW` (a flow to replay) adapt the
Electron suite to an app. The end-to-end scenarios open the app on screen and
cover: the tool loop and a flow (`internal/mcp`), the tool list following
the sessions, `attach` leaving the app alive on `stop`, a hard crash of the
main process (state `exited`, helpers and profile cleaned), a dev loop that
restarts the app (state `restarting`, automatic re-attach, restart counted;
for Tauri also the adoption of the dev command by a new server), a second
window driven by its own index, and the server stopping its apps when the
host closes stdin (`cmd/mcp-app-desktop`). The fake bridge
(`internal/tauri/tauritest`, `StartGPUI` for a GPUI one) covers the Tauri and
GPUI tools without a window.

Layout: `internal/engine` (renderer engine from ghostchrome, attach-only
`Browser`), `internal/driver` (the `Window` interface and the CDP backend
`cdpwin`), `internal/tauri` (bridge client, page agent, log poller),
`internal/desktop` (sessions, detection, process tree, inspector),
`internal/mcp` (tools), `internal/flow` (YAML flows), `cmd/mcp-app-desktop`
(CLI), `crates/tauri-plugin-mcp-app-desktop` (the Rust plugin),
`crates/gpui-mcp-app-desktop` (the GPUI bridge),
`examples/tauri-fixture` (Tauri app of the end-to-end tests), `plugin/`
(Claude Code plugin: manifest, `.mcp.json`, and the skill, also embedded in
the binary by `plugin/skills`), `.claude-plugin/` (the marketplace),
`npm/mcp-app-desktop` (npm launcher, unpublished), `scripts/release.ts`.

## Release

The marketplace (`.claude-plugin/marketplace.json` on `main`) serves the
plugin from the `dist` branch, which the release workflow replaces on every
tag with a single commit: `plugin/` (manifest, `.mcp.json`, skill) plus
`bin/mcp-app-desktop` (universal macOS binary, assembled by the script since
Linux has no `lipo`) and `bin/mcp-app-desktop.exe` (Windows x64, which Windows on
arm64 runs too). `.mcp.json` starts `${CLAUDE_PLUGIN_ROOT}/bin/mcp-app-desktop`
directly: no Node, no npx, no registry, and no secret beyond the workflow's
own `GITHUB_TOKEN`.

```bash
bun scripts/release.ts version 0.2.0   # plugin.json (+ the dormant npm package.json)
git commit -am "release 0.2.0" && git tag v0.2.0
git push origin main v0.2.0            # release workflow: test, build, GitHub release, dist branch
```

The `dist` branch is generated: never commit to it. It moves only once the
build and the GitHub release succeeded, and re-running a release replaces it
again. To try a plugin build locally: `bun scripts/release.ts build 0.2.0 &&
bun scripts/release.ts plugin 0.2.0`, then `claude --plugin-dir dist/plugin`.

An npm distribution (`npm/mcp-app-desktop`: Node launcher plus one package per
platform) is ready but not published; `bun scripts/release.ts stage` and
`publish` (with `NPM_CONFIG_TOKEN`) would ship it.

CI runs on a single `ubuntu-latest` job (no macOS or Windows runners): the
binaries are pure Go and cross-compile in parallel in a few seconds,
`setup-go` caches modules and `GOCACHE` (test results included), and a newer
push cancels the running check. The end-to-end suites, which need a display
and Electron or a Tauri build, stay local, and so does the Rust crate (built
by the Tauri fixture).
