//! A window macOS does not draw (here never shown; covered, minimised or hidden alike) is read
//! all the same: the bridge draws the frame that starts its accessibility tree itself.
//!
//! A real app, not a test harness: GPUI runs on the main thread, so `harness = false`. It needs a
//! macOS login session (a window server), as the app it stands for does.

#[cfg(target_os = "macos")]
fn main() {
    use std::io::{BufRead, BufReader, Write};
    use std::net::{TcpListener, TcpStream};
    use std::time::Duration;

    use gpui::{div, prelude::*, px, size, App, Bounds, Context, Role, Window, WindowBounds, WindowOptions};
    use serde_json::{json, Value};

    struct Root;
    impl Render for Root {
        fn render(&mut self, _: &mut Window, _: &mut Context<Self>) -> impl IntoElement {
            div().size_full().child(div().id("hello").role(Role::Button).aria_label("Hello button").size(px(80.)))
        }
    }

    let port = TcpListener::bind("127.0.0.1:0").unwrap().local_addr().unwrap().port();
    std::env::set_var("MCP_APP_DESKTOP_BRIDGE_PORT", port.to_string());
    std::env::set_var("MCP_APP_DESKTOP_BRIDGE_TOKEN", "unseen-window-test");

    // The client: hello, then a snapshot of the unseen window. Exits the process with the verdict.
    std::thread::spawn(move || {
        let verdict = (|| -> Result<Value, String> {
            let stream = (0..100)
                .find_map(|_| {
                    TcpStream::connect(("127.0.0.1", port)).ok().or_else(|| {
                        std::thread::sleep(Duration::from_millis(50));
                        None
                    })
                })
                .ok_or("the bridge never listened")?;
            stream.set_read_timeout(Some(Duration::from_secs(20))).unwrap();
            let mut writer = stream.try_clone().unwrap();
            let mut reader = BufReader::new(stream);
            let mut call = |request: Value| -> Result<Value, String> {
                writeln!(writer, "{request}").map_err(|e| e.to_string())?;
                let mut line = String::new();
                reader.read_line(&mut line).map_err(|e| e.to_string())?;
                let answer: Value = serde_json::from_str(&line).map_err(|e| e.to_string())?;
                if answer["ok"] != json!(true) {
                    return Err(format!("{request} failed: {answer}"));
                }
                Ok(answer["result"].clone())
            };
            call(json!({ "id": 1, "cmd": "hello", "token": "unseen-window-test" }))?;
            call(json!({ "id": 2, "cmd": "agent", "op": "snapshot", "args": { "level": "skeleton" } }))
        })();
        match verdict {
            Ok(snapshot) if snapshot.to_string().contains("Hello button") => {
                println!("unseen window: snapshot read its tree");
                std::process::exit(0);
            }
            Ok(snapshot) => {
                eprintln!("unseen window: the snapshot misses the button: {snapshot}");
                std::process::exit(1);
            }
            Err(e) => {
                eprintln!("unseen window: {e}");
                std::process::exit(1);
            }
        }
    });

    gpui_platform::application().run(|cx: &mut App| {
        gpui_mcp_app_desktop::init(cx);
        let bounds = Bounds::centered(None, size(px(200.), px(150.)), cx);
        cx.open_window(
            WindowOptions { window_bounds: Some(WindowBounds::Windowed(bounds)), show: false, focus: false, ..Default::default() },
            |window, cx| {
                assert!(!window.is_visible(), "the test needs a window macOS does not draw");
                cx.new(|_| Root)
            },
        )
        .unwrap();
    });
}

#[cfg(not(target_os = "macos"))]
fn main() {}
