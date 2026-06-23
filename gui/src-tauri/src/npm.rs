//! npm.rs — the `extension_search` Tauri command. Queries the public npm
//! registry search endpoint for packages tagged `autosk-extension` and
//! normalises the result into a flat, weekly-downloads-sorted `NpmExtension`
//! list the GUI renders. The search ALWAYS runs on the machine hosting the GUI
//! (this Rust side), even in remote-daemon mode; the install half goes through
//! the daemon RPC (`extension.install`) separately. No autoskd round-trip here.

use serde::{Deserialize, Serialize};

/// The public npm registry search endpoint, scoped to the `autosk-extension`
/// keyword. `size=250` over-fetches (the index has far fewer today) so a single
/// request returns everything without pagination.
const SEARCH_URL: &str =
    "https://registry.npmjs.org/-/v1/search?text=keywords:autosk-extension&size=250";

/// A normalised npm extension package surfaced to the GUI. Field names are
/// snake_case to match the wire convention the rest of the app uses (the
/// frontend `NpmExtension` type mirrors this struct verbatim). `updated` is the
/// raw RFC3339 UTC string; the frontend renders it in local time.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct NpmExtension {
    pub name: String,
    pub version: String,
    pub description: String,
    pub publisher: String,
    pub weekly_downloads: u64,
    pub updated: String,
    pub npm_url: String,
}

// ---- raw npm search response (only the fields we read) --------------------

#[derive(Debug, Deserialize)]
struct SearchResponse {
    #[serde(default)]
    objects: Vec<SearchObject>,
}

#[derive(Debug, Deserialize)]
struct SearchObject {
    package: PackageInfo,
    #[serde(default)]
    downloads: Option<Downloads>,
    #[serde(default)]
    updated: Option<String>,
}

#[derive(Debug, Deserialize)]
struct PackageInfo {
    name: String,
    #[serde(default)]
    version: String,
    #[serde(default)]
    description: String,
    #[serde(default)]
    date: Option<String>,
    #[serde(default)]
    publisher: Option<Publisher>,
    #[serde(default)]
    links: Option<Links>,
}

#[derive(Debug, Deserialize)]
struct Publisher {
    #[serde(default)]
    username: String,
}

#[derive(Debug, Deserialize)]
struct Links {
    #[serde(default)]
    npm: Option<String>,
}

#[derive(Debug, Deserialize)]
struct Downloads {
    #[serde(default)]
    weekly: u64,
}

/// Normalise + sort the npm search response by weekly downloads (descending).
/// Split out from the command so it is unit-testable without a network round
/// trip (see the fixture-backed test below).
fn normalize(resp: SearchResponse) -> Vec<NpmExtension> {
    let mut out: Vec<NpmExtension> = resp
        .objects
        .into_iter()
        .map(|o| {
            let pkg = o.package;
            let name = pkg.name;
            let weekly = o.downloads.map(|d| d.weekly).unwrap_or(0);
            // Prefer the object-level `updated`; fall back to the package
            // publish `date`; finally empty (the frontend treats "" as unknown).
            let updated = o.updated.or(pkg.date).unwrap_or_default();
            let npm_url = pkg
                .links
                .and_then(|l| l.npm)
                .unwrap_or_else(|| format!("https://www.npmjs.com/package/{name}"));
            NpmExtension {
                name,
                version: pkg.version,
                description: pkg.description,
                publisher: pkg.publisher.map(|p| p.username).unwrap_or_default(),
                weekly_downloads: weekly,
                updated,
                npm_url,
            }
        })
        .collect();
    // Sort by weekly downloads desc; tie-break on name for a stable order.
    out.sort_by(|a, b| {
        b.weekly_downloads
            .cmp(&a.weekly_downloads)
            .then_with(|| a.name.cmp(&b.name))
    });
    out
}

/// Search the npm registry for `autosk-extension` packages. Returns a list
/// sorted by weekly downloads (descending). Network or parse failures surface
/// as an `Err(String)` the browse modal renders.
#[tauri::command]
pub async fn extension_search() -> Result<Vec<NpmExtension>, String> {
    let client = reqwest::Client::builder()
        .user_agent(concat!("autosk-gui/", env!("CARGO_PKG_VERSION")))
        .build()
        .map_err(|e| format!("failed to build http client: {e}"))?;
    let resp = client
        .get(SEARCH_URL)
        .send()
        .await
        .map_err(|e| format!("npm search request failed: {e}"))?;
    if !resp.status().is_success() {
        return Err(format!("npm search returned HTTP {}", resp.status()));
    }
    let parsed: SearchResponse = resp
        .json()
        .await
        .map_err(|e| format!("failed to parse npm search response: {e}"))?;
    Ok(normalize(parsed))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_and_sorts_by_weekly_downloads() {
        let fixture = include_str!("../tests/fixtures/npm-search.json");
        let resp: SearchResponse = serde_json::from_str(fixture).expect("fixture parses");
        let out = normalize(resp);

        // Sorted desc by weekly downloads. `tie-zebra` (90) is listed BEFORE
        // `tie-apple` (90) in the fixture, so the name tie-break is what flips
        // them into ascending-name order here (sort_by is stable, so without
        // the `.then_with(name)` branch they would stay zebra-then-apple).
        let order: Vec<&str> = out.iter().map(|e| e.name.as_str()).collect();
        assert_eq!(
            order,
            vec![
                "@autosk/feature-dev",
                "high-rollers",
                "@autosk/worktree",
                "tie-apple",
                "tie-zebra",
                "no-downloads-ext",
            ],
        );

        // The equal-weekly pair: same count, ascending-name order (tie-break).
        assert_eq!(out[3].weekly_downloads, 90);
        assert_eq!(out[4].weekly_downloads, 90);
        assert_eq!(out[3].name, "tie-apple");
        assert_eq!(out[4].name, "tie-zebra");

        // Weekly downloads parsed; the package missing a `downloads` block → 0.
        assert_eq!(out[0].weekly_downloads, 200);
        assert_eq!(out[5].weekly_downloads, 0);

        // Scoped name + version + publisher + npm link carried through.
        let fd = &out[0];
        assert_eq!(fd.version, "0.1.2");
        assert_eq!(fd.publisher, "wierdbytes");
        assert_eq!(fd.npm_url, "https://www.npmjs.com/package/@autosk/feature-dev");
        assert_eq!(fd.updated, "2026-06-15T15:13:36.234Z");

        // Missing `links.npm` falls back to a constructed npmjs.com URL.
        let nd = &out[5];
        assert_eq!(nd.npm_url, "https://www.npmjs.com/package/no-downloads-ext");
        // Missing object-level `updated` falls back to the package date.
        assert_eq!(nd.updated, "2026-01-02T00:00:00.000Z");
    }

    #[test]
    fn empty_objects_yields_empty_list() {
        let resp: SearchResponse = serde_json::from_str(r#"{"objects":[]}"#).unwrap();
        assert!(normalize(resp).is_empty());
    }
}
