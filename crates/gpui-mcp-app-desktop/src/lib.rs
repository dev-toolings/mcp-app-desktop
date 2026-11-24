//! Development bridge between the mcp-app-desktop MCP server and a GPUI app.
//!
//! The bridge is inert unless `MCP_APP_DESKTOP_BRIDGE_PORT` is set when [`init`] runs. When it
//! is, it serves the same newline-delimited JSON protocol as the Tauri plugin on
//! `127.0.0.1:PORT` only (`hello` first, with `MCP_APP_DESKTOP_BRIDGE_TOKEN` when set), and
//! answers `hello` with `"framework": "gpui"`.
//!
//! GPUI has no DOM: the bridge reads the window through its accessibility tree (AccessKit, as
//! macOS exposes it), and acts through real GPUI input events (`Window::dispatch_event`,
//! `Window::dispatch_keystroke`). Only elements with an `.id(...)` and a `.role(...)` (or a
//! gpui-component control, which sets both) appear in a snapshot; give them an `.aria_label`
//! when their text is not their name.
//!
//! ```rust,ignore
//! gpui_kit::application().run(|cx| {
//!     #[cfg(debug_assertions)]
//!     gpui_mcp_app_desktop::init(cx);
//!     // Optional: commands the `invoke` tool can call (the GPUI stand-in for `eval`).
//!     gpui_mcp_app_desktop::register(cx, "state", |_args, _window, _cx| Ok(serde_json::json!({})));
//! });
//! ```

mod agent;
mod base64;
mod keys;
mod server;
mod tree;

use std::collections::HashMap;
use std::rc::Rc;
use std::sync::mpsc;
use std::time::{Duration, Instant};

use gpui::{AnyWindowHandle, App, AsyncApp, Global, Window};
use serde_json::{json, Value};

pub(crate) const NAME: &str = "gpui-mcp-app-desktop";
const ENV_PORT: &str = "MCP_APP_DESKTOP_BRIDGE_PORT";
const ENV_TOKEN: &str = "MCP_APP_DESKTOP_BRIDGE_TOKEN";
pub(crate) const PROTOCOL: u64 = 1;

/// A command the app exposes to the `invoke` tool.
pub type Command = Rc<dyn Fn(Value, &mut Window, &mut App) -> Result<Value, String>>;

#[derive(Default)]
struct Commands(HashMap<String, Command>);

impl Global for Commands {}

/// Registers `name` for the `invoke` tool: it runs on the main thread with the window the call
/// names (the first window when it names none) and answers JSON.
pub fn register(
    cx: &mut App,
    name: &str,
    command: impl Fn(Value, &mut Window, &mut App) -> Result<Value, String> + 'static,
) {
    cx.default_global::<Commands>().0.insert(name.to_owned(), Rc::new(command));
}

/// Bridge settings, read once from the environment by [`init`].
#[derive(Clone)]
pub(crate) struct Settings {
    pub port: u16,
    pub token: Option<String>,
}

fn settings_from_env() -> Option<Settings> {
    let raw = std::env::var_os(ENV_PORT)?;
    let port = match raw.to_str().map(str::trim).and_then(|s| s.parse::<u16>().ok()) {
        Some(p) if p != 0 => p,
        _ => {
            log::error!("[{NAME}] bridge disabled: {ENV_PORT}={raw:?} is not a valid port (1-65535)");
            return None;
        }
    };
    let token = std::env::var(ENV_TOKEN).ok().filter(|t| !t.is_empty());
    Some(Settings { port, token })
}

/// One request handed from a connection thread to the main thread.
pub(crate) struct Job {
    pub cmd: String,
    pub params: Value,
    pub reply: mpsc::SyncSender<Result<Value, String>>,
    /// When the caller stops waiting. A request still queued then, or still waiting for a
    /// frame, never acts: the caller already reported a timeout and may send it again.
    pub deadline: Instant,
}

/// Requests queued for the main thread at most; beyond, the bridge answers « busy ».
pub(crate) const MAX_QUEUED: usize = 64;

pub(crate) fn expired(cmd: &str, deadline: Instant) -> Result<(), String> {
    if Instant::now() >= deadline {
        Err(format!("{cmd}: expired before the app's main thread could run it; nothing was done"))
    } else {
        Ok(())
    }
}

/// Starts the bridge when `MCP_APP_DESKTOP_BRIDGE_PORT` holds a valid port, in debug builds only;
/// does nothing otherwise. A bridge that cannot start logs why and never stops the app.
pub fn init(cx: &mut App) {
    let Some(settings) = settings_from_env() else { return };
    // The bridge drives the whole app: a release build never opens it, whatever its features.
    if !cfg!(debug_assertions) {
        log::error!("[{NAME}] bridge refused: {ENV_PORT} is set but this is a release build");
        return;
    }
    let (tx, rx) = async_channel::bounded::<Job>(MAX_QUEUED);
    if !server::start(settings, tx) {
        return;
    }
    cx.spawn(async move |cx: &mut AsyncApp| {
        while let Ok(job) = rx.recv().await {
            let cx2 = cx.clone();
            cx.foreground_executor()
                .spawn(async move {
                    let Job { cmd, params, reply, deadline } = job;
                    let result = match expired(&cmd, deadline) {
                        Ok(()) => dispatch(&cmd, params, deadline, cx2).await,
                        Err(e) => Err(e),
                    };
                    let _ = reply.send(result);
                })
                .detach();
        }
    })
    .detach();
}

