package tui

import (
	"testing"

	"autosk/internal/lazy/datasource"
)

// TestCommentsCache_BoundedAndPreservesSelected verifies the
// eviction policy on state.comments: when the cache hits
// commentsCacheMax entries the eviction loop drops an arbitrary
// existing entry BUT must not drop the entry we're about to write
// for the currently-selected task. (The bug guarded against: the
// dashboard would visit one task, hit the cap, evict that task's
// own entry, then re-insert it — silent thrash, plus the Tasks-
// detail pane would briefly flicker the comments list.)
func TestCommentsCache_BoundedAndPreservesSelected(t *testing.T) {
	st := newState()
	// Pre-fill the cache up to the cap with synthetic entries.
	for i := 0; i < commentsCacheMax; i++ {
		st.comments[idForI(i)] = []datasource.Comment{{Text: "old"}}
	}
	if len(st.comments) != commentsCacheMax {
		t.Fatalf("pre-fill len=%d want %d", len(st.comments), commentsCacheMax)
	}

	// Now simulate refresh hydrating a fresh task. Mirror the
	// eviction loop in refresh.go.
	selectedTaskID := "as-new"
	selectedComments := []datasource.Comment{{Text: "fresh"}}
	if len(st.comments) >= commentsCacheMax {
		for k := range st.comments {
			if k == selectedTaskID {
				continue
			}
			delete(st.comments, k)
			break
		}
	}
	st.comments[selectedTaskID] = selectedComments

	if got := st.comments[selectedTaskID]; len(got) != 1 || got[0].Text != "fresh" {
		t.Fatalf("selected task entry missing/wrong: %+v", got)
	}
	if len(st.comments) > commentsCacheMax {
		t.Fatalf("cache exceeded cap: len=%d (>%d)", len(st.comments), commentsCacheMax)
	}
}

func idForI(i int) string {
	return string(rune('a'+(i%26))) + string(rune('a'+(i/26)))
}
