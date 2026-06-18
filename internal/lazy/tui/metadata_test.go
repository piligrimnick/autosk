package tui

import (
	"context"
	"reflect"
	"testing"

	"autosk/internal/lazy/datasource"
)

// fakeMetaDS records the metadata set/unset calls the editor diff makes.
type fakeMetaDS struct {
	refreshFakeDS
	setID    string
	setPatch map[string]any
	setCalls int
	unsetID  string
	unsetKey []string
	unCalls  int
}

func (f *fakeMetaDS) SetTaskMetadata(_ context.Context, id string, patch map[string]any) error {
	f.setCalls++
	f.setID = id
	f.setPatch = patch
	return nil
}

func (f *fakeMetaDS) UnsetTaskMetadata(_ context.Context, id string, keys []string) error {
	f.unCalls++
	f.unsetID = id
	f.unsetKey = keys
	return nil
}

func TestMetadataDiff(t *testing.T) {
	old := map[string]any{
		"step_visits": map[string]any{"dev": float64(2), "review": float64(1)},
		"note":        "keep",
		"drop":        true,
	}
	cases := []struct {
		name      string
		edited    string
		wantPatch map[string]any
		wantUnset []string
		wantErr   bool
	}{
		{
			name:      "no change",
			edited:    `{"step_visits":{"dev":2,"review":1},"note":"keep","drop":true}`,
			wantPatch: map[string]any{},
			wantUnset: nil,
		},
		{
			name:      "change a nested value + add a key + remove a key",
			edited:    `{"step_visits":{"dev":5,"review":1},"note":"keep","added":7}`,
			wantPatch: map[string]any{"step_visits": map[string]any{"dev": float64(5), "review": float64(1)}, "added": float64(7)},
			wantUnset: []string{"drop"},
		},
		{
			name:      "empty object clears every key",
			edited:    `{}`,
			wantPatch: map[string]any{},
			wantUnset: []string{"drop", "note", "step_visits"},
		},
		{
			name:      "blank document clears every key",
			edited:    "   \n",
			wantPatch: nil,
			wantUnset: []string{"drop", "note", "step_visits"},
		},
		{
			name:    "invalid JSON errors",
			edited:  `{not json`,
			wantErr: true,
		},
		{
			name:    "a non-object document errors",
			edited:  `[1,2,3]`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			patch, unset, err := metadataDiff(old, tc.edited)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got patch=%v unset=%v", patch, unset)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(patch, tc.wantPatch) {
				t.Errorf("patch = %v, want %v", patch, tc.wantPatch)
			}
			if !reflect.DeepEqual(unset, tc.wantUnset) {
				t.Errorf("unset = %v, want %v", unset, tc.wantUnset)
			}
		})
	}
}

// TestTaskEditMetadataAppliesDiff drives the full editor path with an injected
// editObject (no $EDITOR / suspend) and a recording datasource: a changed +
// added key flow through SetTaskMetadata, a removed key through
// UnsetTaskMetadata, and the view refreshes.
func TestTaskEditMetadataAppliesDiff(t *testing.T) {
	gu := newHeadlessGui(t, 120, 40)
	ds := &fakeMetaDS{}
	gu.ds = ds
	gu.ctx = context.Background()
	gu.dispatch = func(f func()) { f() }
	gu.editObject = func(string) (string, error) {
		// Change step_visits.dev, add a key, drop "stale".
		return `{"step_visits":{"dev":9},"added":"x"}`, nil
	}
	gu.st.tasks = []datasource.Task{{
		ID: "ask-meta01",
		Metadata: map[string]any{
			"step_visits": map[string]any{"dev": float64(2)},
			"stale":       true,
		},
	}}
	gu.st.taskCursor = 0
	gu.st.focused = panelTasks

	if err := gu.taskEditMetadata(nil, nil); err != nil {
		t.Fatalf("taskEditMetadata: %v", err)
	}
	if ds.setCalls != 1 || ds.setID != "ask-meta01" {
		t.Fatalf("SetTaskMetadata calls=%d id=%q, want 1 / ask-meta01", ds.setCalls, ds.setID)
	}
	wantPatch := map[string]any{"step_visits": map[string]any{"dev": float64(9)}, "added": "x"}
	if !reflect.DeepEqual(ds.setPatch, wantPatch) {
		t.Errorf("patch = %v, want %v", ds.setPatch, wantPatch)
	}
	if ds.unCalls != 1 || !reflect.DeepEqual(ds.unsetKey, []string{"stale"}) {
		t.Errorf("UnsetTaskMetadata calls=%d keys=%v, want 1 / [stale]", ds.unCalls, ds.unsetKey)
	}
	if gu.st.flash.Level != "info" {
		t.Errorf("flash level = %q, want info; flash=%+v", gu.st.flash.Level, gu.st.flash)
	}
}

// TestTaskEditMetadataNoChange: editing without changing anything makes no DS
// calls and flashes "unchanged".
func TestTaskEditMetadataNoChange(t *testing.T) {
	gu := newHeadlessGui(t, 120, 40)
	ds := &fakeMetaDS{}
	gu.ds = ds
	gu.ctx = context.Background()
	gu.dispatch = func(f func()) { f() }
	gu.editObject = func(initial string) (string, error) { return initial, nil } // unchanged
	gu.st.tasks = []datasource.Task{{ID: "ask-meta02", Metadata: map[string]any{"a": float64(1)}}}
	gu.st.taskCursor = 0
	gu.st.focused = panelTasks

	if err := gu.taskEditMetadata(nil, nil); err != nil {
		t.Fatalf("taskEditMetadata: %v", err)
	}
	if ds.setCalls != 0 || ds.unCalls != 0 {
		t.Errorf("expected no DS calls, got set=%d unset=%d", ds.setCalls, ds.unCalls)
	}
}
