//! Native webview screenshot, as PNG.

use serde_json::Value;
use tauri::{Runtime, Webview};

#[cfg(target_os = "macos")]
pub fn capture<R: Runtime>(webview: &Webview<R>) -> Result<Value, String> {
  use std::{
    sync::mpsc::{self, RecvTimeoutError},
    time::Duration,
  };

  const TIMEOUT: Duration = Duration::from_secs(15);

  let (tx, rx) = mpsc::channel::<Result<macos::Png, String>>();
  // with_webview runs the closure on the main thread; the snapshot completes
  // later on the main thread too, and both only send on the channel.
  webview
    .with_webview(move |pw| macos::snapshot(pw.inner(), tx))
    .map_err(|e| format!("screenshot failed: {e}"))?;
  let png = match rx.recv_timeout(TIMEOUT) {
    Ok(r) => r?,
    Err(RecvTimeoutError::Timeout) => {
      return Err(format!(
        "screenshot timed out after {} s: the webview did not answer (main thread busy?)",
        TIMEOUT.as_secs()
      ))
    }
    Err(RecvTimeoutError::Disconnected) => {
      return Err("screenshot failed: the webview dropped the request".into())
    }
  };
  Ok(serde_json::json!({
    "png_base64": crate::base64::encode(&png.data),
    "width": png.width,
    "height": png.height,
  }))
}

#[cfg(not(target_os = "macos"))]
pub fn capture<R: Runtime>(_webview: &Webview<R>) -> Result<Value, String> {
  Err(format!(
    "screenshot is not supported by the bridge on {}: use the CDP backend on Windows",
    std::env::consts::OS
  ))
}

#[cfg(target_os = "macos")]
mod macos {
  use std::{ffi::c_void, sync::mpsc::Sender};

  use block2::RcBlock;
  use objc2_app_kit::{NSBitmapImageFileType, NSBitmapImageRep, NSImage};
  use objc2_foundation::{NSDictionary, NSError};
  use objc2_web_kit::WKWebView;

  pub struct Png {
    pub data: Vec<u8>,
    pub width: i64,
    pub height: i64,
  }

  type Reply = Sender<Result<Png, String>>;

  /// Must run on the main thread: `inner` is the WKWebView of the webview.
  pub fn snapshot(inner: *mut c_void, tx: Reply) {
    // SAFETY: on macOS, Tauri's PlatformWebview::inner() is the WKWebView,
    // alive for the duration of the with_webview closure we run in.
    let Some(wk) = (unsafe { (inner as *const WKWebView).as_ref() }) else {
      let _ = tx.send(Err("screenshot failed: the webview has no native view".into()));
      return;
    };
    let handler = RcBlock::new(move |image: *mut NSImage, error: *mut NSError| {
      // SAFETY: WebKit passes either a valid NSImage or null, and a valid
      // NSError or null, for the duration of the call.
      let result = unsafe { encode(image.as_ref(), error.as_ref()) };
      let _ = tx.send(result);
    });
    // SAFETY: a nil configuration snapshots the visible bounds; the block is
    // retained by WebKit until it is called.
    unsafe { wk.takeSnapshotWithConfiguration_completionHandler(None, &handler) };
  }

  fn encode(image: Option<&NSImage>, error: Option<&NSError>) -> Result<Png, String> {
    let Some(image) = image else {
      let reason = error.map(|e| e.localizedDescription().to_string()).unwrap_or_else(|| "no image".into());
      return Err(format!("screenshot failed: {reason}"));
    };
    let tiff = image.TIFFRepresentation().ok_or("screenshot failed: the snapshot has no bitmap")?;
    let rep = NSBitmapImageRep::imageRepWithData(&tiff).ok_or("screenshot failed: cannot decode the snapshot")?;
    let properties = NSDictionary::new();
    // SAFETY: an empty dictionary is a valid property set.
    let png = unsafe { rep.representationUsingType_properties(NSBitmapImageFileType::PNG, &properties) }
      .ok_or("screenshot failed: PNG encoding failed")?;
    Ok(Png { data: png.to_vec(), width: rep.pixelsWide() as i64, height: rep.pixelsHigh() as i64 })
  }
}
