//! Agent-package registry resolution — the Rust port of the read slice of
//! `internal/agent/pkgregistry` the executor needs (`Resolve`, install-dir +
//! bootstrap-path helpers). Package INSTALL (npm) is a CLI write verb
//! (Phase 3); this only reads an already-installed package's config.

use std::path::{Path, PathBuf};

use serde::Deserialize;

/// npm package name of the Node bootstrapper (mirror of `RuntimePackageName`).
pub const RUNTIME_PACKAGE_NAME: &str = "@autosk/agent-runtime";
/// Reserved human-agent name; has no package config (mirror of `HumanAgentName`).
pub const HUMAN_AGENT_NAME: &str = "human";
const SCHEMA_VERSION: i64 = 1;

/// Resolution failures (mirror of `ErrNotInstalled` / `ErrPackageMalformed`).
#[derive(Debug, Clone)]
pub enum PkgError {
    NotInstalled(String),
    Malformed(String),
    Io(String),
}

impl std::fmt::Display for PkgError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            PkgError::NotInstalled(s) => write!(f, "agent_not_installed: {s}"),
            PkgError::Malformed(s) => write!(f, "agent_package_malformed: {s}"),
            PkgError::Io(s) => write!(f, "{s}"),
        }
    }
}

impl std::error::Error for PkgError {}

type PkgResult<T> = std::result::Result<T, PkgError>;

/// Resolved view of an installed agent package (mirror of `PackageConfig`).
/// All paths are absolute; `first_message_file` is inlined into
/// `first_message` at resolve time.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct PackageConfig {
    pub name: String,
    pub version: String,
    pub install_dir: String,
    /// Absolute path to the runner module; empty for standard (pi) agents.
    pub runner: String,
    pub model: String,
    pub thinking: String,
    pub first_message: String,
    pub extra_args: Vec<String>,
    pub pi_extensions: Vec<String>,
    pub pi_skills: Vec<String>,
}

/// A handle on a packages prefix (`~/.autosk/packages` by default).
pub struct Registry {
    prefix: PathBuf,
}

impl Registry {
    /// Opens a registry rooted at `prefix`.
    pub fn open(prefix: impl Into<PathBuf>) -> Registry {
        Registry {
            prefix: prefix.into(),
        }
    }

    /// Resolves the conventional prefix: `$AUTOSK_PACKAGES` →
    /// `$XDG_DATA_HOME/autosk/packages` → `$HOME/.autosk/packages`.
    pub fn default_prefix() -> Option<PathBuf> {
        if let Some(p) = std::env::var_os("AUTOSK_PACKAGES") {
            if !p.is_empty() {
                return Some(PathBuf::from(p));
            }
        }
        if let Some(x) = std::env::var_os("XDG_DATA_HOME") {
            if !x.is_empty() {
                return Some(PathBuf::from(x).join("autosk").join("packages"));
            }
        }
        std::env::var_os("HOME").map(|h| PathBuf::from(h).join(".autosk").join("packages"))
    }

    pub fn prefix(&self) -> &Path {
        &self.prefix
    }

    /// Absolute path of the Node bootstrapper script.
    pub fn runtime_bootstrap_path(&self) -> String {
        self.prefix
            .join("node_modules")
            .join(RUNTIME_PACKAGE_NAME)
            .join("dist")
            .join("bootstrap.js")
            .to_string_lossy()
            .to_string()
    }

    /// Absolute install dir for a package (`<prefix>/node_modules/<name>`).
    pub fn package_install_dir(&self, name: &str) -> PathBuf {
        let rel: PathBuf = name.split('/').collect();
        self.prefix.join("node_modules").join(rel)
    }

    fn registry_path(&self) -> PathBuf {
        self.prefix.join("registry.json")
    }

