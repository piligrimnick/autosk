package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/jesseduffield/gocui"

	"autosk/internal/lazy/ansiutil"
)

// TestCheatsheetEditor_AppendsLettersToFilter pins R1: every
// printable rune the editor sees — including 'j' and 'k' —
// appends to the live filter. No rune is short-circuited into
// cursor motion; navigation is exclusively driven by the
// view-scoped arrows/wheel bindings on winPopupCheatsheet.
//
// Regression for the previous bug where typing "block" landed
// filter="bloc" because 'k' was swallowed as cursor-up, and 'k'
// to search for "kill" jumped the cursor instead of filtering.
func TestCheatsheetEditor_AppendsLettersToFilter(t *testing.T) {
	cases := []struct {
		name string
		word string
	}{
		{"block", "block"},
		{"kill", "kill"},
		{"unblock", "unblock"},
		{"j_alone", "j"},
		{"k_alone", "k"},
		{"jk_pair", "jk"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gu := &Gui{st: newState()}
			gu.st.focused = panelTasks
			_ = gu.openHelp(nil, nil)
			initialCursor := gu.st.popup.CheatsheetCursor
			for _, ch := range c.word {
				if !gu.cheatsheetEditor(nil, 0, ch, gocui.ModNone) {
					t.Fatalf("editor refused printable rune %q", ch)
				}
			}
			if gu.st.popup.CheatsheetFilter != c.word {
				t.Errorf("filter=%q want %q (rune was silently consumed)", gu.st.popup.CheatsheetFilter, c.word)
			}
			// Cursor must not have been moved by the typing (each
			// append resets cursor to 0; initial cursor is also 0).
			if gu.st.popup.CheatsheetCursor != 0 {
				t.Errorf("cursor=%d after typing %q, want 0 (initial=%d)", gu.st.popup.CheatsheetCursor, c.word, initialCursor)
			}
		})
	}
}

// TestCheatsheetEditor_IgnoresModifierChords pins the inverse:
// chords carrying a modifier (Alt/Motion) are NOT routed into the
// filter — they fall through to global keybindings. Backspace
// arrives as gocui.KeyBackspace (key != 0, ch == 0) and so is
// also ignored by the editor (the view-scoped binding handles
// it).
func TestCheatsheetEditor_IgnoresModifierChords(t *testing.T) {
	gu := &Gui{st: newState()}
	_ = gu.openHelp(nil, nil)
	if gu.cheatsheetEditor(nil, 0, 'x', gocui.ModAlt) {
		t.Errorf("alt+x must fall through, editor consumed it")
	}
	if gu.st.popup.CheatsheetFilter != "" {
		t.Errorf("alt+x leaked into filter: %q", gu.st.popup.CheatsheetFilter)
	}
	// Non-printable rune with ch==0 (e.g. raw keycode) is
	// dropped too.
	if gu.cheatsheetEditor(nil, gocui.KeyBackspace, 0, gocui.ModNone) {
		t.Errorf("backspace must fall through, editor consumed it")
	}
}

// TestCheatsheet_NoCloseRowVisible pins R2: the global Esc
// binding is intentionally not listed in the cheatsheet, so
// selecting a "back / close" row can't fire handleEsc with the
// popup already cleared (which would clobber the focused panel's
// filter as an unsignalled side effect).
func TestCheatsheet_NoCloseRowVisible(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.st.focused = panelTasks
	_ = gu.openHelp(nil, nil)
	for _, it := range gu.st.popup.CheatsheetItems {
		if it.IsHeader {
			continue
		}
		if it.KeyLabel == "esc" {
			t.Errorf("cheatsheet exposes the global esc binding (%q); selecting it would route through handleEsc with a no-op popup state and wipe the panel filter", it.Description)
		}
		if strings.Contains(strings.ToLower(it.Description), "back / close") {
			t.Errorf("cheatsheet still carries a back/close row: %q", it.Description)
		}
	}
}

