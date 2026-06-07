//! Persisted project registry (plan §7.4): add/list/remove + atomic, 0600 file.

use std::os::unix::fs::PermissionsExt;

use autosk_core::registry::Registry;

#[test]
fn add_list_remove_roundtrip() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("projects.json");
    let reg = Registry::open_at(&path);

    assert!(reg.list().unwrap().is_empty(), "fresh registry is empty");

    let p = reg.add("/repo/beta", "/repo/beta/.autosk/db").unwrap();
    assert_eq!(p.root, "/repo/beta");
    assert_eq!(p.name, "beta");
    reg.add("/repo/alpha", "/repo/alpha/.autosk/db").unwrap();

    let list = reg.list().unwrap();
    assert_eq!(list.len(), 2);
    // Sorted by root.
    assert_eq!(list[0].root, "/repo/alpha");
    assert_eq!(list[1].root, "/repo/beta");

    // Re-adding the same root replaces (idempotent), not duplicates.
    reg.add("/repo/alpha", "/repo/alpha/.autosk/db").unwrap();
    assert_eq!(reg.list().unwrap().len(), 2);

    // The file is 0600.
    let mode = std::fs::metadata(&path).unwrap().permissions().mode() & 0o777;
    assert_eq!(mode, 0o600, "registry file must be 0600");

    assert!(reg.remove("/repo/alpha").unwrap());
    assert!(
        !reg.remove("/repo/alpha").unwrap(),
        "second remove is a no-op"
    );
    let list = reg.list().unwrap();
    assert_eq!(list.len(), 1);
    assert_eq!(list[0].root, "/repo/beta");
}

#[test]
fn corrupt_file_is_not_clobbered() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("projects.json");
    // Seed two real projects, then corrupt the file on disk.
    let reg = Registry::open_at(&path);
    reg.add("/repo/alpha", "/repo/alpha/.autosk/db").unwrap();
    reg.add("/repo/beta", "/repo/beta/.autosk/db").unwrap();
    std::fs::write(&path, b"{ this is not json").unwrap();

    // load-backed ops surface the corruption instead of silently resetting.
    assert!(reg.list().is_err(), "list refuses to parse a corrupt file");
    assert!(
        reg.add("/repo/gamma", "/repo/gamma/.autosk/db").is_err(),
        "add refuses to clobber a corrupt file"
    );
    // The bad bytes are still on disk (nothing was destroyed).
    assert_eq!(
        std::fs::read(&path).unwrap(),
        b"{ this is not json",
        "corrupt file left intact for the operator to recover"
    );

    // An empty / whitespace-only file is tolerated as a fresh registry.
    std::fs::write(&path, b"   \n").unwrap();
    assert!(
        reg.list().unwrap().is_empty(),
        "blank file = empty registry"
    );
}

#[test]
fn survives_reopen() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("projects.json");
    Registry::open_at(&path).add("/x", "/x/.autosk/db").unwrap();
    // A fresh handle reads the persisted state.
    let reopened = Registry::open_at(&path);
    assert_eq!(reopened.list().unwrap().len(), 1);
}
