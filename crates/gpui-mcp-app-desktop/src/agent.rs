//! The page-agent operations of the Tauri bridge (`agent.js`), implemented natively for GPUI:
//! the same op names, arguments and answers, so mcp-app-desktop drives both with one code path.
//! Agent failures answer `{"error", "code"}` (codes `notfound`, `hidden`, `disabled`, `stale`,
//! `ambiguous`, `invalid`, `error`), which the server retries or reports like the Tauri ones.

use std::collections::hash_map::DefaultHasher;
use std::collections::HashMap;
use std::hash::{Hash, Hasher};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use gpui::{
    point, px, AnyWindowHandle, App, AsyncApp, Global, KeyUpEvent, Keystroke, Modifiers, MouseButton, MouseDownEvent,
    MouseMoveEvent, MouseUpEvent, PlatformInput, ScrollDelta, ScrollEvent, TouchPhase, Window,
};
use serde_json::{json, Map, Value};

use crate::tree::{self, AxNode};

/// An agent failure, reported as `{"error", "code"}`.
struct Fail {
    code: &'static str,
    msg: String,
}

fn fail(code: &'static str, msg: impl Into<String>) -> Fail {
    Fail { code, msg: msg.into() }
}

type Out = Result<Value, Fail>;

/// What a ref named at its snapshot: re-resolved by role, name and rank, like the Tauri agent.
#[derive(Clone)]
struct RefMeta {
    role: String,
    name: String,
    nth: usize,
}

#[derive(Default)]
struct WindowState {
    refs: HashMap<String, RefMeta>,
    signature: u64,
    mutated_ms: u64,
    search: Option<Search>,
}

/// A search by scrolling under way: `scroll` was given a target the window does not draw. GPUI
/// describes only what it draws, so what a scroll area holds further along is in no tree: the
/// only way to it is to scroll and look again. Kept between calls, as the server asks again
/// while the search goes on; any other input to the window ends it (`run`).
#[derive(Clone, Debug, Default, PartialEq)]
struct Search {
    target: String,
    /// The scroll area searched, among those in view, largest first.
    area: usize,
    /// Up to its start first, then down to its end.
    down: bool,
    /// Wheels that moved the area on the way up: the way back when nothing is found.
    up: usize,
    /// Wheels sent to this area: a list that never ends is left after `SEARCH_WHEELS_PER_AREA`.
    wheels: usize,
    /// This area's start was never seen: the way up ran out of wheels.
    partial: bool,
    /// Areas left before both their ends were seen, for the final answer.
    cut: usize,
}

impl Search {
    /// The search for `target` under way, or a new one; moved on to the next area, or down,
    /// when the wheels of the way up or of the whole area ran out (content that keeps changing
    /// never looks still, a list that never ends never stops).
    fn resume(prev: Option<Search>, target: &str) -> Search {
        let mut s = prev.filter(|s| s.target == target).unwrap_or_else(|| Search { target: target.to_owned(), ..Default::default() });
        if s.wheels >= SEARCH_WHEELS_PER_AREA {
            s = s.next_area(true);
        }
        if !s.down && s.wheels >= SEARCH_WHEELS_PER_AREA / 2 {
            s.down = true;
            s.partial = true;
        }
        s
    }

    fn next_area(self, cut: bool) -> Search {
        Search { target: self.target, area: self.area + 1, cut: self.cut + usize::from(cut || self.partial), ..Default::default() }
    }

    /// After a wheel and a frame: an area that did not move is at its start (the search turns
    /// down) or at its end (the search moves to the next area). In that last case, the wheels
    /// that moved it on the way up: how far below its start to give it back.
    fn moved(&mut self, moved: bool) -> Option<usize> {
        if moved {
            if !self.down {
                self.up += 1;
            }
            None
        } else if !self.down {
            self.down = true;
            None
        } else {
            let up = self.up;
            *self = std::mem::take(self).next_area(false);
            Some(up)
        }
    }

    /// The answer once no area is left to search (`in_view` scroll areas in view).
    fn exhausted(&self, in_view: usize) -> Fail {
        if self.area == 0 && in_view == 0 {
            // Nothing was scrolled: the target may still come (a page that loads), as for any
            // other target, the server asks again until its timeout.
            return fail("notfound", format!("{} matched no element, and the window shows no scroll area to look further in", self.target));
        }
        // Not `notfound`: the server would ask again, and the search start over.
        let mut msg = format!("{} matched no element: not in the {} scroll area(s) searched, each scrolled from its start to its end", self.target, self.area);
        if self.cut > 0 {
            msg.push_str(&format!(
                " ({} of them only in part: content that kept changing, or longer than {SEARCH_WHEELS_PER_AREA} wheels)",
                self.cut
            ));
        }
        fail("error", msg)
    }
}

/// Ends the search under way in this window, if any.
fn forget_search(label: &str, cx: &mut App) {
    if let Some(s) = cx.default_global::<States>().0.get_mut(label) {
        s.search = None;
    }
}

#[derive(Default)]
struct States(HashMap<String, WindowState>);

impl Global for States {}

fn now_ms() -> u64 {
    SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_millis() as u64).unwrap_or(0)
}

pub async fn run(handle: AnyWindowHandle, op: &str, args: Value, deadline: Instant, cx: AsyncApp) -> Result<Value, String> {
    let label = crate::label_of(handle);
    let envelope = |f: Fail| json!({ "error": f.msg, "code": f.code });
    // Reads see the frame after every event already sent: GPUI rebuilds the accessibility tree
    // when it draws, so each operation first waits for a fresh frame (bounded).
    fresh_frame(handle, &cx).await?;
    crate::expired(op, deadline)?;
    // A search resumes only on the page it went through: any other input may have changed it.
    if INPUT.contains(&op) {
        cx.update(|cx| forget_search(&label, cx));
    }
    // A target scrolled out of its scroll area is brought into view first, as a browser does
    // before a click: the tree still names it, but no event can reach it where it is.
    if let Some((_, key)) = REVEALING.iter().find(|(o, _)| *o == op) {
        if args.get(*key).is_some_and(|l| !l.is_null()) {
            if let Err(f) = reveal(handle, &label, op, &args, key, deadline, &cx).await? {
                return Ok(envelope(f));
            }
            // `scroll` to a target asks for it to be brought into view: when the window does not
            // draw it at all, it is looked for by scrolling, then revealed like any other.
            if op == "scrollIntoView" {
                if let Err(f) = search(handle, &label, &args, key, deadline, &cx).await? {
                    return Ok(envelope(f));
                }
                if let Err(f) = reveal(handle, &label, op, &args, key, deadline, &cx).await? {
                    return Ok(envelope(f));
                }
            }
        }
    }
    let mut args = args;
    // Typing or pressing on a target: focus it first, let a frame show the focus, and type only
    // once the target holds it — never into whatever had the focus before.
    if matches!(op, "type" | "press") && args.get("loc").is_some_and(|l| !l.is_null()) {
        let clicked = cx
            .update(|cx| handle.update(cx, |_, window, cx| focus_target(&label, &args, op == "type", window, cx)))
            .map_err(|e| format!("window closed: {e}"))?;
        match clicked {
            Err(f) => return Ok(envelope(f)),
            Ok(false) => {}
            Ok(true) => {
                fresh_frame(handle, &cx).await?;
                crate::expired(op, deadline)?;
                let focused = cx
                    .update(|cx| handle.update(cx, |_, window, cx| target_focused(&label, &args, window, cx)))
                    .map_err(|e| format!("window closed: {e}"))?;
                if let Err(f) = focused {
                    return Ok(envelope(f));
                }
            }
        }
        if let Some(o) = args.as_object_mut() {
            o.insert("loc".into(), Value::Null);
        }
    }
    let result = cx
        .update(|cx| handle.update(cx, |_, window, cx| dispatch(&label, op, &args, window, cx)))
        .map_err(|e| format!("window closed: {e}"))?;
    Ok(result.unwrap_or_else(envelope))
}

/// The operations that act on a target, with the argument naming it. A drag reveals where it
/// starts only: scrolling to its end could take the start away.
const REVEALING: [(&str, &str); 8] = [
    ("click", "loc"),
    ("type", "loc"),
    ("press", "loc"),
    ("hover", "loc"),
    ("drag", "from"),
    ("point", "loc"),
    ("rect", "loc"),
    ("scrollIntoView", "loc"),
];

/// The operations that send input to the window: they end a search under way.
const INPUT: [&str; 8] = ["click", "clickAt", "type", "press", "hover", "drag", "wheel", "scrollBy"];

/// The wheels one reveal sends at most: one per nested scroll area, a few more for a list that
/// lays its rows out as they come.
const REVEAL_STEPS: usize = 8;