// TestCheatsheet_SectionOrder pins the bucket ordering contract
// from AC2: the cheatsheet renders Local first, then Global, then
// Navigation. Buckets with zero rows are omitted (no empty
// header). The headers wear the `--- Local ---` / `--- Global ---`
// / `--- Navigation ---` shape.
func TestCheatsheet_SectionOrder(t *testing.T) {
	gu := &Gui{st: newState()}
	_ = gu.openHelp(nil, nil)
	if k := gu.st.popup.Kind; k != popupCheatsheet {
		t.Fatalf("popup kind=%v want popupCheatsheet", k)
	}
	items := gu.st.popup.CheatsheetItems
	if len(items) == 0 {
		t.Fatalf("cheatsheet items empty")
	}
	// Walk the items, capture section header order.
	var headers []string
	for _, it := range items {
		if it.IsHeader {
			headers = append(headers, it.Section)
		}
	}
	if len(headers) < 2 {
		t.Fatalf("expected at least 2 buckets, got headers=%v", headers)
	}
	// Order pinned by AC2: Local, Global, Navigation (any omitted
	// when its bucket is empty, but order of those present is fixed).
	wantOrder := []string{"Local", "Global", "Navigation"}
	hi := 0
	for _, want := range wantOrder {
		if hi < len(headers) && headers[hi] == want {
			hi++
		}
	}
	if hi != len(headers) {
		t.Fatalf("section order mismatch: got %v, want subset of %v in that order", headers, wantOrder)
	}
}

// TestCheatsheet_NoCrossPanelLeakage pins AC2: bindings whose View
// belongs to a different panel from the focused one are NOT listed
// in the cheatsheet. Open with focus on Workflows; assert that no
// item carries a description that's exclusively a Tasks/Jobs/Agents
// panel verb (e.g. `cancel job`, `new task`, `install`).
func TestCheatsheet_NoCrossPanelLeakage(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.st.focused = panelWorkflows
	_ = gu.openHelp(nil, nil)
	bannedTaskOnly := []string{"new task", "edit task", "mark done", "cancel task", "set priority", "edit metadata"}
	bannedJobsOnly := []string{"cancel job"}
	bannedAgentsOnly := []string{"install (via CLI hint)", "uninstall (via CLI hint)"}
	all := append(append(append([]string{}, bannedTaskOnly...), bannedJobsOnly...), bannedAgentsOnly...)
	for _, it := range gu.st.popup.CheatsheetItems {
		if it.IsHeader {
			continue
		}
		for _, banned := range all {
			if it.Description == banned {
				t.Errorf("cheatsheet leaked cross-panel binding %q on focused=Workflows", banned)
			}
		}
	}
	// And: v2 workflows are READ-ONLY, so the Local section must NOT
	// contain any workflow write verbs.
	gotLocalDescs := map[string]bool{}
	currentSection := ""
	for _, it := range gu.st.popup.CheatsheetItems {
		if it.IsHeader {
			currentSection = it.Section
			continue
		}
		if currentSection == "Local" {
			gotLocalDescs[it.Description] = true
		}
	}
	for _, banned := range []string{"new workflow (from file)", "delete workflow", "toggle isolation"} {
		if gotLocalDescs[banned] {
			t.Errorf("Local bucket leaked removed workflow write verb %q; got %v", banned, gotLocalDescs)
		}
	}
}

// TestCheatsheet_FilterAndCursor: typing into the filter shrinks
// the visible item set; the cursor resets to row 0 on filter
// change; Backspace pops a rune; Esc with a non-empty filter
// clears it but keeps the popup open; Esc with an empty filter
// closes the popup.
func TestCheatsheet_FilterAndCursor(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.st.focused = panelTasks
	_ = gu.openHelp(nil, nil)
	initialCount := cheatsheetVisibleRowCount(gu.st.popup.CheatsheetItems)
	if initialCount < 5 {
		t.Fatalf("expected several visible rows; got %d", initialCount)
	}
	// Type 'comment' → must produce a smaller filtered set
	// including the "add comment" row.
	gu.cheatsheetAppendRune('c')
	gu.cheatsheetAppendRune('o')
	gu.cheatsheetAppendRune('m')
	gu.cheatsheetAppendRune('m')
	if gu.st.popup.CheatsheetFilter != "comm" {
		t.Fatalf("filter=%q want %q", gu.st.popup.CheatsheetFilter, "comm")
	}
	filtered := filterCheatsheetItems(gu.st.popup.CheatsheetItems, "comm")
	if cheatsheetVisibleRowCount(filtered) >= initialCount {
		t.Errorf("filter did not shrink set: %d >= %d", cheatsheetVisibleRowCount(filtered), initialCount)
	}
	// "add comment" must appear.
	visibleDescs := map[string]bool{}
	for _, it := range filtered {
		if !it.IsHeader {
			visibleDescs[it.Description] = true
		}
	}
	if !visibleDescs["add comment"] {
		t.Errorf("filter \"comm\" did not include \"add comment\": %v", visibleDescs)
	}

	// Backspace pops one rune.
	_ = gu.cheatsheetBackspace(nil, nil)
	if gu.st.popup.CheatsheetFilter != "com" {
		t.Errorf("backspace did not pop: filter=%q", gu.st.popup.CheatsheetFilter)
	}

	// Esc with non-empty filter clears it but keeps popup open.
	_ = gu.cheatsheetEscape(nil, nil)
	if gu.st.popup.CheatsheetFilter != "" {
		t.Errorf("first Esc did not clear filter: %q", gu.st.popup.CheatsheetFilter)
	}
	if gu.st.popup.Kind != popupCheatsheet {
		t.Errorf("first Esc closed the popup; want it to stay open with cleared filter")
	}

	// Esc with empty filter closes.
	_ = gu.cheatsheetEscape(nil, nil)
	if gu.st.popup.Kind != popupNone {
		t.Errorf("second Esc did not close popup: kind=%v", gu.st.popup.Kind)
	}
}

