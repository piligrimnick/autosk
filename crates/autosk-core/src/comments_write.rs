//! `comments` writes — the Rust port of `internal/comments.Add`.
//!
//! Comments are immutable append-only notes. `add` trims a trailing newline,
//! rejects empty text and empty FKs, inserts the row (autoincrement id +
//! `created_at = now`), and returns the materialised [`wire::Comment`].

use rusqlite::{params, Connection};

use crate::error::{Error, Result};
use crate::timefmt;
use autosk_proto::wire;

/// `Add` — inserts a comment and returns it (mirror of `comments.Store.Add`).
/// `text` has a trailing `\n` stripped; empty-after-trim is rejected.
pub fn add(conn: &Connection, task_id: &str, author_id: &str, text: &str) -> Result<wire::Comment> {
    let text = text.trim_end_matches('\n');
    if text.trim().is_empty() {
        return Err(Error::Invalid("comment text is empty".into()));
    }
    if task_id.is_empty() || author_id.is_empty() {
        return Err(Error::Invalid(format!(
            "invalid task_id or author_id: task_id={task_id:?} author_id={author_id:?}"
        )));
    }
    let now = timefmt::now_unix();
    conn.execute(
        "INSERT INTO comments(task_id, author_id, text, created_at) VALUES (?1, ?2, ?3, ?4)",
        params![task_id, author_id, text, now],
    )?;
    let id = conn.last_insert_rowid();
    get_by_id(conn, id)
}

/// Reads one comment by id (joined against `agents.name`), or
/// [`Error::NotFound`].
pub fn get_by_id(conn: &Connection, id: i64) -> Result<wire::Comment> {
    use rusqlite::OptionalExtension;
    conn.query_row(
        "SELECT c.id, c.task_id, c.author_id, a.name, c.text, c.created_at \
           FROM comments c JOIN agents a ON c.author_id = a.id WHERE c.id = ?1",
        params![id],
        |row| {
            Ok(wire::Comment {
                id: row.get(0)?,
                task_id: row.get(1)?,
                author_id: row.get(2)?,
                author_name: row.get(3)?,
                text: row.get(4)?,
                created_at: timefmt::rfc3339_utc(row.get::<_, i64>(5)?),
            })
        },
    )
    .optional()?
    .ok_or(Error::NotFound)
}