/// Scrolls the target of `op` into view when a scroll area hides it. The inner error is the
/// agent's (the target cannot be brought into view); a target that is not found, or needs no
/// scroll, is left to the operation itself.
async fn reveal(
    handle: AnyWindowHandle,
    label: &str,
    op: &str,
    args: &Value,
    key: &str,
    deadline: Instant,
    cx: &AsyncApp,
) -> Result<Result<(), Fail>, String> {
    let mut last = None;
    for _ in 0..REVEAL_STEPS {
        let step = cx
            .update(|cx| handle.update(cx, |_, window, cx| reveal_step(label, args, key, last, window, cx)))
            .map_err(|e| format!("window closed: {e}"))?;
        match step {
            Err(f) => return Ok(Err(f)),
            Ok(None) => break,
            Ok(Some(w)) => last = Some(w),
        }
        // Without a new frame the tree still shows the target where it was: the next step
        // would take the area for one that does not move. The operation reads it as it is.
        let drawn = next_frame(handle, cx).await?;
        crate::expired(op, deadline)?;
        if !drawn {
            break;
        }
    }
    Ok(Ok(()))
}

/// A wheel `reveal` sent: where, how far, and where the target was before it.
#[derive(Clone, Copy, Debug)]
struct Wheeled {
    x: f64,
    y: f64,
    dx: f64,
    dy: f64,
    at: (f64, f64),
}

#[derive(Debug, PartialEq)]
enum Progress {
    /// The target moved on an axis the wheel was sent on.
    Moved,
    /// The scroll area did not move.
    Still,
    /// It moved on the other axis only: GPUI turns a wheel on one axis into a scroll on the
    /// other in an area that scrolls only that other way (`div.rs`, `overflow` handling).
    OtherAxis,
}

fn progress(w: &Wheeled, now: (f64, f64)) -> Progress {
    let moved_x = (now.0 - w.at.0).abs() >= 0.5;
    let moved_y = (now.1 - w.at.1).abs() >= 0.5;
    if (w.dx != 0. && moved_x) || (w.dy != 0. && moved_y) {
        Progress::Moved
    } else if moved_x || moved_y {
        Progress::OtherAxis
    } else {
        Progress::Still
    }
}

/// One wheel towards the target. `None` when there is nothing to scroll: the target is in view,
/// or not found (the operation says so). Else the wheel sent, so the next step can tell a scroll
/// area that did not move, or that moved the other way.
fn reveal_step(label: &str, args: &Value, key: &str, last: Option<Wheeled>, window: &mut Window, cx: &mut App) -> Result<Option<Wheeled>, Fail> {
    let page = Page::read(window)?;
    let Ok(n) = resolve(label, &page, loc(args, key)?, cx) else { return Ok(None) };
    if page.visible(n) {
        return Ok(None);
    }
    let Some((x, y, dx, dy)) = page.scroll_towards(n) else { return Ok(None) };
    if let Some(w) = last {
        match progress(&w, (n.x, n.y)) {
            Progress::Moved => {}
            Progress::Still => {
                return Err(fail(
                    "hidden",
                    format!("{} is scrolled out of view and the scroll area under ({x:.0}, {y:.0}) did not move: scroll it with wheel", n.describe()),
                ))
            }
            Progress::OtherAxis => {
                // Give the area back, and stop: every retry would scroll it the wrong way again.
                wheel(window, cx, w.x, w.y, -w.dx, -w.dy);
                return Err(fail(
                    "error",
                    format!(
                        "{} is out of view {} and the scroll area under ({:.0}, {:.0}) scrolls only the other way: scroll it with wheel",
                        n.describe(),
                        if w.dx != 0. { "to the side" } else { "above or below" },
                        w.x,
                        w.y
                    ),
                ));
            }
        }
    }
    wheel(window, cx, x, y, dx, dy);
    Ok(Some(Wheeled { x, y, dx, dy, at: (n.x, n.y) }))
}

/// The wheels one call spends on a search before answering « still searching » (the server asks
/// again until its own timeout), and those one scroll area gets in all.
const SEARCH_WHEELS_PER_CALL: usize = 12;
const SEARCH_WHEELS_PER_AREA: usize = 80;
/// How far one wheel of a search goes, in heights of the area: rows overlap from one to the next.
const SEARCH_STEP: f64 = 0.85;

/// Looks for a target the window does not draw by scrolling the areas in view: each one up to
/// its start, then down to its end, and back to about where it was when the target is not in it.
/// `Ok(())` once the tree names the target (or it did from the start).
async fn search(handle: AnyWindowHandle, label: &str, args: &Value, key: &str, deadline: Instant, cx: &AsyncApp) -> Result<Result<(), Fail>, String> {
    let closed = |e| format!("window closed: {e}");
    for _ in 0..SEARCH_WHEELS_PER_CALL {
        // Leave the time to answer: an expired call would report nothing of where the search is.
        if Instant::now() + Duration::from_millis(500) >= deadline {
            break;
        }
        let step = cx.update(|cx| handle.update(cx, |_, window, cx| search_step(label, args, key, window, cx))).map_err(closed)?;
        let (area, before) = match step {
            Err(f) => return Ok(Err(f)),
            Ok(None) => return Ok(Ok(())),
            Ok(Some(wheeled)) => wheeled,
        };
        // Without a new frame the tree is the one before the wheel: the area would look still,
        // and the search take it for one at its start or at its end.
        if !drawn_frame(handle, cx).await? {
            cx.update(|cx| forget_search(label, cx));
            return Ok(Err(fail("error", "the window drew no frame after a wheel (minimised, hidden or busy?): the search for the target stopped")));
        }
        crate::expired("scrollIntoView", deadline)?;
        let back = match cx.update(|cx| handle.update(cx, |_, window, cx| search_moved(label, area, before, window, cx))).map_err(closed)? {
            Ok(back) => back,
            Err(f) => return Ok(Err(f)),
        };
        if let Some((x, y, dy)) = back {
            // GPUI adds up the wheels of one frame before it bounds the offset: the jump to the
            // start must be drawn before the way back down, or the area stays at its start; and
            // before the next area is read, or it is read as it was before the jump.
            if !drawn_frame(handle, cx).await? {
                cx.update(|cx| forget_search(label, cx));
                return Ok(Err(fail("error", "the window drew no frame after a wheel (minimised, hidden or busy?): the search for the target stopped")));
            }
            crate::expired("scrollIntoView", deadline)?;
            if dy > 0. {
                cx.update(|cx| handle.update(cx, |_, window, cx| wheel(window, cx, x, y, 0., dy))).map_err(closed)?;
                drawn_frame(handle, cx).await?;
            }
        }
    }
    Ok(Err(fail("notfound", "the target is not drawn yet: still searching the scroll areas in view")))
}

/// One wheel of a search. `None` when the tree names the target (found, or never missing); else
/// the area wheeled and its signature before the wheel, for `search_moved` to tell whether it
/// moved.
fn search_step(label: &str, args: &Value, key: &str, window: &mut Window, cx: &mut App) -> Result<Option<(Rect, u64)>, Fail> {
    let page = Page::read(window)?;
    let l = loc(args, key)?;
    let target = l.desc.clone();
    let by_ref = l.kind == "ref";
    match resolve(label, &page, l, cx) {
        Ok(_) => {
            forget_search(label, cx);
            return Ok(None);
        }
        // A ref names what a snapshot saw: it is not looked for elsewhere. Any other failure
        // (several matches, a bad target) is the operation's to report, and ends the search.
        Err(f) if f.code != "notfound" || by_ref => {
            forget_search(label, cx);
            return Err(f);
        }
        Err(_) => {}
    }
    let areas = page.scroll_areas();
    let prev = cx.default_global::<States>().0.get(label).and_then(|s| s.search.clone());
    let mut search = Search::resume(prev, &target);
    let Some(&area) = areas.get(search.area) else {
        forget_search(label, cx);
        return Err(search.exhausted(areas.len()));
    };
    let (x0, y0, x1, y1) = area;
    let step = (y1 - y0) * SEARCH_STEP;
    wheel(window, cx, (x0 + x1) / 2., (y0 + y1) / 2., 0., if search.down { step } else { -step });
    search.wheels += 1;
    cx.default_global::<States>().0.entry(label.to_owned()).or_default().search = Some(search);
    Ok(Some((area, page.area_signature(area))))
}

/// After the wheel of `search_step` and a frame: an area that did not move is at its start (the
/// search turns down) or at its end (the next area is searched). Leaving an area, it is sent to
/// its start here, and `Some((x, y, dy))` is the wheel that brings it back about where it was
/// (`dy` 0: it was at its start), to send once that jump is drawn.
fn search_moved(label: &str, area: Rect, before: u64, window: &mut Window, cx: &mut App) -> Result<Option<(f64, f64, f64)>, Fail> {
    let page = Page::read(window)?;
    let moved = page.area_signature(area) != before;
    let up = match cx.default_global::<States>().0.get_mut(label).and_then(|s| s.search.as_mut()) {
        Some(search) => search.moved(moved),
        None => None,
    };
    let Some(up) = up else { return Ok(None) };
    let (x0, y0, x1, y1) = area;
    let (x, y) = ((x0 + x1) / 2., (y0 + y1) / 2.);
    wheel(window, cx, x, y, 0., -1e7);
    Ok(Some((x, y, up as f64 * (y1 - y0) * SEARCH_STEP)))
}

