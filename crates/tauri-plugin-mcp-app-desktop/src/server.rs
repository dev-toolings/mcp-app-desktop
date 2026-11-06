//! Loopback TCP server: newline-delimited JSON, one thread per connection and
//! one thread per request, responses written under a per-connection mutex.

use std::{
  io::{self, BufRead, BufReader, Write},
  net::{Ipv4Addr, Shutdown, SocketAddrV4, TcpListener, TcpStream},
  panic::{catch_unwind, AssertUnwindSafe},
  sync::{
    atomic::{AtomicUsize, Ordering},
    Arc, Mutex,
  },
  thread,
  time::Duration,
};

use serde_json::{json, Value};
use tauri::{AppHandle, Runtime};

use crate::{commands, Settings, PLUGIN_NAME};

const MAX_CONNECTIONS: usize = 8;
const MAX_LINE: usize = 32 * 1024 * 1024;
/// Bounds the threads one connection can pin (eval and screenshot wait up to
/// two minutes each).
const MAX_IN_FLIGHT: usize = 64;

struct Shared<R: Runtime> {
  app: AppHandle<R>,
  token: Option<String>,
  connections: AtomicUsize,
}

/// Binds 127.0.0.1:port and serves in background threads. Failures are
/// reported on one stderr line and leave the app untouched.
pub fn start<R: Runtime>(app: AppHandle<R>, settings: Settings) {
  let addr = SocketAddrV4::new(Ipv4Addr::LOCALHOST, settings.port);
  let listener = match TcpListener::bind(addr) {
    Ok(l) => l,
    Err(e) => {
      eprintln!("[{PLUGIN_NAME}] bridge disabled: cannot listen on {addr}: {e}");
      return;
    }
  };
  let shared = Arc::new(Shared { app, token: settings.token, connections: AtomicUsize::new(0) });
  let spawned = thread::Builder::new()
    .name("mcp-app-desktop-accept".into())
    .spawn(move || accept_loop(listener, shared));
  match spawned {
    Ok(_) => eprintln!("[{PLUGIN_NAME}] bridge listening on {addr}"),
    Err(e) => eprintln!("[{PLUGIN_NAME}] bridge disabled: cannot start its thread: {e}"),
  }
}

fn accept_loop<R: Runtime>(listener: TcpListener, shared: Arc<Shared<R>>) {
  for stream in listener.incoming() {
    let stream = match stream {
      Ok(s) => s,
      Err(_) => {
        // EMFILE and friends: do not spin.
        thread::sleep(Duration::from_millis(50));
        continue;
      }
    };
    if shared.connections.fetch_add(1, Ordering::SeqCst) >= MAX_CONNECTIONS {
      shared.connections.fetch_sub(1, Ordering::SeqCst);
      let mut s = stream;
      let _ = write_line(&mut s, &error_response(0, format!("too many connections (max {MAX_CONNECTIONS})")));
      let _ = s.shutdown(Shutdown::Both);
      continue;
    }
    let conn_shared = shared.clone();
    let spawned = thread::Builder::new().name("mcp-app-desktop-conn".into()).spawn(move || {
      let _slot = Slot(&conn_shared.connections);
      serve_connection(stream, &conn_shared);
    });
    if spawned.is_err() {
      shared.connections.fetch_sub(1, Ordering::SeqCst);
    }
  }
}

/// Releases a connection slot when the connection thread ends.
struct Slot<'a>(&'a AtomicUsize);

impl Drop for Slot<'_> {
  fn drop(&mut self) {
    self.0.fetch_sub(1, Ordering::SeqCst);
  }
}

type Writer = Arc<Mutex<TcpStream>>;

