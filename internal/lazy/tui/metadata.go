package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/jesseduffield/gocui"
)

// taskEditMetadata opens $EDITOR on the selected task's metadata (as pretty
// JSON), then applies the edit as a set/unset diff. The diff is computed at
// TOP-LEVEL-KEY granularity (plan §8): a changed/added top-level key is sent via
// task.metadata.set (its whole value), a removed top-level key via
// task.metadata.unset — never a whole-document replace on the wire.
// Last-writer-wins against a concurrent engine step_visits bump is acceptable
// (the same model as a direct task.json edit).
func (gu *Gui) taskEditMetadata(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	id := t.ID
	old := t.Metadata
	initial := metadataEditDoc(old)
	gu.runDispatch(func() {
		edited, err := gu.editObject(initial)
		if err != nil {
			gu.flashf("err", "metadata edit: %v", err)
			return
		}
		patch, unset, err := metadataDiff(old, edited)
		if err != nil {
			gu.flashf("err", "metadata: %v", err)
			return
		}
		if len(patch) == 0 && len(unset) == 0 {
			gu.flashf("info", "metadata unchanged")
			return
		}
		if len(patch) > 0 {
			if err := gu.ds.SetTaskMetadata(gu.ctx, id, patch); err != nil {
				gu.flashf("err", "metadata set: %v", err)
				return
			}
		}
		if len(unset) > 0 {
			if err := gu.ds.UnsetTaskMetadata(gu.ctx, id, unset); err != nil {
				gu.flashf("err", "metadata unset: %v", err)
				return
			}
		}
		gu.flashf("info", "metadata updated %s", id)
		gu.refreshAll()
	})
	return nil
}

// metadataEditDoc renders a metadata bag as the pretty-JSON document the editor
// is seeded with. An empty/nil bag seeds an empty object so the operator edits
// valid JSON from the first keystroke.
func metadataEditDoc(meta map[string]any) string {
	if len(meta) == 0 {
		return "{}\n"
	}
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return "{}\n"
	}
	return string(b) + "\n"
}

// metadataDiff parses the edited JSON document (which MUST be a JSON object) and
// diffs it against `old` at top-level-key granularity, returning the set-patch
// (added/changed keys, keyed by their top-level name) and the unset-list
// (removed keys). A non-object document or invalid JSON returns an error.
func metadataDiff(old map[string]any, editedJSON string) (map[string]any, []string, error) {
	trimmed := strings.TrimSpace(editedJSON)
	if trimmed == "" {
		// An emptied buffer means "clear everything" — unset every old key.
		return nil, sortedKeys(old), nil
	}
	var next map[string]any
	dec := json.NewDecoder(strings.NewReader(trimmed))
	if err := dec.Decode(&next); err != nil {
		return nil, nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if next == nil {
		// A literal `null` document → clear everything.
		return nil, sortedKeys(old), nil
	}

	patch := map[string]any{}
	for k, v := range next {
		prev, had := old[k]
		if !had || !reflect.DeepEqual(prev, v) {
			patch[k] = v
		}
	}
	var unset []string
	for _, k := range sortedKeys(old) {
		if _, stillThere := next[k]; !stillThere {
			unset = append(unset, k)
		}
	}
	return patch, unset, nil
}

// sortedKeys returns a map's keys in deterministic (sorted) order.
func sortedKeys(m map[string]any) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Small maps; a tiny hand-rolled insertion sort keeps them in deterministic
	// key order without importing sort for a handful of entries.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}

// runEditorJSON is the default Gui.editObject: it seeds `initial` into a temp
// `.json` file, suspends the gocui screen, runs the operator's editor
// ($VISUAL, then $EDITOR, then `vi`) on it, resumes the screen, and returns the
// edited bytes. The editor command may carry args (e.g. `code --wait`); it is
// split on whitespace. Runs off the gocui main loop (the metadata handler
// dispatches it onto a worker).
func (gu *Gui) runEditorJSON(initial string) (string, error) {
	tmp, err := os.CreateTemp("", "autosk-metadata-*.json")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.WriteString(initial); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close temp file: %w", err)
	}

	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	fields := strings.Fields(editor)
	args := append(fields[1:], path)
	cmd := exec.Command(fields[0], args...) //nolint:gosec // operator's own $EDITOR
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Suspend the TUI so the editor owns the terminal, then restore it. Resume
	// is deferred so a failed editor run still hands the screen back.
	if gu.g != nil {
		if err := gu.g.Suspend(); err != nil {
			return "", fmt.Errorf("suspend tui: %w", err)
		}
		defer func() { _ = gu.g.Resume() }()
	}
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("editor %q: %w", filepath.Base(fields[0]), err)
	}

	out, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read temp file: %w", err)
	}
	return string(out), nil
}