/// The point an action on `n` uses: the centre of its visible part.
fn point_of(page: &Page, n: &Flat) -> Result<(f64, f64), Fail> {
    page.click_point(n).ok_or_else(|| fail("hidden", format!("{} is not visible", n.describe())))
}

/// Clicks the target of a `type`/`press` unless it has the focus already. True when it clicked.
fn focus_target(label: &str, args: &Value, need_enabled: bool, window: &mut Window, cx: &mut App) -> Result<bool, Fail> {
    let page = Page::read(window)?;
    let n = actionable(label, &page, loc(args, "loc")?, need_enabled, cx)?;
    // `type` goes into a text field only: clicking anything else to « focus » it would act on it.
    if need_enabled && !n.editable {
        return Err(fail("invalid", format!("{} is not a text field: type needs a textbox", n.describe())));
    }
    if n.focused {
        return Ok(false);
    }
    let (x, y) = page.click_point(n).ok_or_else(|| fail("hidden", format!("{} is not visible", n.describe())))?;
    click_at(window, cx, x, y, MouseButton::Left, 1);
    Ok(true)
}

/// After the focusing click: the target must report the focus now.
fn target_focused(label: &str, args: &Value, window: &mut Window, cx: &mut App) -> Result<(), Fail> {
    let page = Page::read(window)?;
    let n = resolve(label, &page, loc(args, "loc")?, cx)?;
    if n.focused {
        Ok(())
    } else {
        Err(fail("invalid", format!("{} did not take the focus after a click; nothing was typed", n.describe())))
    }
}

/// Activates accessibility when needed and waits (bounded) for the next frame.
pub(crate) async fn fresh_frame(handle: AnyWindowHandle, cx: &AsyncApp) -> Result<(), String> {
    next_frame(handle, cx).await.map(|_| ())
}

/// `fresh_frame`, telling whether the frame came before the wait ran out.
async fn next_frame(handle: AnyWindowHandle, cx: &AsyncApp) -> Result<bool, String> {
    for _ in 0..3 {
        let (tx, rx) = async_channel::bounded::<()>(1);
        let frame = cx
            .update(|cx| {
                handle.update(cx, |_, window, cx| -> Result<Frame, String> {
                    let active = tree::ensure_active(window)?;
                    // macOS requests no frame for a window nobody sees (covered by other windows,
                    // minimised, hidden, on another Space): the tree activated above would never
                    // be built, and every read would see the frame from before. Draw it here, as
                    // GPUI's own frame callback does; it is presented once the window shows.
                    if !window.is_visible() {
                        window.refresh();
                        window.draw(cx).clear(cx);
                        return Ok(Frame::Drawn { active: window.is_a11y_active() });
                    }
                    window.on_next_frame(move |_, _| {
                        let _ = tx.try_send(());
                    });
                    window.refresh();
                    Ok(Frame::Requested { active })
                })
            })
            .map_err(|e| format!("window closed: {e}"))??;
        let active = match frame {
            Frame::Drawn { active: true } => return Ok(true),
            Frame::Drawn { active: false } => continue,
            Frame::Requested { active } => active,
        };
        let timer = cx.background_executor().timer(Duration::from_millis(if active { 300 } else { 1500 }));
        let drawn = futures_lite::future::or(async { rx.recv().await.is_ok() }, async {
            timer.await;
            false
        })
        .await;
        if active {
            return Ok(drawn);
        }
    }
    Err(TREE_NOT_STARTED.into())
}

/// What `next_frame` did for one frame: drew it itself (the window is not visible), or asked
/// the platform for it. `active`: the accessibility tree was running before it.
enum Frame {
    Drawn { active: bool },
    Requested { active: bool },
}

/// Hidden, minimised and covered windows are drawn by the bridge itself: what is left is a
/// window that draws no frame, or AccessKit refusing to start.
const TREE_NOT_STARTED: &str = "the window's accessibility tree did not start: the window drew no frame building it \
     (main thread blocked, or a display that never refreshes?) or AccessKit did not activate for it";

/// Waits for a frame actually drawn, giving a slow one (a large tree) a few chances. False when
/// none came: the tree is the one from before.
async fn drawn_frame(handle: AnyWindowHandle, cx: &AsyncApp) -> Result<bool, String> {
    for _ in 0..3 {
        if next_frame(handle, cx).await? {
            return Ok(true);
        }
    }
    Ok(false)
}

fn dispatch(label: &str, op: &str, args: &Value, window: &mut Window, cx: &mut App) -> Out {
    let s = |k: &str| args.get(k).and_then(Value::as_str).unwrap_or_default().to_owned();
    let f = |k: &str| args.get(k).and_then(Value::as_f64).unwrap_or(0.);
    match op {
        "ping" => Ok(json!({ "ok": true })),
        "info" => info(label, window, cx),
        "snapshot" => snapshot(label, &s("level"), &s("selector"), window, cx),
        "state" => {
            let page = Page::read(window)?;
            state(label, &page, loc(args, "loc")?, &s("state"), cx)
        }
        "text" => {
            let page = Page::read(window)?;
            let want = s("text");
            Ok(json!({ "ok": page.flat.iter().any(|n| n.name.contains(&want) || n.value.contains(&want)) }))
        }
        "click" => {
            let page = Page::read(window)?;
            let n = actionable(label, &page, loc(args, "loc")?, true, cx)?;
            let button = match s("button").as_str() {
                "right" => MouseButton::Right,
                "middle" => MouseButton::Middle,
                _ => MouseButton::Left,
            };
            let count = args.get("count").and_then(Value::as_u64).unwrap_or(1).max(1) as usize;
            let (x, y) = point_of(&page, n)?;
            click_at(window, cx, x, y, button, count);
            Ok(json!({ "note": "" }))
        }
        "clickAt" => {
            let count = args.get("count").and_then(Value::as_u64).unwrap_or(1).max(1) as usize;
            click_at(window, cx, f("x"), f("y"), MouseButton::Left, count);
            Ok(json!({}))
        }
        "type" => {
            if args.get("loc").is_some_and(|l| !l.is_null()) {
                let page = Page::read(window)?;
                let n = actionable(label, &page, loc(args, "loc")?, false, cx)?;
                if !n.focused {
                    let (x, y) = point_of(&page, n)?;
                    click_at(window, cx, x, y, MouseButton::Left, 1);
                }
            }
            let append = args.get("append").and_then(Value::as_bool).unwrap_or(false);
            if !append {
                key(window, cx, keystroke("a", Modifiers { platform: true, ..Default::default() }));
                key(window, cx, keystroke("backspace", Modifiers::default()));
            }
            let text = s("text");
            if !text.is_empty() && !insert_text(window, cx, &text) {
                return Err(fail("invalid", "type: nothing focused takes text input (click a text field first, or give type a target)"));
            }
            if args.get("submit").and_then(Value::as_bool).unwrap_or(false) {
                key(window, cx, keystroke("enter", Modifiers::default()));
            }
            Ok(json!({}))
        }
        "press" => {
            if args.get("loc").is_some_and(|l| !l.is_null()) {
                let page = Page::read(window)?;
                let n = actionable(label, &page, loc(args, "loc")?, false, cx)?;
                if !n.focused {
                    let (x, y) = point_of(&page, n)?;
                    click_at(window, cx, x, y, MouseButton::Left, 1);
                }
            }
            let combo = args.get("combo").ok_or_else(|| fail("invalid", "press needs a combo"))?;
            let combo = match combo {
                Value::String(s) => s.clone(),
                // The Tauri agent's parsed form: {mods: [...], key}.
                Value::Object(o) => {
                    let mut parts: Vec<String> = o
                        .get("mods")
                        .and_then(Value::as_array)
                        .map(|m| m.iter().filter_map(Value::as_str).map(str::to_owned).collect())
                        .unwrap_or_default();
                    parts.push(o.get("key").and_then(Value::as_str).unwrap_or_default().to_owned());
                    parts.join("+")
                }
                _ => return Err(fail("invalid", "press: combo must be a string")),
            };
            let k = crate::keys::parse(&combo).map_err(|e| fail("invalid", e))?;
            key(window, cx, k);
            Ok(json!({}))
        }
        "hover" => {
            let page = Page::read(window)?;
            let n = actionable(label, &page, loc(args, "loc")?, false, cx)?;
            let (x, y) = point_of(&page, n)?;
            mouse_move(window, cx, x, y, None);
            Ok(json!({}))
        }
        "drag" => {
            let page = Page::read(window)?;
            let from = point_of(&page, actionable(label, &page, loc(args, "from")?, false, cx)?)?;
            let to = point_of(&page, actionable(label, &page, loc(args, "to")?, false, cx)?)?;
            let steps = args.get("steps").and_then(Value::as_u64).unwrap_or(10).clamp(1, 200);
            mouse_move(window, cx, from.0, from.1, None);
            window.dispatch_event(
                PlatformInput::MouseDown(MouseDownEvent {
                    button: MouseButton::Left,
                    position: point(px(from.0 as f32), px(from.1 as f32)),
                    modifiers: Modifiers::default(),
                    click_count: 1,
                    first_mouse: false,
                }),
                cx,
            );
            for i in 1..=steps {
                let t = i as f64 / steps as f64;
                mouse_move(window, cx, from.0 + (to.0 - from.0) * t, from.1 + (to.1 - from.1) * t, Some(MouseButton::Left));
            }
            window.dispatch_event(
                PlatformInput::MouseUp(MouseUpEvent {
                    button: MouseButton::Left,
                    position: point(px(to.0 as f32), px(to.1 as f32)),
                    modifiers: Modifiers::default(),
                    click_count: 1,
                }),
                cx,
            );
            Ok(json!({}))
        }
        "scrollIntoView" => {
            let page = Page::read(window)?;
            let n = resolve(label, &page, loc(args, "loc")?, cx)?;
            // `run` scrolled it into view already when a scroll area hid it.
            if page.visible(n) {
                Ok(json!({}))
            } else {
                Err(fail("hidden", format!("{} is outside the window and no scroll area holds it: nothing to scroll", n.describe())))
            }
        }
        "scrollBy" => {
            let size = window.viewport_size();
            let (x, y) = (f64::from(size.width.as_f32()) / 2., f64::from(size.height.as_f32()) / 2.);
            wheel(window, cx, x, y, f("dx"), f("dy"));
            Ok(json!({ "pos": 0 }))
        }
        "point" | "rect" => {
            let page = Page::read(window)?;
            let n = actionable(label, &page, loc(args, "loc")?, false, cx)?;
            let (x, y) = point_of(&page, n)?;
            // `x`/`y` are the centre (the Tauri agent's answer); the box serves a cropped capture.
            Ok(json!({ "x": x, "y": y, "left": n.x, "top": n.y, "width": n.w, "height": n.h }))
        }
        "wheel" => {
            wheel(window, cx, f("x"), f("y"), f("dx"), f("dy"));
            Ok(json!({}))
        }
        "select" | "upload" | "navigate" | "history" | "reload" | "take" | "drop" => Err(fail(
            "invalid",
            format!("{op} is not available in a GPUI app (no DOM): use click/type/press, or invoke an app command"),
        )),
        other => Err(fail("invalid", format!("unknown operation {other}"))),
    }
}

