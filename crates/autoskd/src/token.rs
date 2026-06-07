//! TCP auth token (plan §4.1): `~/.autosk/daemon-token`, mode `0600`.
//!
//! The token gates the TCP transport only — UDS connections are exempt (the
//! socket is `0600` in a `0700` dir, so filesystem perms already authenticate
//! the local peer). On first `serve` the daemon reads the token, minting a
//! fresh random one if the file is absent.

use std::io::{Read, Write};
use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
use std::path::PathBuf;

/// Resolves the token path: `$AUTOSK_TOKEN_FILE` → `~/.autosk/daemon-token`.
pub fn token_path() -> Option<PathBuf> {
    if let Some(p) = std::env::var_os("AUTOSK_TOKEN_FILE") {
        if !p.is_empty() {
            return Some(PathBuf::from(p));
        }
    }
    std::env::var_os("HOME").map(|h| PathBuf::from(h).join(".autosk").join("daemon-token"))
}

/// Reads the token, minting + persisting a fresh one (`0600`) if absent.
pub fn ensure_token() -> std::io::Result<String> {
    let path = token_path()
        .ok_or_else(|| std::io::Error::new(std::io::ErrorKind::NotFound, "HOME not set"))?;
    if let Ok(s) = std::fs::read_to_string(&path) {
        let t = s.trim().to_string();
        if !t.is_empty() {
            return Ok(t);
        }
    }
    let token = mint()?;
    if let Some(dir) = path.parent() {
        std::fs::create_dir_all(dir)?;
        let _ = std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700));
    }
    // Create the file 0600 from the start (mode applies on creation) so the
    // secret never exists with the umask's looser perms, even briefly. Then
    // re-tighten in case the file pre-existed (empty) with wider perms.
    let mut f = std::fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o600)
        .open(&path)?;
    f.write_all(format!("{token}\n").as_bytes())?;
    f.flush()?;
    std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600))?;
    Ok(token)
}

/// 32 random bytes from `/dev/urandom`, hex-encoded. Fallible: a failed open
/// OR a short read is an error — never accept partial entropy (a zero/constant
/// token would be a usable TCP credential for an attacker who induced the
/// failure).
fn mint() -> std::io::Result<String> {
    let mut buf = [0u8; 32];
    let mut f = std::fs::File::open("/dev/urandom")?;
    f.read_exact(&mut buf)?;
    Ok(buf.iter().map(|b| format!("{b:02x}")).collect())
}
