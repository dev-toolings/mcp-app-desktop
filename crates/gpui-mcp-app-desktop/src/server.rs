//! The loopback listener: newline-delimited JSON requests `{"id", "cmd", ...}`, answered
//! `{"id", "ok", "result" | "error"}`. Same protocol and limits as the Tauri plugin's. Each
//! request is handed to the main thread (GPUI runs there) and its connection thread waits for the
//! answer, so a slow request never blocks the others or the app.

use std::{
  io::{self, BufRead, BufReader, Write},
  net::{Ipv4Addr, Shutdown, SocketAddrV4, TcpListener, TcpStream},
  sync::{
    atomic::{AtomicUsize, Ordering},
    mpsc, Arc, Mutex,
  },
  thread,
  time::Duration,
};

use serde_json::{json, Value};

use crate::{Job, Settings, NAME};

const MAX_CONNECTIONS: usize = 8;
const MAX_LINE: usize = 32 * 1024 * 1024;
const MAX_IN_FLIGHT: usize = 64;

struct Shared {
  jobs: async_channel::Sender<Job>,
  token: Option<String>,
  connections: AtomicUsize,
}

/// Listens on `127.0.0.1:port`; false when it cannot (the app then runs without a bridge).
pub fn start(settings: Settings, jobs: async_channel::Sender<Job>) -> bool {
  let addr = SocketAddrV4::new(Ipv4Addr::LOCALHOST, settings.port);
  let listener = match TcpListener::bind(addr) {
    Ok(l) => l,
    Err(e) => {
      log::error!("[{NAME}] bridge disabled: cannot listen on {addr}: {e}");
      return false;
    }
  };
  let shared = Arc::new(Shared { jobs, token: settings.token, connections: AtomicUsize::new(0) });
  let spawned = thread::Builder::new().name("mcp-app-desktop-accept".into()).spawn(move || accept_loop(listener, shared));
  match spawned {
    Ok(_) => {
      // Printed on stderr too: mcp-app-desktop reads the process output to know the bridge is up.
      eprintln!("[{NAME}] bridge listening on {addr}");
      true
    }
    Err(e) => {
      log::error!("[{NAME}] bridge disabled: cannot start its thread: {e}");
      false
    }
  }
}

fn accept_loop(listener: TcpListener, shared: Arc<Shared>) {
  for stream in listener.incoming() {
    let stream = match stream {
      Ok(s) => s,
      Err(_) => {
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

struct Slot<'a>(&'a AtomicUsize);

impl Drop for Slot<'_> {
  fn drop(&mut self) {
    self.0.fetch_sub(1, Ordering::SeqCst);
  }
}

type Writer = Arc<Mutex<TcpStream>>;

/// Runs one request on the main thread and waits for its answer.
fn call_main(shared: &Shared, cmd: &str, params: Value) -> Result<Value, String> {
  let (reply, answer) = mpsc::sync_channel(1);
  // The caller's own budget when it gives one (it stops waiting then): past it, the request
  // must not act.
  let budget = params.get("timeout_ms").and_then(Value::as_u64).map(Duration::from_millis);
  let timeout = budget.map_or(crate::reply_timeout(cmd), |b| b.min(crate::reply_timeout(cmd)));
  let deadline = std::time::Instant::now() + timeout;
  match shared.jobs.try_send(Job { cmd: cmd.to_owned(), params, reply, deadline }) {
    Ok(()) => {}
    Err(async_channel::TrySendError::Full(_)) => {
      return Err(format!("{cmd}: the app has {} requests waiting for its main thread; try again", crate::MAX_QUEUED))
    }
    Err(async_channel::TrySendError::Closed(_)) => return Err("the app is shutting down".to_string()),
  }
  match answer.recv_timeout(timeout) {
    Ok(result) => result,
    Err(mpsc::RecvTimeoutError::Timeout) => Err(format!(
      "{cmd}: the app's main thread did not answer within {} s (busy or blocked?)",
      timeout.as_secs()
    )),
    Err(mpsc::RecvTimeoutError::Disconnected) => Err(format!("{cmd}: the app dropped the request (window closed?)")),
  }
}

/// How long a connection may stay silent before its `hello`: a client that never authenticates
/// must not keep one of the few connection slots.
const HELLO_TIMEOUT: Duration = Duration::from_secs(5);
/// A client that stops reading must not block the thread answering it.
const WRITE_TIMEOUT: Duration = Duration::from_secs(10);

fn serve_connection(stream: TcpStream, shared: &Arc<Shared>) {
  let _ = stream.set_nodelay(true);
  // A client must authenticate within HELLO_TIMEOUT in all: a watchdog closes the connection
  // otherwise, even if it keeps sending a line byte by byte (a read timeout alone would not).
  let authenticated_flag = Arc::new(std::sync::atomic::AtomicBool::new(shared.token.is_none()));
  if shared.token.is_some() {
    let flag = authenticated_flag.clone();
    if let Ok(watched) = stream.try_clone() {
      let _ = thread::Builder::new().name("mcp-app-desktop-hello".into()).spawn(move || {
        thread::sleep(HELLO_TIMEOUT);
        if !flag.load(Ordering::SeqCst) {
          let _ = watched.shutdown(Shutdown::Both);
        }
      });
    }
  }
  let _ = stream.set_write_timeout(Some(WRITE_TIMEOUT));
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
      match check_hello(&request, shared.token.as_deref()) {
        Ok(()) => {
          authenticated = true;
          authenticated_flag.store(true, Ordering::SeqCst);
          send(&writer, &respond(request.id, Ok(crate::hello())));
          continue;
        }
        Err(msg) => {
          send(&writer, &error_response(request.id, msg));
          close(&writer);
          return;
        }
      }
    }
    if request.cmd == "hello" {
      send(&writer, &respond(request.id, Ok(crate::hello())));
      continue;
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
      let result = call_main(&req_shared, &request.cmd, request.params);
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

/// Writes one answer. A write that fails or times out may have sent part of the line: the
/// connection is then closed, never left to carry the next answer after a fragment.
fn send(writer: &Writer, value: &Value) {
  // A poisoned lock only means another writer panicked mid-write; the stream
  // itself is still usable.
  let mut stream = writer.lock().unwrap_or_else(|p| p.into_inner());
  if write_line(&mut stream, value).is_err() {
    let _ = stream.shutdown(Shutdown::Both);
  }
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
