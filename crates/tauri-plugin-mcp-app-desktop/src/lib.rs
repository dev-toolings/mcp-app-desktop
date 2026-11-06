//! Development bridge between the mcp-app-desktop MCP server and a Tauri 2 app.
//!
//! The plugin is inert unless `MCP_APP_DESKTOP_BRIDGE_PORT` is set when
//! [`init`] runs. When it is, the plugin injects `init.js` (console, errors and
//! IPC recording) into every webview and serves a newline-delimited JSON
//! protocol on `127.0.0.1:PORT` only. When `MCP_APP_DESKTOP_BRIDGE_TOKEN` is
//! non-empty, each connection must authenticate with `hello` first.
//!
//! Register it in debug builds only:
//!
//! ```rust,ignore
//! let mut builder = tauri::Builder::default();
//! #[cfg(debug_assertions)]
//! { builder = builder.plugin(tauri_plugin_mcp_app_desktop::init()); }
//! ```

use tauri::{
  plugin::{Builder, TauriPlugin},
  Runtime,
};

mod base64;
mod commands;
mod screenshot;
mod server;

const PLUGIN_NAME: &str = "mcp-app-desktop";
const ENV_PORT: &str = "MCP_APP_DESKTOP_BRIDGE_PORT";
const ENV_TOKEN: &str = "MCP_APP_DESKTOP_BRIDGE_TOKEN";
const INIT_SCRIPT: &str = include_str!("init.js");

/// Bridge settings, read once from the environment by [`init`].
#[derive(Clone)]
struct Settings {
  port: u16,
  token: Option<String>,
}

fn settings_from_env() -> Option<Settings> {
  let raw = std::env::var_os(ENV_PORT)?;
  let port = match raw.to_str().map(str::trim).and_then(|s| s.parse::<u16>().ok()) {
    Some(p) if p != 0 => p,
    _ => {
      eprintln!("[{PLUGIN_NAME}] bridge disabled: {ENV_PORT}={raw:?} is not a valid port (1-65535)");
      return None;
    }
  };
  let token = std::env::var(ENV_TOKEN).ok().filter(|t| !t.is_empty());
  Some(Settings { port, token })
}

/// Creates the plugin. It does nothing (no script, no listener) unless
/// `MCP_APP_DESKTOP_BRIDGE_PORT` holds a valid port.
pub fn init<R: Runtime>() -> TauriPlugin<R> {
  let builder = Builder::new(PLUGIN_NAME);
  let Some(settings) = settings_from_env() else {
    return builder.build();
  };
  builder
    .js_init_script(INIT_SCRIPT)
    .setup(move |app, _api| {
      // A bridge that cannot start must never prevent the app from running.
      server::start(app.clone(), settings);
      Ok(())
    })
    .build()
}