// ---------------------------------------------------------------- input

fn mouse_move(window: &mut Window, cx: &mut App, x: f64, y: f64, pressed: Option<MouseButton>) {
    window.dispatch_event(
        PlatformInput::MouseMove(MouseMoveEvent {
            position: point(px(x as f32), px(y as f32)),
            pressed_button: pressed,
            modifiers: Modifiers::default(),
        }),
        cx,
    );
}

fn click_at(window: &mut Window, cx: &mut App, x: f64, y: f64, button: MouseButton, count: usize) {
    let position = point(px(x as f32), px(y as f32));
    mouse_move(window, cx, x, y, None);
    for click_count in 1..=count {
        window.dispatch_event(
            PlatformInput::MouseDown(MouseDownEvent {
                button,
                position,
                modifiers: Modifiers::default(),
                click_count,
                first_mouse: false,
            }),
            cx,
        );
        window.dispatch_event(
            PlatformInput::MouseUp(MouseUpEvent { button, position, modifiers: Modifiers::default(), click_count }),
            cx,
        );
    }
}

/// `dy` in DOM terms (positive scrolls down), turned into GPUI's (positive moves content down).
fn wheel(window: &mut Window, cx: &mut App, x: f64, y: f64, dx: f64, dy: f64) {
    let position = point(px(x as f32), px(y as f32));
    mouse_move(window, cx, x, y, None);
    window.dispatch_event(
        PlatformInput::Scroll(ScrollEvent {
            position,
            delta: ScrollDelta::Pixels(point(px(-dx as f32), px(-dy as f32))),
            modifiers: Modifiers::default(),
            touch_phase: TouchPhase::Moved,
            ..Default::default()
        }),
        cx,
    );
}

fn keystroke(key: &str, modifiers: Modifiers) -> Keystroke {
    Keystroke { modifiers, key: key.into(), key_char: None }
}

/// A key press: down then up, as the keyboard sends them (GPUI turns Enter/Space on a focused
/// element into a click on the release).
fn key(window: &mut Window, cx: &mut App, k: Keystroke) {
    window.dispatch_keystroke(k.clone(), cx);
    window.dispatch_event(PlatformInput::KeyUp(KeyUpEvent { keystroke: k }), cx);
}

/// Inserts `text` into the focused input as one committed input (like a paste or an IME
/// commit): newlines stay text instead of pressing Enter. False when nothing takes input.
fn insert_text(window: &mut Window, cx: &mut App, text: &str) -> bool {
    window.dispatch_keystroke(Keystroke { modifiers: Modifiers::default(), key: String::new(), key_char: Some(text.into()) }, cx)
}

// ---------------------------------------------------------------- tree

/// One node of the flattened tree, with its role in ARIA terms.
#[derive(Clone, Debug)]
struct Flat {
    role: String,
    name: String,
    value: String,
    placeholder: String,
    focused: bool,
    enabled: bool,
    selected: Option<bool>,
    x: f64,
    y: f64,
    w: f64,
    h: f64,
    /// Its position in the flattened list.
    index: usize,
    /// Whether it clips its descendants (a scroll area, list, tree or table: see `clips`).
    clips: bool,
    /// Whether text can be typed into it (a text field or an editable combo box; not a pop-up
    /// button, which AccessKit also reports as a combo box).
    editable: bool,
    /// Index of the last descendant: `(i, end]` are this node's descendants.
    end: usize,
}

impl Flat {
    fn describe(&self) -> String {
        if self.name.is_empty() {
            self.role.clone()
        } else {
            format!("{} {:?}", self.role, self.name)
        }
    }
}

/// A box in window coordinates, as `(left, top, right, bottom)`.
type Rect = (f64, f64, f64, f64);

struct Page {
    flat: Vec<Flat>,
    w: f64,
    h: f64,
}

impl Page {
    fn read(window: &mut Window) -> Result<Page, Fail> {
        let roots = tree::read(window).map_err(|e| fail("error", e))?;
        let size = window.viewport_size();
        let mut flat = Vec::new();
        for r in &roots {
            flatten(r, &mut flat);
        }
        Ok(Page { flat, w: f64::from(size.width.as_f32()), h: f64::from(size.height.as_f32()) })
    }

    /// The part of `n` actually on screen: its box clipped by the window and by the ancestors
    /// that clip (scroll areas and lists report their children's full boxes, not the visible
    /// part). Other ancestors may let a child overflow: they do not clip it.
    fn visible_rect(&self, n: &Flat) -> Option<(f64, f64, f64, f64)> {
        let mut r = (n.x, n.y, n.x + n.w, n.y + n.h);
        let clip = |r: (f64, f64, f64, f64), c: (f64, f64, f64, f64)| (r.0.max(c.0), r.1.max(c.1), r.2.min(c.2), r.3.min(c.3));
        r = clip(r, (0., 0., self.w, self.h));
        for a in self.flat[..n.index].iter().filter(|a| a.clips && a.end >= n.index && a.w > 0. && a.h > 0.) {
            r = clip(r, (a.x, a.y, a.x + a.w, a.y + a.h));
        }
        (r.2 - r.0 >= 1. && r.3 - r.1 >= 1.).then_some(r)
    }

    fn visible(&self, n: &Flat) -> bool {
        n.w > 0. && n.h > 0. && self.visible_rect(n).is_some()
    }

