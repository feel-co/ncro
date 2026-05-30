use std::io::{BufRead, BufReader, Read};

use base64::{Engine, engine::general_purpose::STANDARD};
use ed25519_dalek::{Signature, Verifier, VerifyingKey};
use thiserror::Error;

#[derive(Debug, Error)]
pub enum NarInfoError {
  #[error("read narinfo: {0}")]
  Io(#[from] std::io::Error),
  #[error("malformed line: {0:?}")]
  MalformedLine(String),
  #[error("missing StorePath")]
  MissingStorePath,
  #[error("{field}: {source}")]
  ParseInt {
    field:  &'static str,
    source: std::num::ParseIntError,
  },
  #[error("invalid public key {input:?}: missing ':'")]
  MissingPublicKeySeparator { input: String },
  #[error("invalid public key {input:?}: {source}")]
  InvalidPublicKeyBase64 {
    input:  String,
    source: base64::DecodeError,
  },
  #[error("invalid public key size {got}, want 32")]
  InvalidPublicKeySize { got: usize },
}

#[cfg(test)]
mod tests {
  use ed25519_dalek::{Signer, SigningKey};
  use rand::RngExt;

  use super::*;

  #[test]
  fn parses_realistic_narinfo() -> Result<(), NarInfoError> {
    let input = "StorePath: /nix/store/abc-hello\nURL: \
                 nar/abc.nar.xz\nCompression: xz\nFileSize: 42\nNarHash: \
                 sha256:abc\nNarSize: 123\nReferences: abc-hello dep\nSig: \
                 key:sig=\n";
    let ni = NarInfo::parse(input.as_bytes())?;
    assert_eq!(ni.store_path, "/nix/store/abc-hello");
    assert_eq!(ni.url, "nar/abc.nar.xz");
    assert_eq!(ni.references.len(), 2);
    Ok(())
  }

  #[test]
  fn missing_store_path_returns_error() {
    let input = "URL: nar/abc.nar.xz\nNarHash: sha256:abc\nNarSize: 1\n";
    assert!(matches!(
      NarInfo::parse(input.as_bytes()),
      Err(NarInfoError::MissingStorePath)
    ));
  }

  #[test]
  fn malformed_line_returns_error() {
    let input = "StorePath: /nix/store/abc\nno-colon-here\n";
    assert!(matches!(
      NarInfo::parse(input.as_bytes()),
      Err(NarInfoError::MalformedLine(_))
    ));
  }

  #[test]
  fn parse_public_key_error_paths() {
    assert!(matches!(
      parse_public_key("no-separator"),
      Err(NarInfoError::MissingPublicKeySeparator { .. })
    ));
    // Empty name before colon
    assert!(matches!(
      parse_public_key(":dGVzdA=="),
      Err(NarInfoError::MissingPublicKeySeparator { .. })
    ));
    // Valid name but invalid base64
    assert!(matches!(
      parse_public_key("test:!!!"),
      Err(NarInfoError::InvalidPublicKeyBase64 { .. })
    ));
    // Valid base64 but wrong length (not 32 bytes)
    assert!(matches!(
      parse_public_key("test:dGVzdA=="),
      Err(NarInfoError::InvalidPublicKeySize { .. })
    ));
  }

  #[test]
  fn verifies_roundtrip_signature() -> Result<(), NarInfoError> {
    let mut key_bytes = [0_u8; 32];
    rand::rng().fill(&mut key_bytes);
    let signing = SigningKey::from_bytes(&key_bytes);
    let mut ni = NarInfo {
      store_path: "/nix/store/abc-test".into(),
      nar_hash: "sha256:abc".into(),
      nar_size: 12,
      references: vec!["abc-test".into()],
      ..Default::default()
    };
    let sig = signing.sign(ni.fingerprint().as_bytes());
    let pubkey = format!(
      "test:{}",
      STANDARD.encode(signing.verifying_key().to_bytes())
    );
    ni.sig = vec![format!("test:{}", STANDARD.encode(sig.to_bytes()))];
    assert!(ni.verify(&pubkey)?);
    Ok(())
  }

  #[test]
  fn verify_with_wrong_key_returns_false() -> Result<(), NarInfoError> {
    let mut key1_bytes = [0u8; 32];
    let mut key2_bytes = [1u8; 32];
    rand::rng().fill(&mut key1_bytes);
    rand::rng().fill(&mut key2_bytes);
    let signing1 = SigningKey::from_bytes(&key1_bytes);
    let signing2 = SigningKey::from_bytes(&key2_bytes);
    let mut ni = NarInfo {
      store_path: "/nix/store/abc-test".into(),
      nar_hash: "sha256:abc".into(),
      nar_size: 12,
      ..Default::default()
    };
    let sig = signing1.sign(ni.fingerprint().as_bytes());
    ni.sig = vec![format!("test:{}", STANDARD.encode(sig.to_bytes()))];
    let wrong_pubkey = format!(
      "test:{}",
      STANDARD.encode(signing2.verifying_key().to_bytes())
    );
    assert!(!ni.verify(&wrong_pubkey)?);
    Ok(())
  }

