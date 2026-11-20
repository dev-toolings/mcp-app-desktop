//! Standard base64 encoding (RFC 4648, with padding), enough for screenshots.

const ALPHABET: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

pub fn encode(input: &[u8]) -> String {
  let mut out = String::with_capacity(input.len().div_ceil(3) * 4);
  for chunk in input.chunks(3) {
    let b = [chunk[0], *chunk.get(1).unwrap_or(&0), *chunk.get(2).unwrap_or(&0)];
    let n = (u32::from(b[0]) << 16) | (u32::from(b[1]) << 8) | u32::from(b[2]);
    out.push(ALPHABET[(n >> 18) as usize & 63] as char);
    out.push(ALPHABET[(n >> 12) as usize & 63] as char);
    out.push(if chunk.len() > 1 { ALPHABET[(n >> 6) as usize & 63] as char } else { '=' });
    out.push(if chunk.len() > 2 { ALPHABET[n as usize & 63] as char } else { '=' });
  }
  out
}

#[cfg(test)]
mod tests {
  use super::encode;

  #[test]
  fn rfc4648_vectors() {
    let cases = [
      ("", ""),
      ("f", "Zg=="),
      ("fo", "Zm8="),
      ("foo", "Zm9v"),
      ("foob", "Zm9vYg=="),
      ("fooba", "Zm9vYmE="),
      ("foobar", "Zm9vYmFy"),
    ];
    for (raw, want) in cases {
      assert_eq!(encode(raw.as_bytes()), want, "encode({raw:?})");
    }
    assert_eq!(encode(&[0xff, 0xfe, 0xfd]), "//79");
  }
}