fn serve_connection<R: Runtime>(stream: TcpStream, shared: &Arc<Shared<R>>) {
  let _ = stream.set_nodelay(true);
  let writer: Writer = match stream.try_clone() {
    Ok(w) => Arc::new(Mutex::new(w)),
    Err(_) => return,
  };
  let mut reader = BufReader::new(stream);
  let mut authenticated = shared.token.is_none();
  let in_flight = Arc::new(AtomicUsize::new(0));

  loop {
    let line = match read_line_bounded(&mut reader, MAX_LINE) {
      Ok(Line::Data(l)) => l,
      Ok(Line::Eof) | Err(_) => return,
      Ok(Line::TooLong) => {
        send(&writer, &error_response(0, format!("request line exceeds {} MiB", MAX_LINE / (1024 * 1024))));
        close(&writer);
        return;
      }
    };
    if line.iter().all(u8::is_ascii_whitespace) {
      continue;
    }

    let request = match parse_request(&line) {
      Ok(r) => r,
      Err((id, msg)) => {
        send(&writer, &error_response(id, msg));
        if !authenticated {
          close(&writer);
          return;
        }
        continue;
      }
    };

    if !authenticated {
      // Handled inline so nothing else runs before the token is checked.
      match check_hello(&request, shared.token.as_deref()) {
        Ok(()) => {
          authenticated = true;
          send(&writer, &respond(request.id, commands::hello(&shared.app)));
          continue;
        }
        Err(msg) => {
          send(&writer, &error_response(request.id, msg));
          close(&writer);
          return;
        }
      }
    }

    if in_flight.load(Ordering::SeqCst) >= MAX_IN_FLIGHT {
      send(&writer, &error_response(request.id, format!("too many requests in flight on this connection (max {MAX_IN_FLIGHT})")));
      continue;
    }
    in_flight.fetch_add(1, Ordering::SeqCst);
    let (req_shared, req_writer, req_in_flight) = (shared.clone(), writer.clone(), in_flight.clone());
    let id = request.id;
    let spawned = thread::Builder::new().name("mcp-app-desktop-req".into()).spawn(move || {
      let _slot = Slot(&req_in_flight);
      let result = catch_unwind(AssertUnwindSafe(|| commands::dispatch(&req_shared.app, &request.cmd, &request.params)))
        .unwrap_or_else(|panic| Err(format!("internal error in the bridge: {}", panic_message(&panic))));
      send(&req_writer, &respond(request.id, result));
    });
    if let Err(e) = spawned {
      in_flight.fetch_sub(1, Ordering::SeqCst);
      send(&writer, &error_response(id, format!("cannot start a thread for this request: {e}")));
    }
  }
}

struct Request {
  id: u64,
  cmd: String,
  params: Value,
}

/// Parses one request line; the error carries the id to answer with (0 when
/// the line has no usable id).
fn parse_request(line: &[u8]) -> Result<Request, (u64, String)> {
  let value: Value = serde_json::from_slice(line).map_err(|e| (0, format!("malformed request: {e}")))?;
  if !value.is_object() {
    return Err((0, "malformed request: expected a JSON object".into()));
  }
  let id = match value.get("id") {
    Some(v) => v.as_u64().ok_or((0, "malformed request: \"id\" must be a non-negative integer".to_string()))?,
    None => return Err((0, "malformed request: missing \"id\"".into())),
  };
  let cmd = match value.get("cmd").and_then(Value::as_str) {
    Some(c) => c.to_string(),
    None => return Err((id, "malformed request: missing string \"cmd\"".into())),
  };
  Ok(Request { id, cmd, params: value })
}

fn check_hello(request: &Request, token: Option<&str>) -> Result<(), String> {
  if request.cmd != "hello" {
    return Err(format!("authentication required: send hello with the token before {:?}", request.cmd));
  }
  let Some(expected) = token else { return Ok(()) };
  match request.params.get("token").and_then(Value::as_str) {
    Some(given) if constant_time_eq(given.as_bytes(), expected.as_bytes()) => Ok(()),
    Some(_) => Err("authentication failed: wrong token".into()),
    None => Err("authentication required: hello must carry the bridge token".into()),
  }
}

fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
  if a.len() != b.len() {
    return false;
  }
  a.iter().zip(b).fold(0u8, |acc, (x, y)| acc | (x ^ y)) == 0
}

fn respond(id: u64, result: Result<Value, String>) -> Value {
  match result {
    Ok(v) => json!({ "id": id, "ok": true, "result": v }),
    Err(e) => error_response(id, e),
  }
}

fn error_response(id: u64, error: impl Into<String>) -> Value {
  json!({ "id": id, "ok": false, "error": error.into() })
}

fn send(writer: &Writer, value: &Value) {
  // A poisoned lock only means another writer panicked mid-write; the stream
  // itself is still usable.
  let mut stream = writer.lock().unwrap_or_else(|p| p.into_inner());
  let _ = write_line(&mut stream, value);
}

fn write_line(stream: &mut TcpStream, value: &Value) -> io::Result<()> {
  let mut bytes = serde_json::to_vec(value).map_err(io::Error::other)?;
  bytes.push(b'\n');
  stream.write_all(&bytes)?;
  stream.flush()
}