  #[test]
  fn verify_tampered_content_returns_false() -> Result<(), NarInfoError> {
    let mut key_bytes = [0u8; 32];
    rand::rng().fill(&mut key_bytes);
    let signing = SigningKey::from_bytes(&key_bytes);
    let mut ni = NarInfo {
      store_path: "/nix/store/abc-test".into(),
      nar_hash: "sha256:abc".into(),
      nar_size: 12,
      ..Default::default()
    };
    let sig = signing.sign(ni.fingerprint().as_bytes());
    let pubkey = format!(
      "test:{}",
      STANDARD.encode(signing.verifying_key().to_bytes())
    );
    ni.sig = vec![format!("test:{}", STANDARD.encode(sig.to_bytes()))];
    ni.nar_size = 999; // tamper after signing
    assert!(!ni.verify(&pubkey)?);
    Ok(())
  }
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct NarInfo {
  pub store_path:  String,
  pub url:         String,
  pub compression: String,
  pub file_hash:   String,
  pub file_size:   u64,
  pub nar_hash:    String,
  pub nar_size:    u64,
  pub references:  Vec<String>,
  pub deriver:     String,
  pub sig:         Vec<String>,
  pub ca:          String,
}

/// # Errors
///
/// Returns [`NarInfoError`] if the input lacks a `name:base64` separator,
/// the name is empty, the base64 is invalid, or the key is not 32 bytes.
pub fn parse_public_key(
  input: &str,
) -> Result<(String, VerifyingKey), NarInfoError> {
  let (name, b64) = input.split_once(':').ok_or_else(|| {
    NarInfoError::MissingPublicKeySeparator {
      input: input.to_string(),
    }
  })?;
  if name.is_empty() {
    return Err(NarInfoError::MissingPublicKeySeparator {
      input: input.to_string(),
    });
  }
  let raw = STANDARD.decode(b64).map_err(|source| {
    NarInfoError::InvalidPublicKeyBase64 {
      input: input.to_string(),
      source,
    }
  })?;
  let bytes: [u8; 32] = raw.try_into().map_err(|raw: Vec<u8>| {
    NarInfoError::InvalidPublicKeySize { got: raw.len() }
  })?;
  let key = VerifyingKey::from_bytes(&bytes)
    .map_err(|_| NarInfoError::InvalidPublicKeySize { got: bytes.len() })?;
  Ok((name.to_string(), key))
}

impl NarInfo {
  /// # Errors
  ///
  /// Returns [`NarInfoError`] if a line is malformed, an integer field cannot
  /// be parsed, an I/O error occurs, or `StorePath` is missing.
  pub fn parse(reader: impl Read) -> Result<Self, NarInfoError> {
    let mut narinfo = Self::default();
    for line in BufReader::new(reader).lines() {
      let line = line?;
      if line.is_empty() {
        continue;
      }
      let (key, value) = line
        .split_once(": ")
        .ok_or_else(|| NarInfoError::MalformedLine(line.clone()))?;
      match key {
        "StorePath" => narinfo.store_path = value.to_string(),
        "URL" => narinfo.url = value.to_string(),
        "Compression" => narinfo.compression = value.to_string(),
        "FileHash" => narinfo.file_hash = value.to_string(),
        "FileSize" => {
          narinfo.file_size = value.parse().map_err(|source| {
            NarInfoError::ParseInt {
              field: "FileSize",
              source,
            }
          })?;
        },
        "NarHash" => narinfo.nar_hash = value.to_string(),
        "NarSize" => {
          narinfo.nar_size = value.parse().map_err(|source| {
            NarInfoError::ParseInt {
              field: "NarSize",
              source,
            }
          })?;
        },
        "References"
          if !value.is_empty() => {
            narinfo.references =
              value.split_whitespace().map(str::to_string).collect();
          },
        "Deriver" => narinfo.deriver = value.to_string(),
        "Sig" => narinfo.sig.push(value.to_string()),
        "CA" => narinfo.ca = value.to_string(),
        _ => {},
      }
    }
    if narinfo.store_path.is_empty() {
      return Err(NarInfoError::MissingStorePath);
    }
    Ok(narinfo)
  }

  #[must_use]
  pub fn fingerprint(&self) -> String {
    let refs = self
      .references
      .iter()
      .map(|reference| {
        if reference.starts_with("/nix/store/") {
          reference.clone()
        } else {
          format!("/nix/store/{reference}")
        }
      })
      .collect::<Vec<_>>()
      .join(",");
    format!(
      "1;{};{};{};{}",
      self.store_path, self.nar_hash, self.nar_size, refs
    )
  }

  /// # Errors
  ///
  /// Returns [`NarInfoError`] if `public_key` cannot be parsed (see
  /// [`parse_public_key`]).
  pub fn verify(&self, public_key: &str) -> Result<bool, NarInfoError> {
    let (key_name, key) = parse_public_key(public_key)?;
    let fingerprint = self.fingerprint();
    for sig_line in &self.sig {
      let Some((name, b64)) = sig_line.split_once(':') else {
        continue;
      };
      if name != key_name {
        continue;
      }
      let Ok(raw) = STANDARD.decode(b64) else {
        continue;
      };
      let Ok(bytes) = <[u8; 64]>::try_from(raw.as_slice()) else {
        continue;
      };
      let signature = Signature::from_bytes(&bytes);
      if key.verify(fingerprint.as_bytes(), &signature).is_ok() {
        return Ok(true);
      }
    }
    Ok(false)
  }
}
