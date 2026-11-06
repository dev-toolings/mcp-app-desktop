// Prevents an extra console window on Windows in release.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

#[tauri::command]
fn greet(name: String) -> String {
  format!("Hello, {name}!")
}

#[tauri::command]
fn fail() -> Result<(), String> {
  Err("boom".into())
}

fn main() {
  #[allow(unused_mut)]
  let mut builder = tauri::Builder::default();
  #[cfg(debug_assertions)]
  {
    builder = builder.plugin(tauri_plugin_mcp_app_desktop::init());
  }
  builder
    .invoke_handler(tauri::generate_handler![greet, fail])
    .run(tauri::generate_context!())
    .expect("error while running tauri-fixture");
}