    /// The wheel that brings `n` towards view, as `(x, y, dx, dy)` (DOM terms: a positive `dy`
    /// scrolls down): over the visible part of the scroll area to scroll, by what separates
    /// their centres on the axis `n` is out on. `None` when no scroll area hides it: it is in
    /// view, has no size, or lies outside the window with nothing to scroll.
    ///
    /// The area to scroll is the innermost one whose own box leaves `n` out: its content is what
    /// hides `n`, wherever the areas around it stand (an outer area at its end must not be
    /// wheeled in vain). When that area is itself out of sight, the one around it is scrolled to
    /// bring it in first. When no box leaves `n` out, the window does: the outermost area, which
    /// runs past it, is scrolled. A wheel reaches every area under the pointer (GPUI does not
    /// stop it at the first one), so the outer areas follow when they can.
    fn scroll_towards(&self, n: &Flat) -> Option<(f64, f64, f64, f64)> {
        if n.w <= 0. || n.h <= 0. {
            return None;
        }
        let areas: Vec<&Flat> = self.flat[..n.index].iter().filter(|a| a.clips && a.end >= n.index && a.w > 0. && a.h > 0.).collect();
        // What each area shows: its box, within the window and the areas around it.
        let mut views = Vec::with_capacity(areas.len());
        let mut view: Rect = (0., 0., self.w, self.h);
        for a in &areas {
            view = (view.0.max(a.x), view.1.max(a.y), view.2.min(a.x + a.w), view.3.min(a.y + a.h));
            views.push(view);
        }
        let shows = |v: &Rect| v.2 - v.0 >= 1. && v.3 - v.1 >= 1.;
        let towards = |v: &Rect, goal: &Rect, out_x: bool, out_y: bool| {
            let (x, y) = ((v.0 + v.2) / 2., (v.1 + v.3) / 2.);
            let dx = if out_x { (goal.0 + goal.2) / 2. - x } else { 0. };
            let dy = if out_y { (goal.1 + goal.3) / 2. - y } else { 0. };
            (x, y, dx, dy)
        };
        let mut goal: Rect = (n.x, n.y, n.x + n.w, n.y + n.h);
        for (a, v) in areas.iter().zip(&views).rev() {
            let b: Rect = (a.x, a.y, a.x + a.w, a.y + a.h);
            let out_x = goal.2 - b.0 < 1. || b.2 - goal.0 < 1.;
            let out_y = goal.3 - b.1 < 1. || b.3 - goal.1 < 1.;
            if !(out_x || out_y) {
                continue;
            }
            if !shows(v) {
                // No wheel can land on this area: bring it into view first.
                goal = b;
                continue;
            }
            return Some(towards(v, &goal, out_x, out_y));
        }
        // Within every box: what leaves the goal out is the window.
        let v = views.first().filter(|v| shows(v))?;
        let out_x = goal.2 - v.0 < 1. || v.2 - goal.0 < 1.;
        let out_y = goal.3 - v.1 < 1. || v.3 - goal.1 < 1.;
        (out_x || out_y).then(|| towards(v, &goal, out_x, out_y))
    }

    /// What a wheel over `area` may move: the nodes lying in it, by role, name and box. Not
    /// their value, focus or state, which change with no scroll (a progress bar, a clock), nor
    /// anything elsewhere in the window: a search reads a still area as one at its start or end.
    fn area_signature(&self, area: Rect) -> u64 {
        let mut h = DefaultHasher::new();
        for n in self.flat.iter().filter(|n| n.x < area.2 && n.x + n.w > area.0 && n.y < area.3 && n.y + n.h > area.1) {
            (&n.role, &n.name).hash(&mut h);
            ((n.x * 4.) as i64, (n.y * 4.) as i64, (n.w * 4.) as i64, (n.h * 4.) as i64).hash(&mut h);
        }
        h.finish()
    }

    /// The visible part of every scroll area in view, largest first: where a search by scrolling
    /// sends its wheels. One too small to hold a row is left out.
    fn scroll_areas(&self) -> Vec<(f64, f64, f64, f64)> {
        let mut areas: Vec<(f64, f64, f64, f64)> = self
            .flat
            .iter()
            .filter(|n| n.clips && n.w > 0. && n.h > 0.)
            .filter_map(|n| self.visible_rect(n))
            .filter(|r| r.2 - r.0 >= 24. && r.3 - r.1 >= 24.)
            .collect();
        areas.sort_by(|a, b| ((b.2 - b.0) * (b.3 - b.1)).total_cmp(&((a.2 - a.0) * (a.3 - a.1))));
        areas.dedup();
        areas
    }

    /// Why a node that is not visible is not.
    fn why_hidden(&self, n: &Flat) -> &'static str {
        if self.scroll_towards(n).is_some() { "scrolled out of view" } else { "not visible" }
    }

    /// Among `all`, the nodes a person can see; when none is, those a scroll area hides: an
    /// action scrolls them into view first (`reveal`), a wait reports them as not visible.
    fn shown<'a>(&self, all: Vec<&'a Flat>) -> Vec<&'a Flat> {
        let visible: Vec<&Flat> = all.iter().copied().filter(|n| self.visible(n)).collect();
        if !visible.is_empty() {
            return visible;
        }
        all.into_iter().filter(|n| self.scroll_towards(n).is_some()).collect()
    }

    /// Where a click on `n` lands: the centre of its visible part.
    fn click_point(&self, n: &Flat) -> Option<(f64, f64)> {
        self.visible_rect(n).map(|r| ((r.0 + r.2) / 2., (r.1 + r.3) / 2.))
    }

    fn signature(&self) -> u64 {
        let mut h = DefaultHasher::new();
        for n in &self.flat {
            (&n.role, &n.name, &n.value, n.focused, n.enabled).hash(&mut h);
            ((n.x * 4.) as i64, (n.y * 4.) as i64, (n.w * 4.) as i64, (n.h * 4.) as i64).hash(&mut h);
        }
        h.finish()
    }
}

fn flatten(n: &AxNode, out: &mut Vec<Flat>) {
    let role = aria_role(&n.ax_role, &n.ax_subrole);
    let name = name_of(n, &role);
    let index = out.len();
    out.push(Flat {
        value: if role == "StaticText" { String::new() } else { n.value.clone() },
        role,
        name,
        placeholder: n.placeholder.clone(),
        focused: n.focused,
        enabled: n.enabled,
        selected: n.selected,
        x: n.x,
        y: n.y,
        w: n.w,
        h: n.h,
        index,
        clips: clips(&n.ax_role),
        editable: matches!(n.ax_role.as_str(), "TextField" | "TextArea" | "SecureTextField" | "ComboBox"),
        end: index,
    });
    for c in &n.children {
        flatten(c, out);
    }
    out[index].end = out.len() - 1;
}

/// macOS accessibility roles, as AccessKit maps GPUI's, back to ARIA roles.
/// Whether a node of this macOS role clips its descendants. A GPUI scroll view (AccessKit's
/// `ScrollView`, as `Pane`, `Feed` and `ListGrid`) reaches macOS as `Unknown`: AccessKit's
/// adapter has no `AXScrollArea` for it, and keeps the plain containers that share that role
/// (`GenericContainer`) out of the tree, so an `Unknown` node that is read is one of those.
fn clips(ax_role: &str) -> bool {
    matches!(ax_role, "ScrollArea" | "List" | "Outline" | "Table" | "Unknown")
}

fn aria_role(role: &str, subrole: &str) -> String {
    let r = match (role, subrole) {
        // AccessKit gives several ARIA roles one macOS role and tells them apart by subrole.
        ("Group", "ApplicationAlert") => "alert",
        ("Group", "ApplicationStatus") => "status",
        ("Group", "ApplicationLog") => "log",
        ("Group", "ApplicationGroup") => "group",
        ("Group", "LandmarkNavigation") => "navigation",
        ("Group", "LandmarkMain") => "main",
        ("Group", "LandmarkBanner") => "banner",
        ("Group", "LandmarkSearch") => "search",
        ("Group", "LandmarkRegion") => "region",
        ("Group", "LandmarkForm") => "form",
        ("Group", "LandmarkComplementary") => "complementary",
        ("Group", "LandmarkContentInfo") => "contentinfo",
        ("Window", "Dialog") => "dialog",
        ("Button", _) | ("MenuButton", _) | ("DisclosureTriangle", _) => "button",
        ("Link", _) => "link",
        ("TextField", "SearchField") => "searchbox",
        ("TextField", _) | ("TextArea", _) | ("SecureTextField", _) => "textbox",
        ("CheckBox", "Switch") => "switch",
        ("CheckBox", _) => "checkbox",
        ("RadioButton", "TabButton") => "tab",
        ("RadioButton", _) => "radio",
        ("PopUpButton", _) | ("ComboBox", _) => "combobox",
        ("MenuItem", _) | ("MenuBarItem", _) => "menuitem",
        ("Menu", _) => "menu",
        ("MenuBar", _) => "menubar",
        ("Slider", _) => "slider",
        ("Incrementor", _) => "spinbutton",
        ("StaticText", _) => "StaticText",
        ("Heading", _) => "heading",
        ("Image", _) => "img",
        ("List", _) => "list",
        ("Outline", _) => "tree",
        ("Row", "OutlineRow") => "treeitem",
        ("Row", _) => "row",
        ("Cell", _) => "cell",
        ("Table", _) => "table",
        ("TabGroup", _) => "tablist",
        ("Toolbar", _) => "toolbar",
        ("Sheet", _) | ("Dialog", _) => "dialog",
        ("ProgressIndicator", _) => "progressbar",
        ("LevelIndicator", _) => "meter",
        ("Window", _) => "window",
        ("WebArea", _) => "document",
        ("Group", _) | ("ScrollArea", _) | ("SplitGroup", _) | ("Unknown", _) | ("", _) => "generic",
        _ => "",
    };
    if r.is_empty() { role.to_ascii_lowercase() } else { r.to_owned() }
}

fn name_of(n: &AxNode, role: &str) -> String {
    let name = if !n.title.is_empty() {
        n.title.clone()
    } else if !n.label.is_empty() {
        n.label.clone()
    } else if role == "StaticText" {
        n.value.clone()
    } else {
        String::new()
    };
    collapse(&name)
}