    /// Returns the registered version for `name`, or [`PkgError::NotInstalled`].
    fn get_version(&self, name: &str) -> PkgResult<String> {
        let path = self.registry_path();
        let bytes = match std::fs::read(&path) {
            Ok(b) => b,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                return Err(PkgError::NotInstalled(name.to_string()))
            }
            Err(e) => return Err(PkgError::Io(format!("read registry.json: {e}"))),
        };
        let f: RegistryFile = serde_json::from_slice(&bytes)
            .map_err(|e| PkgError::Io(format!("parse registry.json: {e}")))?;
        if f.schema_version != 0 && f.schema_version != SCHEMA_VERSION {
            return Err(PkgError::Io(format!(
                "registry.json schema_version={} (this binary expects {SCHEMA_VERSION})",
                f.schema_version
            )));
        }
        f.agents
            .get(name)
            .map(|e| e.version.clone())
            .ok_or_else(|| PkgError::NotInstalled(name.to_string()))
    }

    /// `Resolve` — loads the install-time config for one registered package.
    pub fn resolve(&self, name: &str) -> PkgResult<PackageConfig> {
        if name.is_empty() {
            return Err(PkgError::Io("Resolve: empty name".into()));
        }
        if name == HUMAN_AGENT_NAME {
            return Err(PkgError::NotInstalled(format!(
                "{name} (the human agent has no package config)"
            )));
        }
        let version = self.get_version(name)?;
        let install_dir = self.package_install_dir(name);
        let manifest_path = install_dir.join("package.json");

        let bytes = match std::fs::read(&manifest_path) {
            Ok(b) => b,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                return Err(PkgError::Malformed(format!(
                    "{name} is registered but {} does not exist",
                    manifest_path.display()
                )))
            }
            Err(e) => {
                return Err(PkgError::Io(format!(
                    "read {}: {e}",
                    manifest_path.display()
                )))
            }
        };
        let m: PackageManifest = serde_json::from_slice(&bytes)
            .map_err(|e| PkgError::Malformed(format!("parse {}: {e}", manifest_path.display())))?;
        if m.name != name {
            return Err(PkgError::Malformed(format!(
                "registered name {name:?} does not match package.json name {:?}",
                m.name
            )));
        }
        let am = m
            .autosk
            .and_then(|a| a.agent)
            .ok_or_else(|| PkgError::Malformed(format!("{name} missing \"autosk.agent\" block")))?;
        if !thinking_valid(&am.thinking) {
            return Err(PkgError::Malformed(format!(
                "{name} thinking={:?} (want one of off|minimal|low|medium|high|xhigh)",
                am.thinking
            )));
        }
        if !am.first_message.is_empty() && !am.first_message_file.is_empty() {
            return Err(PkgError::Malformed(format!(
                "{name} declares both first_message and first_message_file (pick one)"
            )));
        }

        let install_dir_s = install_dir.to_string_lossy().to_string();
        let mut cfg = PackageConfig {
            name: name.to_string(),
            version,
            install_dir: install_dir_s.clone(),
            model: am.model.clone(),
            thinking: am.thinking.clone(),
            first_message: am.first_message.clone(),
            extra_args: am.extra_args.clone(),
            ..Default::default()
        };

        if !am.runner.is_empty() {
            let abs = resolve_inside_pkg(&install_dir, &am.runner)
                .map_err(|e| PkgError::Malformed(format!("{name} runner {:?}: {e}", am.runner)))?;
            if !Path::new(&abs).exists() {
                return Err(PkgError::Malformed(format!("{name} runner missing: {abs}")));
            }
            cfg.runner = abs;
        }
        if !am.first_message_file.is_empty() {
            let abs = resolve_inside_pkg(&install_dir, &am.first_message_file).map_err(|e| {
                PkgError::Malformed(format!(
                    "{name} first_message_file {:?}: {e}",
                    am.first_message_file
                ))
            })?;
            let body = std::fs::read_to_string(&abs).map_err(|e| {
                PkgError::Malformed(format!("{name} read first_message_file {abs}: {e}"))
            })?;
            cfg.first_message = body;
        }
        for ext in &am.pi_extensions {
            let abs = resolve_inside_pkg(&install_dir, ext)
                .map_err(|e| PkgError::Malformed(format!("{name} pi_extensions[{ext:?}]: {e}")))?;
            if !Path::new(&abs).exists() {
                return Err(PkgError::Malformed(format!(
                    "{name} pi_extension missing: {abs}"
                )));
            }
            cfg.pi_extensions.push(abs);
        }
        for sk in &am.pi_skills {
            let abs = resolve_inside_pkg(&install_dir, sk)
                .map_err(|e| PkgError::Malformed(format!("{name} pi_skills[{sk:?}]: {e}")))?;
            if !Path::new(&abs).exists() {
                return Err(PkgError::Malformed(format!(
                    "{name} pi_skill missing: {abs}"
                )));
            }
            cfg.pi_skills.push(abs);
        }
        Ok(cfg)
    }
}