async fn dispatch(cmd: &str, params: Value, deadline: Instant, cx: AsyncApp) -> Result<Value, String> {
    match cmd {
        "hello" => Ok(hello()),
        "ping" => Ok(json!({})),
        "windows" => Ok(cx.update(windows)),
        "agent" => {
            let handle = window_of(&params, &cx)?;
            let op = params.get("op").and_then(Value::as_str).unwrap_or_default().to_owned();
            let args = params.get("args").cloned().unwrap_or(Value::Null);
            agent::run(handle, &op, args, deadline, cx).await
        }
        "screenshot" => {
            let handle = window_of(&params, &cx)?;
            // The scene of the last drawn frame: wait for one drawn after the events already sent.
            agent::fresh_frame(handle, &cx).await?;
            expired(cmd, deadline)?;
            screenshot(handle, &cx)
        }
        "invoke" => {
            let handle = window_of(&params, &cx)?;
            let name = params
                .get("command")
                .and_then(Value::as_str)
                .ok_or("invoke needs a string \"command\"")?
                .to_owned();
            let args = params.get("args").cloned().unwrap_or(Value::Null);
            invoke(handle, &name, args, &cx)
        }
        "commands" => Ok(cx.update(|cx| {
            let mut names: Vec<String> =
                cx.try_global::<Commands>().map(|c| c.0.keys().cloned().collect()).unwrap_or_default();
            names.sort();
            json!(names)
        })),
        other => Err(format!(
            "unknown command {other:?} (known: hello, ping, windows, agent, screenshot, invoke, commands)"
        )),
    }
}

pub(crate) fn hello() -> Value {
    let exe = std::env::current_exe().ok();
    let app_name = exe
        .as_ref()
        .and_then(|p| p.file_stem())
        .map(|s| s.to_string_lossy().into_owned())
        .unwrap_or_default();
    json!({
        "protocol": PROTOCOL,
        "click_modifiers": true,
        "framework": "gpui",
        "plugin_version": env!("CARGO_PKG_VERSION"),
        "app_name": app_name,
        "pid": std::process::id(),
        "os": std::env::consts::OS,
        "capture": cfg!(feature = "capture"),
    })
}

/// A window's label: its GPUI id, stable while it is open.
pub(crate) fn label_of(handle: AnyWindowHandle) -> String {
    handle.window_id().as_u64().to_string()
}

fn windows(cx: &mut App) -> Value {
    let active = cx.active_window();
    let mut out = Vec::new();
    for handle in cx.windows() {
        let described = handle.update(cx, |_, window, _| {
            let size = window.viewport_size();
            json!({
                "label": label_of(handle),
                "url": format!("gpui://window/{}", label_of(handle)),
                "title": window.window_title(),
                "visible": true,
                "focused": active == Some(handle),
                "width": size.width.as_f32(),
                "height": size.height.as_f32(),
                "scale_factor": window.scale_factor(),
            })
        });
        if let Ok(v) = described {
            out.push(v);
        }
    }
    Value::Array(out)
}

/// The window a request names by `label`, or the first one when it names none.
fn window_of(params: &Value, cx: &AsyncApp) -> Result<AnyWindowHandle, String> {
    let wanted = params.get("label").and_then(Value::as_str).filter(|l| !l.is_empty()).map(str::to_owned);
    cx.update(|cx| {
        let handles = cx.windows();
        match &wanted {
            None => handles.first().copied().ok_or_else(|| "the app has no window open".to_owned()),
            Some(label) => handles.iter().copied().find(|h| &label_of(*h) == label).ok_or_else(|| {
                let open: Vec<String> = handles.iter().map(|h| label_of(*h)).collect();
                format!("window {label:?} does not exist (open: {})", open.join(", "))
            }),
        }
    })
}

fn invoke(handle: AnyWindowHandle, name: &str, args: Value, cx: &AsyncApp) -> Result<Value, String> {
    cx.update(|cx| {
        let command = cx.try_global::<Commands>().and_then(|c| c.0.get(name).cloned());
        let Some(command) = command else {
            let mut known: Vec<String> =
                cx.try_global::<Commands>().map(|c| c.0.keys().cloned().collect()).unwrap_or_default();
            known.sort();
            return Err(format!("no command {name:?} registered by the app (known: {})", known.join(", ")));
        };
        handle
            .update(cx, |_, window, cx| command(args, window, cx))
            .map_err(|e| format!("window closed: {e}"))?
    })
}

#[cfg(feature = "capture")]
fn screenshot(handle: AnyWindowHandle, cx: &AsyncApp) -> Result<Value, String> {
    let image = cx
        .update(|cx| handle.update(cx, |_, window, _| window.render_to_image()))
        .map_err(|e| format!("window closed: {e}"))?
        .map_err(|e| format!("screenshot failed: {e:#}"))?;
    let (width, height) = image.dimensions();
    let mut png = std::io::Cursor::new(Vec::new());
    image
        .write_to(&mut png, image::ImageFormat::Png)
        .map_err(|e| format!("screenshot: PNG encoding failed: {e}"))?;
    Ok(json!({ "png_base64": base64::encode(png.get_ref()), "width": width, "height": height }))
}

#[cfg(not(feature = "capture"))]
fn screenshot(_handle: AnyWindowHandle, _cx: &AsyncApp) -> Result<Value, String> {
    Err(format!("screenshot needs the bridge's `capture` feature ({NAME} built without it)"))
}

/// How long a connection thread waits for the main thread to answer.
pub(crate) fn reply_timeout(cmd: &str) -> Duration {
    match cmd {
        "screenshot" => Duration::from_secs(20),
        _ => Duration::from_secs(30),
    }
}