fn collapse(s: &str) -> String {
    s.split_whitespace().collect::<Vec<_>>().join(" ")
}

const INTERACTIVE: &[&str] = &[
    "button", "link", "textbox", "checkbox", "radio", "combobox", "menuitem", "tab", "switch", "slider",
    "spinbutton", "option", "searchbox", "listbox", "menuitemcheckbox", "menuitemradio", "treeitem",
];
const SKELETON: &[&str] = &[
    "heading", "menu", "menubar", "dialog", "alertdialog", "navigation", "form", "search", "banner", "main",
    "complementary", "contentinfo", "tablist", "toolbar", "alert", "status",
];
const CONTENT: &[&str] =
    &["StaticText", "text", "paragraph", "listitem", "img", "image", "table", "row", "cell", "list", "tree", "log", "region", "group"];

fn is_interactive(role: &str) -> bool {
    INTERACTIVE.contains(&role)
}

fn info(label: &str, window: &mut Window, cx: &mut App) -> Out {
    let page = Page::read(window)?;
    let mutated = touch(label, &page, cx);
    let size = window.viewport_size();
    Ok(json!({
        "url": format!("gpui://window/{label}"),
        "title": window.window_title(),
        "ready": "complete",
        "boot": "",
        "w": size.width.as_f32(),
        "h": size.height.as_f32(),
        "dpr": window.scale_factor(),
        "scroll": 0,
        "mutated": mutated,
        "now": now_ms(),
    }))
}

/// Records the tree's signature; returns when it last changed (the `Settle` clock).
fn touch(label: &str, page: &Page, cx: &mut App) -> u64 {
    let sig = page.signature();
    let state = cx.default_global::<States>().0.entry(label.to_owned()).or_default();
    if state.signature != sig || state.mutated_ms == 0 {
        state.signature = sig;
        state.mutated_ms = now_ms();
    }
    state.mutated_ms
}

fn snapshot(label: &str, level: &str, selector: &str, window: &mut Window, cx: &mut App) -> Out {
    let level = if level.is_empty() { "content" } else { level };
    let page = Page::read(window)?;
    touch(label, &page, cx);
    let mut warnings = Vec::new();
    if !selector.is_empty() {
        warnings.push(format!("selector {selector:?} ignored: a GPUI window has no CSS; full window instead"));
    }
    let mut counter = 0usize;
    let mut stats = (0usize, 0usize, 0usize);
    let mut refs = HashMap::new();
    let mut nth: HashMap<(String, String), usize> = HashMap::new();
    let mut i = 0;
    let mut nodes = Vec::new();
    while i < page.flat.len() {
        let (mut sub, next) = build(&page, i, level, false, &mut counter, &mut stats, &mut refs, &mut nth);
        nodes.append(&mut sub);
        i = next;
    }
    cx.default_global::<States>().0.entry(label.to_owned()).or_default().refs = refs;
    if page.flat.is_empty() {
        warnings.push("the window exposes no accessibility node: give its elements .id(...) and .role(...)".into());
    }
    Ok(json!({
        "nodes": nodes,
        "stats": { "total_nodes": stats.0, "filtered_nodes": stats.1, "interactive_count": stats.2 },
        "warnings": warnings,
    }))
}

/// Builds the output for node `i` and its subtree; returns the nodes and the next sibling index.
#[allow(clippy::too_many_arguments)]
fn build(
    page: &Page,
    i: usize,
    level: &str,
    parent_named: bool,
    counter: &mut usize,
    stats: &mut (usize, usize, usize),
    refs: &mut HashMap<String, RefMeta>,
    nth: &mut HashMap<(String, String), usize>,
) -> (Vec<Value>, usize) {
    let n = &page.flat[i];
    stats.0 += 1;
    let role = n.role.as_str();
    let visible = page.visible(n);
    let include = visible
        && match level {
            "skeleton" => is_interactive(role) || SKELETON.contains(&role),
            "content" => {
                is_interactive(role)
                    || SKELETON.contains(&role)
                    || (CONTENT.contains(&role) && !(role == "StaticText" && (parent_named || n.name.is_empty())))
            }
            _ => role != "generic" || !n.name.is_empty(),
        };
    let named = parent_named || (include && !n.name.is_empty() && matches!(role, "button" | "link" | "heading" | "tab" | "menuitem" | "checkbox" | "radio" | "switch" | "treeitem"));
    let mut reference = String::new();
    if include && is_interactive(role) {
        *counter += 1;
        reference = format!("@{counter}");
        stats.2 += 1;
        let key = (n.role.clone(), n.name.clone());
        let rank = nth.entry(key).or_insert(0);
        refs.insert(reference.clone(), RefMeta { role: n.role.clone(), name: n.name.clone(), nth: *rank });
        *rank += 1;
    }
    let mut kids = Vec::new();
    let mut j = i + 1;
    while j <= n.end {
        let (mut sub, next) = build(page, j, level, named, counter, stats, refs, nth);
        kids.append(&mut sub);
        j = next;
    }
    if !include {
        return (kids, n.end + 1);
    }
    stats.1 += 1;
    let mut o = Map::new();
    if !reference.is_empty() {
        o.insert("ref".into(), json!(reference));
    }
    o.insert("role".into(), json!(n.role));
    if !n.name.is_empty() {
        o.insert("name".into(), json!(clip(&n.name)));
    }
    let value = if n.value.is_empty() && role == "textbox" { String::new() } else { n.value.clone() };
    if !value.is_empty() && value != n.name {
        o.insert("value".into(), json!(clip(&value)));
    }
    if role == "textbox" && n.value.is_empty() && !n.placeholder.is_empty() {
        o.insert("type".into(), json!(format!("placeholder: {}", clip(&n.placeholder))));
    }
    if matches!(role, "checkbox" | "switch" | "radio" | "tab" | "treeitem" | "option") {
        // macOS reports a check box's, switch's, radio's or tab's state as its value (0 or 1).
        let checked = match n.value.as_str() {
            "1" => Some(true),
            "0" => Some(false),
            _ => n.selected,
        };
        if let Some(checked) = checked {
            o.insert("checked".into(), json!(checked));
            o.remove("value");
        }
    }
    if !n.enabled {
        o.insert("disabled".into(), json!(true));
    }
    if n.focused {
        o.insert("focused".into(), json!(true));
    }
    if !reference.is_empty() {
        o.insert("box".into(), json!({ "x": n.x.round() as i64, "y": n.y.round() as i64, "width": n.w.round() as i64, "height": n.h.round() as i64 }));
    }
    if !kids.is_empty() {
        o.insert("children".into(), Value::Array(kids));
    }
    (vec![Value::Object(o)], n.end + 1)
}

fn clip(s: &str) -> String {
    const MAX: usize = 400;
    if s.chars().count() <= MAX {
        s.to_owned()
    } else {
        let mut out: String = s.chars().take(MAX).collect();
        out.push('…');
        out
    }
}

// ---------------------------------------------------------------- locators

/// A target in the Tauri agent's terms (`{kind, ref | role+name | text | label | css, exact, pattern}`).
struct Loc {
    kind: String,
    reference: String,
    role: String,
    want: String,
    exact: bool,
    pattern: Option<regex::Regex>,
    desc: String,
}

fn loc(args: &Value, key: &str) -> Result<Loc, Fail> {
    let l = args.get(key).filter(|v| v.is_object()).ok_or_else(|| fail("invalid", format!("missing target {key}")))?;
    let g = |k: &str| l.get(k).and_then(Value::as_str).unwrap_or_default().to_owned();
    let kind = g("kind");
    let pattern = match l.get("pattern").filter(|p| p.is_object()) {
        Some(p) => {
            let source = p.get("source").and_then(Value::as_str).unwrap_or_default();
            let flags = p.get("flags").and_then(Value::as_str).unwrap_or_default();
            let ci = if flags.contains('i') { "(?i)" } else { "" };
            Some(regex::Regex::new(&format!("{ci}{source}")).map_err(|e| fail("invalid", format!("pattern /{source}/: {e}")))?)
        }
        None => None,
    };
    let want = match kind.as_str() {
        "role" => g("name"),
        "text" => g("text"),
        "label" => g("label"),
        "css" => g("css"),
        _ => String::new(),
    };
    let desc = match kind.as_str() {
        "ref" => g("ref"),
        "css" => format!("CSS selector {:?}", g("css")),
        _ => {
            let d = g("desc");
            if d.is_empty() { kind.clone() } else { d }
        }
    };
    Ok(Loc { reference: g("ref"), role: g("role"), exact: l.get("exact").and_then(Value::as_bool).unwrap_or(false), kind, want, pattern, desc })
}

impl Loc {
    fn matches(&self, s: &str) -> bool {
        if let Some(p) = &self.pattern {
            return p.is_match(s);
        }
        if self.want.is_empty() {
            return true;
        }
        let s = collapse(s);
        if self.exact {
            s == collapse(&self.want)
        } else {
            s.to_lowercase().contains(&collapse(&self.want).to_lowercase())
        }
    }
}

