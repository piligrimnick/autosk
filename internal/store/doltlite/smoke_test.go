//go:build libsqlite3

package doltlite_test

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// TestDoltliteEngine confirms the build is linked against libdoltlite and not
// stock libsqlite3. If this passes, the prolly tree engine is active and the
// rest of the autosk doltlite impl can rely on Dolt SQL functions/vtables.
//
// Run: make test (the Makefile exports the CGO flags and `libsqlite3` tag).
//
// If libdoltlite isn't linked, the SELECT will fail with "no such function:
// doltlite_engine" — that's the signal that the build pipeline is wrong.
func TestDoltliteEngine(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	var engine string
	if err := db.QueryRow("SELECT doltlite_engine()").Scan(&engine); err != nil {
		t.Fatalf("doltlite_engine() query failed (is libdoltlite linked?): %v", err)
	}
	if engine != "prolly" {
		t.Fatalf("expected engine=prolly, got %q", engine)
	}
}
