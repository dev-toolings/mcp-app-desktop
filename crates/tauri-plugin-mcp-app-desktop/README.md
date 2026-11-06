# tauri-plugin-mcp-app-desktop

Tauri 2 plugin that lets the mcp-app-desktop MCP server drive an app during
development: list its webviews, evaluate JavaScript, take screenshots, and read
the console, uncaught errors and IPC calls recorded by an injected script.

## Use

Add the dependency, then register the plugin in debug builds only:

```rust
let mut builder = tauri::Builder::default();
#[cfg(debug_assertions)]
{ builder = builder.plugin(tauri_plugin_mcp_app_desktop::init()); }
```

## Behaviour

- Inert unless `MCP_APP_DESKTOP_BRIDGE_PORT` is set (read once, when `init()`
  runs): no script is injected and nothing listens. The MCP server sets it when
  it launches the app.
- Listens on `127.0.0.1:PORT` only, never on another interface. If the port is
  invalid or cannot be bound, the plugin prints one line on stderr and the app
  runs normally.
- When the server also sets `MCP_APP_DESKTOP_BRIDGE_TOKEN`, every connection
  must authenticate with `hello` and that token before anything else.
- Protocol v1: newline-delimited JSON over TCP, commands `hello`, `ping`,
  `windows`, `eval`, `screenshot` (macOS only; use the CDP backend on Windows).