fn candidates<'a>(label: &str, page: &'a Page, l: &Loc, cx: &mut App) -> Result<Vec<&'a Flat>, Fail> {
    let flat = &page.flat;
    match l.kind.as_str() {
        "ref" => {
            let meta = cx
                .default_global::<States>()
                .0
                .get(label)
                .and_then(|s| s.refs.get(&l.reference))
                .cloned()
                .ok_or_else(|| fail("stale", format!("ref {} not found in the last snapshot: take a new snapshot", l.reference)))?;
            let same: Vec<&Flat> =
                flat.iter().filter(|n| n.role == meta.role && n.name == meta.name && page.visible(n)).collect();
            Ok(same.get(meta.nth).map(|n| vec![*n]).unwrap_or_default())
        }
        "role" => Ok(page.shown(flat.iter().filter(|n| n.role == l.role && l.matches(&n.name)).collect())),
        "label" => Ok(page.shown(
            flat.iter().filter(|n| (is_interactive(&n.role) || n.role == "img") && !n.name.is_empty() && l.matches(&n.name)).collect(),
        )),
        "text" => {
            let hits: Vec<usize> = page
                .shown(flat.iter().filter(|n| (!n.name.is_empty() && l.matches(&n.name)) || (!n.value.is_empty() && l.matches(&n.value))).collect())
                .into_iter()
                .map(|n| n.index)
                .collect();
            // Innermost: drop a hit that contains another one.
            let mut inner: Vec<&Flat> = hits
                .iter()
                .filter(|&&i| !hits.iter().any(|&j| j != i && j > i && j <= flat[i].end))
                .map(|&i| &flat[i])
                .collect();
            if inner.len() > 1 && l.pattern.is_none() && !l.exact {
                let want = collapse(&l.want).to_lowercase();
                let exact: Vec<&Flat> = inner.iter().copied().filter(|n| n.name.to_lowercase() == want).collect();
                if !exact.is_empty() {
                    inner = exact;
                }
            }
            Ok(inner)
        }
        "css" => Err(fail(
            "invalid",
            format!("{}: a GPUI window has no CSS; use getByRole/getByText/getByLabel or a @ref from snapshot", l.desc),
        )),
        other => Err(fail("invalid", format!("unknown target kind {other}"))),
    }
}

fn resolve<'a>(label: &str, page: &'a Page, l: Loc, cx: &mut App) -> Result<&'a Flat, Fail> {
    let c = candidates(label, page, &l, cx)?;
    match c.len() {
        // A GPUI window describes only what it draws: what a scroll area holds further along is
        // in no tree. Say so where it may be the reason, with the call that looks for it.
        0 if l.kind != "ref" && !page.scroll_areas().is_empty() => Err(fail(
            "notfound",
            format!("{} matched no element among what the window draws; further along a scroll area, `scroll` with this target finds it", l.desc),
        )),
        0 => Err(fail("notfound", format!("{} matched no element", l.desc))),
        1 => Ok(c[0]),
        n => Err(fail(
            "ambiguous",
            format!(
                "{} matched {n} elements; target must be unique ({})",
                l.desc,
                c.iter().take(5).map(|n| n.describe()).collect::<Vec<_>>().join(", ")
            ),
        )),
    }
}

fn actionable<'a>(label: &str, page: &'a Page, l: Loc, need_enabled: bool, cx: &mut App) -> Result<&'a Flat, Fail> {
    let n = resolve(label, page, l, cx)?;
    if !page.visible(n) {
        return Err(fail("hidden", format!("{} is {}", n.describe(), page.why_hidden(n))));
    }
    if need_enabled && !n.enabled {
        return Err(fail("disabled", format!("{} is disabled", n.describe())));
    }
    Ok(n)
}

fn state(label: &str, page: &Page, l: Loc, want: &str, cx: &mut App) -> Out {
    let n = match resolve(label, page, l, cx) {
        Ok(n) => n,
        Err(f) if want == "hidden" && f.code == "notfound" => return Ok(json!({ "ok": true })),
        Err(f) => return Err(f),
    };
    let vis = page.visible(n);
    Ok(match want {
        "attached" => json!({ "ok": true }),
        "hidden" => if vis { json!({ "ok": false, "why": "still visible" }) } else { json!({ "ok": true }) },
        "enabled" if !vis => json!({ "ok": false, "why": page.why_hidden(n) }),
        "enabled" => if n.enabled { json!({ "ok": true }) } else { json!({ "ok": false, "why": "disabled" }) },
        // A frame is read after the previous one settled: a node is stable when its box held
        // across the fresh frame this read waited for (good enough without a DOM observer).
        "stable" if !vis => json!({ "ok": false, "why": page.why_hidden(n) }),
        _ => if vis { json!({ "ok": true }) } else { json!({ "ok": false, "why": page.why_hidden(n) }) },
    })
}

#[cfg(test)]
mod tests {
    use super::{aria_role, collapse, Flat, Page};

    fn node(index: usize, end: usize, x: f64, y: f64, w: f64, h: f64) -> Flat {
        Flat {
            clips: true,
            editable: false,
            role: "button".into(),
            name: String::new(),
            value: String::new(),
            placeholder: String::new(),
            focused: false,
            enabled: true,
            selected: None,
            x,
            y,
            w,
            h,
            index,
            end,
        }
    }

    #[test]
    fn clicks_land_in_the_visible_part() {
        // A scroll area (0..200 high) whose child sticks out above it: y -80..20.
        let page = Page { flat: vec![node(0, 1, 0., 0., 300., 200.), node(1, 1, 10., -80., 100., 100.)], w: 800., h: 600. };
        let child = &page.flat[1];
        assert!(page.visible(child));
        let (x, y) = page.click_point(child).unwrap();
        assert_eq!((x, y), (60., 10.));
        // Wholly scrolled out of its area: hidden, no click point.
        let page = Page { flat: vec![node(0, 1, 0., 100., 300., 200.), node(1, 1, 10., 0., 100., 50.)], w: 800., h: 600. };
        assert!(!page.visible(&page.flat[1]));
        assert!(page.click_point(&page.flat[1]).is_none());
        // A parent that does not clip lets its child overflow: still visible, full centre.
        let mut parent = node(0, 1, 0., 100., 300., 200.);
        parent.clips = false;
        let page = Page { flat: vec![parent, node(1, 1, 10., 0., 100., 50.)], w: 800., h: 600. };
        assert_eq!(page.click_point(&page.flat[1]), Some((60., 25.)));
    }

    #[test]
    fn a_scrolled_out_target_is_found_and_scrolled_towards() {
        // A scroll area (y 100..300) in an 800 × 600 window; its child lies below it, at y 500..540.
        let page = Page { flat: vec![node(0, 1, 0., 100., 300., 200.), node(1, 1, 10., 500., 100., 40.)], w: 800., h: 600. };
        let child = &page.flat[1];
        assert!(!page.visible(child));
        assert_eq!(page.why_hidden(child), "scrolled out of view");
        // Wheel over the centre of the area, down by what separates the centres (520 - 200).
        assert_eq!(page.scroll_towards(child), Some((150., 200., 0., 320.)));
        // No visible match: the scrolled-out one is the candidate. A visible one wins over it.
        assert_eq!(page.shown(vec![child]).len(), 1);
        let page = Page {
            flat: vec![node(0, 2, 0., 100., 300., 200.), node(1, 1, 10., 500., 100., 40.), node(2, 2, 10., 120., 100., 40.)],
            w: 800.,
            h: 600.,
        };
        let shown = page.shown(vec![&page.flat[1], &page.flat[2]]);
        assert_eq!(shown.iter().map(|n| n.index).collect::<Vec<_>>(), vec![2]);
        assert_eq!(page.scroll_towards(&page.flat[2]), None);

        // Above and to the left of its area: both axes, towards negative.
        let page = Page { flat: vec![node(0, 1, 100., 100., 300., 200.), node(1, 1, 0., 0., 50., 40.)], w: 800., h: 600. };
        assert_eq!(page.scroll_towards(&page.flat[1]), Some((250., 200., -225., -180.)));

        // Nested areas, the inner one out of sight: the outer one brings it in first (towards
        // the inner area's centre, 500), not straight towards the target.
        let page = Page {
            flat: vec![node(0, 2, 0., 0., 400., 300.), node(1, 2, 0., 400., 400., 200.), node(2, 2, 10., 700., 100., 40.)],
            w: 800.,
            h: 600.,
        };
        assert_eq!(page.scroll_towards(&page.flat[2]), Some((200., 150., 0., 350.)));

        // Nested areas, the inner one in sight: the inner one holds the target in its content
        // (below its box, y 350..550) and is wheeled, even though the target is out of the outer
        // view too (an outer area at its end would not move).
        let page = Page {
            flat: vec![node(0, 2, 0., 0., 400., 600.), node(1, 2, 0., 350., 400., 200.), node(2, 2, 10., 600., 100., 40.)],
            w: 800.,
            h: 600.,
        };
        assert_eq!(page.scroll_towards(&page.flat[2]), Some((200., 450., 0., 170.)));

        // Within its area's box, below the window that the area runs past: the area is scrolled.
        let page = Page { flat: vec![node(0, 1, 0., 40., 300., 900.), node(1, 1, 10., 700., 100., 40.)], w: 800., h: 600. };
        assert_eq!(page.scroll_towards(&page.flat[1]), Some((150., 320., 0., 400.)));

        // Outside the window with no scroll area above it: nothing to scroll, plainly not visible.
        let mut parent = node(0, 1, 0., 0., 800., 600.);
        parent.clips = false;
        let page = Page { flat: vec![parent, node(1, 1, 10., 900., 100., 40.)], w: 800., h: 600. };
        assert_eq!(page.scroll_towards(&page.flat[1]), None);
        assert_eq!(page.why_hidden(&page.flat[1]), "not visible");
        assert!(page.shown(vec![&page.flat[1]]).is_empty());
        // No size: never a candidate.
        let page = Page { flat: vec![node(0, 1, 0., 100., 300., 200.), node(1, 1, 10., 500., 0., 0.)], w: 800., h: 600. };
        assert!(page.shown(vec![&page.flat[1]]).is_empty());
    }