#[derive(Deserialize)]
struct RegistryFile {
    #[serde(default)]
    schema_version: i64,
    #[serde(default)]
    agents: std::collections::HashMap<String, RegistryEntry>,
}

#[derive(Deserialize)]
struct RegistryEntry {
    #[serde(default)]
    version: String,
}

#[derive(Deserialize)]
struct PackageManifest {
    #[serde(default)]
    name: String,
    #[serde(default)]
    autosk: Option<AutoskManifest>,
}

#[derive(Deserialize)]
struct AutoskManifest {
    #[serde(default)]
    agent: Option<AgentManifest>,
}

#[derive(Deserialize, Default)]
struct AgentManifest {
    #[serde(default)]
    runner: String,
    #[serde(default)]
    model: String,
    #[serde(default)]
    thinking: String,
    #[serde(default)]
    first_message: String,
    #[serde(default)]
    first_message_file: String,
    #[serde(default)]
    extra_args: Vec<String>,
    #[serde(default)]
    pi_extensions: Vec<String>,
    #[serde(default)]
    pi_skills: Vec<String>,
}

fn thinking_valid(t: &str) -> bool {
    matches!(
        t,
        "" | "off" | "minimal" | "low" | "medium" | "high" | "xhigh"
    )
}

/// Joins `rel` to `install_dir`, rejecting any result that escapes the package
/// directory (mirror of `resolveInsidePkg`).
fn resolve_inside_pkg(install_dir: &Path, rel: &str) -> std::result::Result<String, String> {
    let rel = rel.trim_start_matches("./").trim_start_matches('/');
    if Path::new(rel).is_absolute() {
        return Err(format!("absolute paths are not allowed: {rel}"));
    }
    let abs = install_dir.join(rel);
    let clean = abs.canonicalize().unwrap_or_else(|_| lexical_clean(&abs));
    let root = install_dir
        .canonicalize()
        .unwrap_or_else(|_| lexical_clean(install_dir));
    if !clean.starts_with(&root) {
        return Err(format!("path escapes package: {rel}"));
    }
    Ok(clean.to_string_lossy().to_string())
}

fn lexical_clean(p: &Path) -> PathBuf {
    let mut out = PathBuf::new();
    for c in p.components() {
        use std::path::Component::*;
        match c {
            ParentDir => {
                out.pop();
            }
            CurDir => {}
            other => out.push(other.as_os_str()),
        }
    }
    out
}

/// Test-support helper: lays down a stub package on disk (`node_modules/<name>/
/// package.json` + a `registry.json` entry) so [`Registry::resolve`] finds it
/// without an npm install. Mirrors what the Go executor tests do by hand. Kept
/// always-compiled (not `cfg(test)`) so in-tree integration tests — which see
/// the crate as an external dependency — can call it; it is inert in production.
pub fn install_stub(
    prefix: &Path,
    name: &str,
    version: &str,
    agent_block: serde_json::Value,
) -> std::io::Result<()> {
    use serde_json::json;
    let rel: PathBuf = name.split('/').collect();
    let dir = prefix.join("node_modules").join(rel);
    std::fs::create_dir_all(&dir)?;
    let pj = json!({"name": name, "version": version, "autosk": {"agent": agent_block}});
    std::fs::write(
        dir.join("package.json"),
        serde_json::to_vec_pretty(&pj).unwrap(),
    )?;
    // Merge into registry.json.
    let reg_path = prefix.join("registry.json");
    let mut agents = serde_json::Map::new();
    if let Ok(b) = std::fs::read(&reg_path) {
        if let Ok(v) = serde_json::from_slice::<serde_json::Value>(&b) {
            if let Some(a) = v.get("agents").and_then(|a| a.as_object()) {
                agents = a.clone();
            }
        }
    }
    agents.insert(
        name.to_string(),
        json!({"version": version, "installed_at": "2026-01-01T00:00:00Z"}),
    );
    let reg = json!({"schema_version": SCHEMA_VERSION, "agents": agents});
    std::fs::create_dir_all(prefix)?;
    std::fs::write(&reg_path, serde_json::to_vec_pretty(&reg).unwrap())?;
    Ok(())
}
