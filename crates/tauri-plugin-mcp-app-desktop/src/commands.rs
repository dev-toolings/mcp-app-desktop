//! Bridge commands. Each runs on its own thread and may block (Tauri getters
//! round-trip through the main thread); none of them may block the main thread.

use std::{
  sync::mpsc::{self, RecvTimeoutError},
  time::Duration,
};

use serde_json::{json, Map, Value};
use tauri::{AppHandle, Manager, Runtime, Webview};

pub const PROTOCOL: u64 = 1;
const EVAL_DEFAULT_MS: u64 = 10_000;
const EVAL_MAX_MS: u64 = 120_000;

pub fn dispatch<R: Runtime>(app: &AppHandle<R>, cmd: &str, params: &Value) -> Result<Value, String> {
  match cmd {
    "hello" => hello(app),
    "ping" => Ok(json!({})),
    "windows" => Ok(windows(app)),
    "eval" => eval(app, params),
    "screenshot" => crate::screenshot::capture(&webview(app, str_param(params, "label")?)?),
    other => Err(format!("unknown command {other:?} (known: hello, ping, windows, eval, screenshot)")),
  }
}

pub fn hello<R: Runtime>(app: &AppHandle<R>) -> Result<Value, String> {
  let info = app.package_info();
  Ok(json!({
    "protocol": PROTOCOL,
    "plugin_version": env!("CARGO_PKG_VERSION"),
    "tauri_version": tauri::VERSION,
    "app_name": info.name,
    "app_version": info.version.to_string(),
    "identifier": app.config().identifier,
    "pid": std::process::id(),
    "os": std::env::consts::OS,
  }))
}

/// Every webview with its window's state. A getter that fails only drops its
/// field: the list itself never fails.
fn windows<R: Runtime>(app: &AppHandle<R>) -> Value {
  let mut webviews = all_webviews(app);
  webviews.sort_by(|a, b| sort_key(a.label()).cmp(&sort_key(b.label())));
  Value::Array(webviews.iter().map(describe).collect())
}

fn sort_key(label: &str) -> (bool, &str) {
  (label != "main", label)
}

fn describe<R: Runtime>(webview: &Webview<R>) -> Value {
  let mut o = Map::new();
  o.insert("label".into(), json!(webview.label()));
  if let Ok(url) = webview.url() {
    o.insert("url".into(), json!(url.as_str()));
  }
  let window = webview.window();
  if let Ok(title) = window.title() {
    o.insert("title".into(), json!(title));
  }
  o.insert("visible".into(), json!(window.is_visible().unwrap_or(false)));
  o.insert("focused".into(), json!(window.is_focused().unwrap_or(false)));
  o.insert("minimized".into(), json!(window.is_minimized().unwrap_or(false)));
  let scale = window.scale_factor().ok().filter(|s| s.is_finite() && *s > 0.0);
  if let Some(scale) = scale {
    if let Ok(pos) = window.inner_position() {
      let pos = pos.to_logical::<f64>(scale);
      o.insert("x".into(), json!(pos.x));
      o.insert("y".into(), json!(pos.y));
    }
    if let Ok(size) = window.inner_size() {
      let size = size.to_logical::<f64>(scale);
      o.insert("width".into(), json!(size.width));
      o.insert("height".into(), json!(size.height));
    }
    o.insert("scale_factor".into(), json!(scale));
  }
  Value::Object(o)
}

fn eval<R: Runtime>(app: &AppHandle<R>, params: &Value) -> Result<Value, String> {
  let webview = webview(app, str_param(params, "label")?)?;
  let script = str_param(params, "script")?;
  let timeout_ms = match params.get("timeout_ms") {
    None | Some(Value::Null) => EVAL_DEFAULT_MS,
    Some(v) => v.as_u64().ok_or("\"timeout_ms\" must be a non-negative integer")?.clamp(1, EVAL_MAX_MS),
  };
  let (tx, rx) = mpsc::channel::<String>();
  webview
    .eval_with_callback(script, move |raw| {
      let _ = tx.send(raw);
    })
    .map_err(|e| format!("eval failed: {e}"))?;
  match rx.recv_timeout(Duration::from_millis(timeout_ms)) {
    Ok(raw) => Ok(json!({ "value": raw })),
    Err(RecvTimeoutError::Timeout) => Err(format!(
      "eval timed out after {timeout_ms} ms: the webview did not answer (page loading, or its main thread is busy)"
    )),
    // wry drops the callback when it queues the script for a page that has
    // not loaded yet: the script will still run, but its result is lost.
    Err(RecvTimeoutError::Disconnected) => {
      Err("eval got no result: the webview dropped the callback (page still loading?)".into())
    }
  }
}

/// Webviews of the app's webview windows. `Manager::webviews` would also list
/// child webviews, but it needs Tauri's `unstable` feature, which a plugin must
/// not force on its host app.
fn all_webviews<R: Runtime>(app: &AppHandle<R>) -> Vec<Webview<R>> {
  app.webview_windows().into_values().map(|w| w.as_ref().clone()).collect()
}

pub fn webview<R: Runtime>(app: &AppHandle<R>, label: &str) -> Result<Webview<R>, String> {
  if let Some(w) = app.get_webview_window(label) {
    return Ok(w.as_ref().clone());
  }
  let mut known: Vec<String> = app.webview_windows().into_keys().collect();
  known.sort_by(|a, b| sort_key(a).cmp(&sort_key(b)));
  Err(format!("unknown webview label {label:?} (known: {})", known.join(", ")))
}

fn str_param<'a>(params: &'a Value, name: &str) -> Result<&'a str, String> {
  match params.get(name) {
    Some(Value::String(s)) => Ok(s),
    Some(_) => Err(format!("\"{name}\" must be a string")),
    None => Err(format!("missing \"{name}\"")),
  }
}

#[cfg(test)]
mod tests {
  use super::*;

  #[test]
  fn main_sorts_first() {
    let mut labels = vec!["zeta", "second", "main", "alpha"];
    labels.sort_by(|a, b| sort_key(a).cmp(&sort_key(b)));
    assert_eq!(labels, vec!["main", "alpha", "second", "zeta"]);
  }

  #[test]
  fn string_params() {
    let p = json!({ "label": "main", "n": 1 });
    assert_eq!(str_param(&p, "label"), Ok("main"));
    assert!(str_param(&p, "n").is_err());
    assert!(str_param(&p, "missing").is_err());
  }
}