// TestCheatsheet_EnterExecutesSelected pins AC2: pressing Enter
// closes the popup AND invokes the cursored binding's handler
// exactly once. We rewire one binding's handler with a counter and
// then drive the open → cursor → accept dance.
func TestCheatsheet_EnterExecutesSelected(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.st.focused = panelTasks

	// Build a synthetic, controlled item set so the test isn't
	// brittle against future binding registry changes. We exercise
	// the cheatsheet plumbing directly: openHelp → state mutation
	// only differs from this in how it BUILDS the items slice,
	// which we cover in TestCheatsheet_SectionOrder.
	var fired int
	items := []cheatsheetItem{
		{IsHeader: true, Section: "Local"},
		{Section: "Local", KeyLabel: "n", Description: "alpha", Handler: func() error { fired++; return nil }},
		{Section: "Local", KeyLabel: "c", Description: "beta", Handler: func() error { fired += 10; return nil }},
		{Section: "Local", KeyLabel: "d", Description: "gamma", Handler: func() error { fired += 100; return nil }},
	}
	gu.st.popup = popupState{
		Kind:             popupCheatsheet,
		Title:            "t",
		CheatsheetItems:  items,
		CheatsheetCursor: 0,
	}
	// Move cursor to row 2 (gamma) via j twice.
	gu.cheatsheetMoveCursor(+1)
	gu.cheatsheetMoveCursor(+1)
	if gu.st.popup.CheatsheetCursor != 2 {
		t.Fatalf("cursor=%d want 2", gu.st.popup.CheatsheetCursor)
	}
	if err := gu.cheatsheetAccept(nil, nil); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if fired != 100 {
		t.Errorf("expected gamma handler to fire (counter+=100), got fired=%d", fired)
	}
	if gu.st.popup.Kind != popupNone {
		t.Errorf("popup not cleared after Enter: kind=%v", gu.st.popup.Kind)
	}
}

// TestCheatsheet_NoClaimBinding: regression from the v0.2 schema
// removal of the claim verb. The cheatsheet body must not contain
// a row whose description or key advertises 'claim'.
func TestCheatsheet_NoClaimBinding(t *testing.T) {
	gu := &Gui{st: newState()}
	_ = gu.openHelp(nil, nil)
	for _, it := range gu.st.popup.CheatsheetItems {
		if it.IsHeader {
			continue
		}
		joined := strings.ToLower(it.KeyLabel + " " + it.Description)
		if strings.Contains(joined, "claim") {
			t.Errorf("cheatsheet advertises 'claim': %q / %q", it.KeyLabel, it.Description)
		}
	}
}

// TestCheatsheet_NoInspectorReferences: the help body must not
// mention the (removed) Inspector or its tabs.
func TestCheatsheet_NoInspectorReferences(t *testing.T) {
	gu := &Gui{st: newState()}
	_ = gu.openHelp(nil, nil)
	for _, it := range gu.st.popup.CheatsheetItems {
		if it.IsHeader {
			continue
		}
		joined := strings.ToLower(it.Description)
		if strings.Contains(joined, "inspector") {
			t.Errorf("cheatsheet still references inspector: %q", it.Description)
		}
		if strings.Contains(it.Description, "Live tab") || strings.Contains(it.Description, "Archive tab") {
			t.Errorf("cheatsheet references removed Inspector tabs: %q", it.Description)
		}
	}
}

