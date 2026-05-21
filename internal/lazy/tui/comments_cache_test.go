package tui

import (
	"testing"

	"autosk/internal/lazy/datasource"
)

// TestCommentsCache_BoundedAndPreservesSelected verifies the
// eviction policy on state.comments via the shared
// evictCacheIfNeeded helper:
//
//  1. When the cache hits commentsCacheMax entries AND the
//     selectedTaskID is NEW (not yet in the cache), eviction drops
//     an arbitrary existing entry (NOT the selected one).
//  2. When the cache hits commentsCacheMax AND the selectedTaskID
//     is ALREADY in the cache, eviction is a no-op (we're
//     replacing, not growing).
//
// The previous test only exercised case 1; the eviction branch's
// 'skip selectedTaskID' guard was dead.  Now we exercise both
// branches, plus the bonus optimisation that skips the eviction
// loop when the key already exists in the map.
func TestCommentsCache_BoundedAndPreservesSelected(t *testing.T) {
	t.Run("evicts_non_selected_when_new", func(t *testing.T) {
		st := newState()
		for i := 0; i < commentsCacheMax; i++ {
			st.comments[idForI(i)] = []datasource.Comment{{Text: "old"}}
		}
		selectedTaskID := "ask-fffff0"
		selectedComments := []datasource.Comment{{Text: "fresh"}}
		evictCacheIfNeeded(st.comments, selectedTaskID, commentsCacheMax)
		st.comments[selectedTaskID] = selectedComments

		if got := st.comments[selectedTaskID]; len(got) != 1 || got[0].Text != "fresh" {
			t.Fatalf("selected task entry missing/wrong: %+v", got)
		}
		if len(st.comments) > commentsCacheMax {
			t.Fatalf("cache exceeded cap: len=%d (>%d)", len(st.comments), commentsCacheMax)
		}
	})

	t.Run("preserves_selected_when_already_in_cache", func(t *testing.T) {
		// Fill the cache to cap with the selected entry INCLUDED.
		st := newState()
		selectedTaskID := "ask-5e1ec7"
		for i := 0; i < commentsCacheMax-1; i++ {
			st.comments[idForI(i)] = []datasource.Comment{{Text: "old"}}
		}
		st.comments[selectedTaskID] = []datasource.Comment{{Text: "old-selected"}}
		if len(st.comments) != commentsCacheMax {
			t.Fatalf("pre-fill len=%d want %d", len(st.comments), commentsCacheMax)
		}
		// Re-hydrate selectedTaskID (the production refresh path on a
		// re-visit). Since the key is already in the map, eviction
		// should be a no-op — we're replacing, not growing.
		evictCacheIfNeeded(st.comments, selectedTaskID, commentsCacheMax)
		st.comments[selectedTaskID] = []datasource.Comment{{Text: "fresh-selected"}}
		if len(st.comments) != commentsCacheMax {
			t.Fatalf("cache size drifted across replacement: len=%d (want %d)", len(st.comments), commentsCacheMax)
		}
		if got := st.comments[selectedTaskID]; len(got) != 1 || got[0].Text != "fresh-selected" {
			t.Fatalf("selected task entry not updated: %+v", got)
		}
	})

	t.Run("noop_below_cap", func(t *testing.T) {
		st := newState()
		st.comments["x"] = []datasource.Comment{{Text: "x"}}
		evictCacheIfNeeded(st.comments, "y", commentsCacheMax)
		if len(st.comments) != 1 || st.comments["x"][0].Text != "x" {
			t.Fatalf("eviction triggered below cap: %+v", st.comments)
		}
	})
}

func idForI(i int) string {
	return string(rune('a'+(i%26))) + string(rune('a'+(i/26)))
}
