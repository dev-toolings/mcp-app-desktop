//! The window's accessibility tree, read in-process.
//!
//! GPUI builds an AccessKit tree only while assistive technology is active. The bridge activates
//! it the way VoiceOver would, by asking the window's view for its accessibility children (which
//! needs no permission from inside the process), then reads the nodes AccessKit's macOS adapter
//! exposes: role, title, label, value, focus, enabled state and frame. Frames are converted from
//! screen coordinates to the window's logical pixels, the coordinates GPUI input events use.

use gpui::Window;

/// One accessibility node, its frame in window coordinates (logical pixels, top-left origin).
#[derive(Clone, Debug, Default)]
pub struct AxNode {
    /// The macOS role without its `AX` prefix (`Button`, `TextArea`, `StaticText`…).
    pub ax_role: String,
    pub ax_subrole: String,
    pub title: String,
    pub label: String,
    pub value: String,
    pub placeholder: String,
    pub focused: bool,
    pub enabled: bool,
    pub selected: Option<bool>,
    pub x: f64,
    pub y: f64,
    pub w: f64,
    pub h: f64,
    pub children: Vec<AxNode>,
}

/// Upper bounds of one read: a runaway tree never stalls the main thread.
const MAX_NODES: usize = 20_000;
const MAX_DEPTH: usize = 256;

/// Starts AccessKit for `window` when it is not running. Returns true when the tree is already
/// built; false when it was just activated and the next frame will build it.
pub fn ensure_active(window: &mut Window) -> Result<bool, String> {
    if window.is_a11y_active() {
        return Ok(true);
    }
    if !window.is_a11y_enabled() {
        return Err("accessibility is disabled in this app (GPUI's accessibility_force_disabled): the bridge reads the window through it".into());
    }
    imp::activate(window)?;
    window.refresh();
    Ok(false)
}

/// Reads the tree. `Ok(None)` when the window has no accessibility children yet.
pub fn read(window: &Window) -> Result<Vec<AxNode>, String> {
    imp::read(window)
}

#[cfg(target_os = "macos")]
mod imp {
    use std::ffi::{c_char, c_void, CStr};
    use std::ptr::NonNull;

    use gpui::Window;
    use objc2::rc::Retained;
    use objc2::runtime::{AnyObject, MessageReceiver, Sel};
    use objc2::{msg_send, sel, Encode, Encoding, RefEncode};
    use raw_window_handle::{HasWindowHandle, RawWindowHandle};

    use super::{AxNode, MAX_DEPTH, MAX_NODES};

    #[repr(C)]
    #[derive(Clone, Copy, Debug, Default)]
    struct Point {
        x: f64,
        y: f64,
    }
    #[repr(C)]
    #[derive(Clone, Copy, Debug, Default)]
    struct Size {
        w: f64,
        h: f64,
    }
    #[repr(C)]
    #[derive(Clone, Copy, Debug, Default)]
    struct Rect {
        origin: Point,
        size: Size,
    }
    unsafe impl Encode for Point {
        const ENCODING: Encoding = Encoding::Struct("CGPoint", &[f64::ENCODING, f64::ENCODING]);
    }
    unsafe impl RefEncode for Point {
        const ENCODING_REF: Encoding = Encoding::Pointer(&Self::ENCODING);
    }
    unsafe impl Encode for Size {
        const ENCODING: Encoding = Encoding::Struct("CGSize", &[f64::ENCODING, f64::ENCODING]);
    }
    unsafe impl RefEncode for Size {
        const ENCODING_REF: Encoding = Encoding::Pointer(&Self::ENCODING);
    }
    unsafe impl Encode for Rect {
        const ENCODING: Encoding = Encoding::Struct("CGRect", &[Point::ENCODING, Size::ENCODING]);
    }
    unsafe impl RefEncode for Rect {
        const ENCODING_REF: Encoding = Encoding::Pointer(&Self::ENCODING);
    }

    /// The window's content view: AccessKit's adapter subclasses that one
    /// (`SubclassingAdapter::for_window`), which may not be GPUI's own drawing view.
    fn content_view(window: &Window) -> Result<Retained<AnyObject>, String> {
        let handle = HasWindowHandle::window_handle(window).map_err(|e| format!("no native window handle: {e}"))?;
        let view: NonNull<c_void> = match handle.as_raw() {
            RawWindowHandle::AppKit(h) => h.ns_view,
            other => return Err(format!("unexpected native window handle {other:?}")),
        };
        let view: &AnyObject = unsafe { view.cast::<AnyObject>().as_ref() };
        let ns_window: Option<Retained<AnyObject>> = unsafe { msg_send![view, window] };
        let ns_window = ns_window.ok_or("the GPUI view is not in a window")?;
        let content: Option<Retained<AnyObject>> = unsafe { msg_send![&*ns_window, contentView] };
        content.ok_or_else(|| "the window has no content view".to_owned())
    }

    fn responds(obj: &AnyObject, sel: Sel) -> bool {
        unsafe { msg_send![obj, respondsToSelector: sel] }
    }

    fn string_of(obj: Option<Retained<AnyObject>>) -> String {
        let Some(obj) = obj else { return String::new() };
        // NSString answers its own text; NSNumber and the rest their description.
        let description: Option<Retained<AnyObject>> = unsafe { msg_send![&*obj, description] };
        let Some(description) = description else { return String::new() };
        let utf8: *const c_char = unsafe { msg_send![&*description, UTF8String] };
        if utf8.is_null() {
            return String::new();
        }
        unsafe { CStr::from_ptr(utf8) }.to_string_lossy().into_owned()
    }