// TestBindingSpecs_NoDupDescriptionPerBucket pins AC1: a binding
// intended for the cheatsheet (Description != "") must carry a Tag
// AND must not collide with another binding's description in the
// same bucket. Bucket key for collision is (Tag, View) so a
// duplicate Description across panels is still allowed (each panel
// gets its own Local bucket).
//
// Skips entries with empty Description (popup-scoped plumbing).
func TestBindingSpecs_NoDupDescriptionPerBucket(t *testing.T) {
	gu := &Gui{st: newState()}
	specs := gu.bindingSpecs()
	type bucket struct {
		tag  string
		view string
	}
	seen := map[bucket]map[string]bool{}
	for _, sp := range specs {
		if sp.Description == "" {
			continue
		}
		// Tag is required on cheatsheet-visible entries, except for
		// per-panel locals where Tag == "" is the implicit "local"
		// bucket. The relaxed rule: View must be non-empty for an
		// untagged entry.
		if sp.Tag == "" && sp.View == "" {
			t.Errorf("binding has Description=%q but no Tag and no View \u2014 cheatsheet cannot bucket it", sp.Description)
		}
		key := bucket{tag: sp.Tag, view: sp.View}
		if seen[key] == nil {
			seen[key] = map[string]bool{}
		}
		if seen[key][sp.Description] {
			t.Errorf("duplicate Description %q in bucket (Tag=%q, View=%q)", sp.Description, sp.Tag, sp.View)
		}
		seen[key][sp.Description] = true
	}
}

// TestKeyLabel pins the keyLabel formatting rules \u2014 lowercase
// plain keys, named gocui keys (enter/esc/tab/arrows/page), and
// modifier chords using `ctrl+x` shape.
func TestKeyLabel(t *testing.T) {
	cases := []struct {
		key  any
		want string
	}{
		{rune('q'), "q"},
		{rune('R'), "R"},
		{rune(' '), "space"},
		{gocui.KeyEnter, "enter"},
		{gocui.KeyEsc, "esc"},
		{gocui.KeyTab, "tab"},
		{gocui.KeyArrowDown, "\u2193"},
		{gocui.KeyArrowUp, "\u2191"},
		{gocui.KeyPgup, "pgup"},
		{gocui.KeyPgdn, "pgdn"},
		{gocui.KeyBackspace, "backspace"},
		{gocui.KeyCtrlD, "ctrl+d"},
		{gocui.KeyCtrlR, "ctrl+r"},
		{gocui.KeyCtrlS, "ctrl+s"},
		{gocui.MouseWheelUp, "wheel\u2191"},
		{gocui.MouseWheelDown, "wheel\u2193"},
	}
	for _, c := range cases {
		if got := keyLabel(c.key); got != c.want {
			t.Errorf("keyLabel(%v): got %q want %q", c.key, got, c.want)
		}
	}
}

// TestRenderCheatsheetBody_ColumnAlignment: every binding row's
// description token starts at the same visible-cell column —
// i.e. the key column is right-aligned to the widest visible
// key. We probe by locating each description text and measuring
// the lipgloss-visible width of the prefix preceding it.
func TestRenderCheatsheetBody_ColumnAlignment(t *testing.T) {
	items := []cheatsheetItem{
		{IsHeader: true, Section: "Local"},
		{Section: "Local", KeyLabel: "n", Description: "alpha"},
		{Section: "Local", KeyLabel: "ctrl+f", Description: "beta"},
		{Section: "Local", KeyLabel: "wheel\u2193", Description: "gamma"},
	}
	body := renderCheatsheetBody(items, -1) // no cursor highlight
	visible := ansiutil.Strip(body)
	lines := strings.Split(visible, "\n")
	var prefixCols []int
	for _, desc := range []string{"alpha", "beta", "gamma"} {
		found := false
		for _, ln := range lines {
			idx := strings.Index(ln, desc)
			if idx < 0 {
				continue
			}
			prefixCols = append(prefixCols, lipgloss.Width(ln[:idx]))
			found = true
			break
		}
		if !found {
			t.Fatalf("description token %q missing from body:\n%s", desc, visible)
		}
	}
	first := prefixCols[0]
	for i, w := range prefixCols {
		if w != first {
			t.Errorf("row %d: description column at cell %d, want %d (key column is not right-aligned consistently)\nbody=%q", i, w, first, visible)
		}
	}
}