    #[test]
    fn a_search_wheels_the_scroll_areas_in_view_largest_first() {
        let mut heading = node(1, 1, 0., 0., 800., 40.);
        heading.clips = false;
        let mut root = node(0, 4, 0., 0., 800., 600.);
        root.clips = false;
        // A list (200 × 300) beside a detail (500 × 560 in the window: it sticks out below), and
        // a strip too thin to hold a row.
        let page = Page {
            flat: vec![root, heading, node(2, 2, 0., 40., 200., 300.), node(3, 3, 220., 40., 500., 700.), node(4, 4, 0., 590., 800., 10.)],
            w: 800.,
            h: 600.,
        };
        assert_eq!(page.scroll_areas(), vec![(220., 40., 720., 600.), (0., 40., 200., 340.)]);
    }

    #[test]
    fn a_reveal_tells_a_still_area_from_one_that_scrolls_the_other_way() {
        use super::{progress, Progress, Wheeled};
        let down = Wheeled { x: 100., y: 100., dx: 0., dy: 300., at: (10., 500.) };
        assert_eq!(progress(&down, (10., 200.)), Progress::Moved);
        assert_eq!(progress(&down, (10.2, 500.2)), Progress::Still);
        // A wheel to the side in an area that scrolls only up and down: GPUI scrolls it vertically.
        let side = Wheeled { x: 100., y: 100., dx: 400., dy: 0., at: (900., 120.) };
        assert_eq!(progress(&side, (900., -280.)), Progress::OtherAxis);
        assert_eq!(progress(&side, (500., 120.)), Progress::Moved);
        // Both axes: moving on either is progress.
        let both = Wheeled { dx: 400., dy: 300., ..side };
        assert_eq!(progress(&both, (900., -180.)), Progress::Moved);
    }

    #[test]
    fn a_search_goes_up_then_down_then_to_the_next_area() {
        use super::{Search, SEARCH_WHEELS_PER_AREA};
        let mut s = Search::resume(None, "getByText('x')");
        assert_eq!((s.area, s.down, s.up, s.wheels), (0, false, 0, 0));
        // Up twice, then still: at its start, the search turns down.
        s.wheels += 1;
        assert_eq!(s.moved(true), None);
        s.wheels += 1;
        assert_eq!(s.moved(true), None);
        s.wheels += 1;
        assert_eq!(s.moved(false), None);
        assert!(s.down && s.up == 2);
        // Down, then still: at its end, the next area; the way back is the two wheels up.
        s.wheels += 1;
        assert_eq!(s.moved(true), None);
        s.wheels += 1;
        assert_eq!(s.moved(false), Some(2));
        assert_eq!(s, Search { target: "getByText('x')".into(), area: 1, ..Default::default() });

        // Resumed for the same target only.
        assert_eq!(Search::resume(Some(s.clone()), "getByText('x')").area, 1);
        assert_eq!(Search::resume(Some(s.clone()), "getByText('y')").area, 0);

        // A start never seen (content that keeps changing): down after half the wheels.
        let up = Search { target: "t".into(), wheels: SEARCH_WHEELS_PER_AREA / 2, ..Default::default() };
        let s = Search::resume(Some(up), "t");
        assert!(s.down && s.partial && s.area == 0);
        // An end never seen: the next area after all of them, counted as cut.
        let long = Search { target: "t".into(), down: true, wheels: SEARCH_WHEELS_PER_AREA, ..Default::default() };
        let s = Search::resume(Some(long), "t");
        assert_eq!((s.area, s.cut, s.wheels, s.down), (1, 1, 0, false));
        // A partial area that reaches its end is counted as cut too.
        let mut partial = Search { target: "t".into(), down: true, partial: true, ..Default::default() };
        assert_eq!(partial.moved(false), Some(0));
        assert_eq!((partial.area, partial.cut, partial.partial), (1, 1, false));
    }

    #[test]
    fn a_search_with_no_area_left_says_why() {
        use super::Search;
        // No scroll area in view and nothing scrolled: not found yet, the server asks again.
        let f = Search { target: "t".into(), ..Default::default() }.exhausted(0);
        assert_eq!(f.code, "notfound");
        assert!(f.msg.contains("no scroll area"), "{}", f.msg);
        // Every area searched: a final answer, saying which were searched only in part.
        let f = Search { target: "t".into(), area: 2, ..Default::default() }.exhausted(2);
        assert_eq!(f.code, "error");
        assert!(f.msg.contains("2 scroll area(s) searched") && !f.msg.contains("in part"), "{}", f.msg);
        let f = Search { target: "t".into(), area: 2, cut: 1, ..Default::default() }.exhausted(1);
        assert_eq!(f.code, "error");
        assert!(f.msg.contains("1 of them only in part"), "{}", f.msg);
    }

    #[test]
    fn an_area_moves_only_when_what_lies_in_it_moves() {
        let mut root = node(0, 3, 0., 0., 800., 600.);
        root.clips = false;
        let mut row = node(2, 2, 0., 120., 300., 30.);
        row.name = "Row 1".into();
        let mut clock = node(3, 3, 400., 10., 100., 20.);
        clock.name = "12:00".into();
        let page = Page { flat: vec![root, node(1, 2, 0., 100., 300., 200.), row, clock], w: 800., h: 600. };
        let area = (0., 100., 300., 300.);
        let before = page.area_signature(area);
        let mut changed = Page { flat: page.flat.clone(), w: 800., h: 600. };
        // Elsewhere in the window, or a row's value, focus or state: the area did not move.
        changed.flat[3].name = "12:01".into();
        changed.flat[2].value = "busy".into();
        changed.flat[2].focused = true;
        changed.flat[2].enabled = false;
        assert_eq!(changed.area_signature(area), before);
        // A row that moved, or another row in its place: it did.
        changed.flat[2].y = 90.;
        assert_ne!(changed.area_signature(area), before);
        changed.flat[2].y = 120.;
        changed.flat[2].name = "Row 9".into();
        assert_ne!(changed.area_signature(area), before);
    }

    #[test]
    fn an_expired_request_does_nothing() {
        let past = std::time::Instant::now() - std::time::Duration::from_millis(1);
        assert!(crate::expired("click", past).is_err());
        let future = std::time::Instant::now() + std::time::Duration::from_secs(5);
        assert!(crate::expired("click", future).is_ok());
    }

    #[test]
    fn a_gpui_scroll_view_clips() {
        // AccessKit hands a `ScrollView` to macOS as `AXUnknown` (trimmed to `Unknown`).
        assert!(super::clips("Unknown"));
        assert!(super::clips("ScrollArea") && super::clips("List") && super::clips("Table") && super::clips("Outline"));
        assert!(!super::clips("Group") && !super::clips("Button") && !super::clips(""));
    }

    #[test]
    fn maps_roles() {
        assert_eq!(aria_role("Button", ""), "button");
        assert_eq!(aria_role("TextArea", ""), "textbox");
        assert_eq!(aria_role("RadioButton", "TabButton"), "tab");
        assert_eq!(aria_role("CheckBox", "Switch"), "switch");
        assert_eq!(aria_role("Group", ""), "generic");
        assert_eq!(aria_role("Group", "ApplicationAlert"), "alert");
        assert_eq!(aria_role("Group", "ApplicationStatus"), "status");
        assert_eq!(aria_role("Window", "Dialog"), "dialog");
        assert_eq!(aria_role("Splitter", ""), "splitter");
    }

    #[test]
    fn collapses_whitespace() {
        assert_eq!(collapse("  a \n b  "), "a b");
    }
}
