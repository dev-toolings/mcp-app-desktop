//! Key combinations in mcp-app-desktop's syntax (`Enter`, `Meta+A`, `Control+Shift+ArrowUp`,
//! `ControlOrMeta+K`, the DOM key names) turned into GPUI keystrokes.

use gpui::{Keystroke, Modifiers};

/// Parses `combo`; the key is GPUI's name for it (`enter`, `up`, `a`, `f5`).
pub fn parse(combo: &str) -> Result<Keystroke, String> {
    let combo = combo.trim();
    if combo.is_empty() {
        return Err("empty key combination".into());
    }
    // `+` alone, or a combination ending in `+` (`Shift++`), presses the plus key.
    let (mods, key) = if combo == "+" {
        ("", "+")
    } else if let Some(prefix) = combo.strip_suffix("++") {
        (prefix, "+")
    } else {
        match combo.rsplit_once('+') {
            Some((mods, key)) => (mods, key),
            None => ("", combo),
        }
    };
    let mut modifiers = Modifiers::default();
    for m in mods.split('+').map(str::trim).filter(|m| !m.is_empty()) {
        match m.to_ascii_lowercase().as_str() {
            "control" | "ctrl" => modifiers.control = true,
            "shift" => modifiers.shift = true,
            "alt" | "option" | "opt" => modifiers.alt = true,
            "meta" | "cmd" | "command" | "super" | "controlormeta" => {
                if m.eq_ignore_ascii_case("controlormeta") && !cfg!(target_os = "macos") {
                    modifiers.control = true;
                } else {
                    modifiers.platform = true;
                }
            }
            "fn" | "function" => modifiers.function = true,
            other => return Err(format!("unknown modifier {other:?} in {combo:?} (Control, Shift, Alt, Meta, ControlOrMeta)")),
        }
    }
    let key = key_name(key.trim()).ok_or_else(|| format!("unknown key {key:?} in {combo:?}"))?;
    Ok(Keystroke { modifiers, key, key_char: None })
}

fn key_name(key: &str) -> Option<String> {
    if key.is_empty() {
        return None;
    }
    let lower = key.to_ascii_lowercase();
    let named = match lower.as_str() {
        "enter" | "return" => "enter",
        "escape" | "esc" => "escape",
        "tab" => "tab",
        "backspace" => "backspace",
        "delete" | "del" => "delete",
        "space" | " " => "space",
        "arrowup" | "up" => "up",
        "arrowdown" | "down" => "down",
        "arrowleft" | "left" => "left",
        "arrowright" | "right" => "right",
        "home" => "home",
        "end" => "end",
        "pageup" => "pageup",
        "pagedown" => "pagedown",
        "insert" => "insert",
        _ => "",
    };
    if !named.is_empty() {
        return Some(named.into());
    }
    if lower.len() > 1 && lower.starts_with('f') && lower[1..].parse::<u8>().is_ok_and(|n| (1..=24).contains(&n)) {
        return Some(lower);
    }
    if lower.starts_with("key") && lower.len() == 4 {
        return Some(lower[3..].into());
    }
    if lower.starts_with("digit") && lower.len() == 6 {
        return Some(lower[5..].into());
    }
    let mut chars = key.chars();
    match (chars.next(), chars.next()) {
        (Some(c), None) => Some(c.to_lowercase().collect()),
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::parse;

    #[test]
    fn parses_combinations() {
        let k = parse("Enter").unwrap();
        assert_eq!(k.key, "enter");
        assert!(!k.modifiers.platform);
        let k = parse("Meta+A").unwrap();
        assert_eq!(k.key, "a");
        assert!(k.modifiers.platform);
        let k = parse("Control+Shift+ArrowUp").unwrap();
        assert_eq!(k.key, "up");
        assert!(k.modifiers.control && k.modifiers.shift);
        assert_eq!(parse("Shift++").unwrap().key, "+");
        assert_eq!(parse("F5").unwrap().key, "f5");
        assert_eq!(parse("KeyK").unwrap().key, "k");
        assert!(parse("Hyper+A").is_err());
        assert!(parse("Nope").is_err());
        assert!(parse("").is_err());
    }
}
