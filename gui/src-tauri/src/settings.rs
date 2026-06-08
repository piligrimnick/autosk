//! App settings — the backend transport mode + remote endpoint (plan §6:
//! "mode is an app setting"). Persisted as JSON at `~/.autosk/gui-settings.json`
//! (0600). Local mode needs no extra config; remote mode carries host + token.

use std::path::PathBuf;

use serde::{Deserialize, Serialize};

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum BackendMode {
    Local,
    Remote,
}

impl BackendMode {
    pub fn as_str(&self) -> &'static str {
        match self {
            BackendMode::Local => "local",
            BackendMode::Remote => "remote",
        }
    }
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct AppSettings {
    pub backend_mode: BackendMode,
    #[serde(default)]
    pub remote_host: String,
    #[serde(default)]
    pub remote_token: String,
}

impl Default for AppSettings {
    fn default() -> Self {
        AppSettings {
            backend_mode: BackendMode::Local,
            remote_host: String::new(),
            remote_token: String::new(),
        }
    }
}

fn settings_path() -> Option<PathBuf> {
    std::env::var_os("HOME").map(|h| PathBuf::from(h).join(".autosk").join("gui-settings.json"))
}

/// Loads persisted settings, falling back to defaults on any error.
pub fn load() -> AppSettings {
    let Some(path) = settings_path() else {
        return AppSettings::default();
    };
    match std::fs::read_to_string(&path) {
        Ok(s) => serde_json::from_str(&s).unwrap_or_default(),
        Err(_) => AppSettings::default(),
    }
}

/// Persists settings (0600). Best-effort; returns a human-readable error.
pub fn save(settings: &AppSettings) -> Result<(), String> {
    let path = settings_path().ok_or_else(|| "HOME not set".to_string())?;
    if let Some(dir) = path.parent() {
        std::fs::create_dir_all(dir).map_err(|e| e.to_string())?;
    }
    let json = serde_json::to_string_pretty(settings).map_err(|e| e.to_string())?;
    std::fs::write(&path, json).map_err(|e| e.to_string())?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600));
    }
    Ok(())
}