    /// `[obj sel]` for a getter answering an object (+0, kept alive here by a retain).
    fn send_object(obj: &AnyObject, sel: Sel) -> Option<Retained<AnyObject>> {
        let ptr: *mut AnyObject = unsafe { obj.send_message(sel, ()) };
        unsafe { Retained::retain(ptr) }
    }

    fn get_string(obj: &AnyObject, sel: Sel) -> String {
        if !responds(obj, sel) {
            return String::new();
        }
        string_of(send_object(obj, sel))
    }

    fn children_of(obj: &AnyObject) -> Vec<Retained<AnyObject>> {
        if !responds(obj, sel!(accessibilityChildren)) {
            return Vec::new();
        }
        let array: Option<Retained<AnyObject>> = unsafe { msg_send![obj, accessibilityChildren] };
        let Some(array) = array else { return Vec::new() };
        let count: usize = unsafe { msg_send![&*array, count] };
        (0..count)
            .filter_map(|i| {
                let item: Option<Retained<AnyObject>> = unsafe { msg_send![&*array, objectAtIndex: i] };
                item
            })
            .collect()
    }

    pub fn activate(window: &Window) -> Result<(), String> {
        let view = content_view(window)?;
        let view: &AnyObject = &view;
        // AccessKit's adapter initialises (and GPUI marks accessibility active) the first time
        // anything asks the view for its children.
        let _ = children_of(view);
        Ok(())
    }

    struct Frame<'a> {
        view: &'a AnyObject,
        window: Retained<AnyObject>,
        flipped: bool,
        height: f64,
    }

    impl Frame<'_> {
        /// Screen rectangle (bottom-left origin, points) to window logical pixels.
        fn to_window(&self, r: Rect) -> (f64, f64, f64, f64) {
            let in_window: Rect = unsafe { msg_send![&*self.window, convertRectFromScreen: r] };
            let none: Option<&AnyObject> = None;
            let local: Rect = unsafe { msg_send![self.view, convertRect: in_window, fromView: none] };
            let y = if self.flipped { local.origin.y } else { self.height - local.origin.y - local.size.h };
            (local.origin.x, y, local.size.w, local.size.h)
        }
    }

    pub fn read(window: &Window) -> Result<Vec<AxNode>, String> {
        let view = content_view(window)?;
        let view: &AnyObject = &view;
        let ns_window: Option<Retained<AnyObject>> = unsafe { msg_send![view, window] };
        let ns_window = ns_window.ok_or("the view has no window")?;
        let flipped: bool = unsafe { msg_send![view, isFlipped] };
        let bounds: Rect = unsafe { msg_send![view, bounds] };
        let frame = Frame { view, window: ns_window, flipped, height: bounds.size.h };
        let mut budget = MAX_NODES;
        let mut out = Vec::new();
        for child in children_of(view) {
            if let Some(node) = walk(&child, &frame, 0, &mut budget) {
                out.push(node);
            }
        }
        Ok(out)
    }

    fn walk(obj: &AnyObject, frame: &Frame, depth: usize, budget: &mut usize) -> Option<AxNode> {
        if *budget == 0 || depth > MAX_DEPTH {
            return None;
        }
        *budget -= 1;
        let rect: Rect = if responds(obj, sel!(accessibilityFrame)) {
            unsafe { msg_send![obj, accessibilityFrame] }
        } else {
            Rect::default()
        };
        let (x, y, w, h) = frame.to_window(rect);
        let mut node = AxNode {
            ax_role: get_string(obj, sel!(accessibilityRole)).trim_start_matches("AX").to_owned(),
            ax_subrole: get_string(obj, sel!(accessibilitySubrole)).trim_start_matches("AX").to_owned(),
            title: get_string(obj, sel!(accessibilityTitle)),
            label: get_string(obj, sel!(accessibilityLabel)),
            value: get_string(obj, sel!(accessibilityValue)),
            placeholder: get_string(obj, sel!(accessibilityPlaceholderValue)),
            focused: bool_of(obj, sel!(isAccessibilityFocused)).unwrap_or(false),
            enabled: bool_of(obj, sel!(isAccessibilityEnabled)).unwrap_or(true),
            selected: bool_of(obj, sel!(isAccessibilitySelected)),
            x,
            y,
            w,
            h,
            children: Vec::new(),
        };
        for child in children_of(obj) {
            if let Some(c) = walk(&child, frame, depth + 1, budget) {
                node.children.push(c);
            }
        }
        Some(node)
    }

    fn bool_of(obj: &AnyObject, sel: Sel) -> Option<bool> {
        if !responds(obj, sel) {
            return None;
        }
        let value: objc2::runtime::Bool = unsafe { obj.send_message(sel, ()) };
        Some(value.as_bool())
    }
}

#[cfg(not(target_os = "macos"))]
mod imp {
    use super::AxNode;
    use gpui::Window;

    pub fn activate(_window: &Window) -> Result<(), String> {
        Err(format!("the GPUI bridge reads windows on macOS only (not {})", std::env::consts::OS))
    }

    pub fn read(_window: &Window) -> Result<Vec<AxNode>, String> {
        activate(_window).map(|_| Vec::new())
    }
}