fn close(writer: &Writer) {
  let stream = writer.lock().unwrap_or_else(|p| p.into_inner());
  let _ = stream.shutdown(Shutdown::Both);
}

fn panic_message(panic: &Box<dyn std::any::Any + Send>) -> String {
  if let Some(s) = panic.downcast_ref::<&str>() {
    (*s).to_string()
  } else if let Some(s) = panic.downcast_ref::<String>() {
    s.clone()
  } else {
    "panic".into()
  }
}

#[derive(Debug, PartialEq)]
enum Line {
  Data(Vec<u8>),
  Eof,
  TooLong,
}

/// Reads up to the next '\n' without ever buffering more than `max` bytes.
/// A final unterminated line is returned as data; '\r' before '\n' is dropped.
fn read_line_bounded<B: BufRead>(reader: &mut B, max: usize) -> io::Result<Line> {
  let mut line = Vec::new();
  loop {
    let available = match reader.fill_buf() {
      Ok(b) => b,
      Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
      Err(e) => return Err(e),
    };
    if available.is_empty() {
      return Ok(if line.is_empty() { Line::Eof } else { Line::Data(line) });
    }
    if let Some(i) = available.iter().position(|&b| b == b'\n') {
      if line.len() + i > max {
        return Ok(Line::TooLong);
      }
      line.extend_from_slice(&available[..i]);
      reader.consume(i + 1);
      if line.last() == Some(&b'\r') {
        line.pop();
      }
      return Ok(Line::Data(line));
    }
    let n = available.len();
    if line.len() + n > max {
      return Ok(Line::TooLong);
    }
    line.extend_from_slice(available);
    reader.consume(n);
  }
}

#[cfg(test)]
mod tests {
  use super::*;

  fn lines(input: &[u8], max: usize) -> Vec<Line> {
    // A tiny buffer forces lines to span several fill_buf calls.
    let mut r = BufReader::with_capacity(3, input);
    let mut out = Vec::new();
    loop {
      let l = read_line_bounded(&mut r, max).expect("read");
      let stop = matches!(l, Line::Eof | Line::TooLong);
      out.push(l);
      if stop {
        return out;
      }
    }
  }

  #[test]
  fn reads_lines_across_buffers() {
    assert_eq!(
      lines(b"{\"a\":1}\r\nxy\n\nlast", 100),
      vec![Line::Data(b"{\"a\":1}".to_vec()), Line::Data(b"xy".to_vec()), Line::Data(vec![]), Line::Data(b"last".to_vec()), Line::Eof]
    );
  }

  #[test]
  fn rejects_long_lines() {
    assert_eq!(lines(b"12345\n", 5), vec![Line::Data(b"12345".to_vec()), Line::Eof]);
    assert_eq!(lines(b"123456\n", 5), vec![Line::TooLong]);
    assert_eq!(lines(b"123456789", 5), vec![Line::TooLong]);
  }

  #[test]
  fn parses_requests() {
    let r = parse_request(br#"{"id":7,"cmd":"eval","label":"main"}"#).expect("valid");
    assert_eq!((r.id, r.cmd.as_str()), (7, "eval"));
    assert_eq!(r.params["label"], "main");
    assert_eq!(parse_request(b"{nope").err().map(|e| e.0), Some(0));
    assert_eq!(parse_request(b"[1]").err().map(|e| e.0), Some(0));
    assert_eq!(parse_request(br#"{"id":-1,"cmd":"ping"}"#).err().map(|e| e.0), Some(0));
    assert_eq!(parse_request(br#"{"id":3}"#).err().map(|e| e.0), Some(3));
  }

  #[test]
  fn hello_gate() {
    let req = |s: &[u8]| parse_request(s).expect("valid");
    assert!(check_hello(&req(br#"{"id":1,"cmd":"hello"}"#), None).is_ok());
    assert!(check_hello(&req(br#"{"id":1,"cmd":"ping"}"#), None).is_err());
    assert!(check_hello(&req(br#"{"id":1,"cmd":"hello","token":"s"}"#), Some("s")).is_ok());
    assert!(check_hello(&req(br#"{"id":1,"cmd":"hello","token":"x"}"#), Some("s")).is_err());
    assert!(check_hello(&req(br#"{"id":1,"cmd":"hello"}"#), Some("s")).is_err());
    assert!(check_hello(&req(br#"{"id":1,"cmd":"ping","token":"s"}"#), Some("s")).is_err());
  }
}
