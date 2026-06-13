package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/jesseduffield/gocui"

	"autosk/internal/lazy/ansiutil"
)

// TestRenderStatusBar_NoDoubleSpaces pins AC4: the status bar
// never emits two consecutive spaces. The previous shape joined
// blocks with a literal "  " (double-space) padding; the new
// contract is " | " (single-space-pipe-single-space) between
// blocks and single-space INSIDE blocks.
func TestRenderStatusBar_NoDoubleSpaces(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*state)
	}{
		{"empty_scope", func(s *state) {
			s.health.Daemon = "ok"
			s.health.Workers = 2
		}},
		{"with_task_scope", func(s *state) {
			s.health.Daemon = "ok"
			s.scope.TaskID = "ask-aaaaaa"
		}},
		{"with_all_scopes", func(s *state) {
			s.health.Daemon = "ok"
			s.scope.TaskID = "ask-aaaaaa"
			s.scope.WorkflowName = "feature-dev"
		}},
		{"daemon_down", func(s *state) {
			s.health.Daemon = "down"
		}},
		{"daemon_stale", func(s *state) {
			s.health.Daemon = "stale"
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newState()
			c.setup(st)
			bar := renderStatusBar(st, "/proj")
			visible := ansiutil.Strip(bar)
			if strings.Contains(visible, "  ") {
				t.Errorf("status bar contains double space:\n%q", visible)
			}
			// Separator between non-empty blocks must be " | "
			// (single-space-pipe-single-space). Existence is
			// sufficient \u2014 the AC also pins "no `?=help`" which
			// the dedicated test below covers.
			if !strings.Contains(visible, " | ") {
				t.Errorf("status bar missing \" | \" separator:\n%q", visible)
			}
		})
	}
}

// TestRenderStatusBar_NoHelpHint pins AC4: the `?=help` block is
// removed from the status bar (it now lives on the options strip).
func TestRenderStatusBar_NoHelpHint(t *testing.T) {
	st := newState()
	st.health.Daemon = "ok"
	bar := renderStatusBar(st, "/proj")
	visible := ansiutil.Strip(bar)
	if strings.Contains(visible, "?=help") {
		t.Errorf("status bar still carries legacy \"?=help\" hint:\n%q", visible)
	}
}

// TestArrangement_StatusBarSize1 pins AC4: the dashboard
// allocates Size:1 to winStatusBar (a single painted row, no
// empty padding rows above or below).
func TestArrangement_StatusBarSize1(t *testing.T) {
	dims := arrange(arrangeArgs{width: 120, height: 40, focusedSide: winTasks})
	d, ok := dims[winStatusBar]
	if !ok {
		t.Fatalf("missing status bar")
	}
	h := d.Y1 - d.Y0 + 1
	if h != 1 {
		t.Errorf("status bar height %d, want 1", h)
	}
	// And the options strip is allocated, also Size:1.
	o, ok := dims[winOptionsStrip]
	if !ok {
		t.Fatalf("missing options strip")
	}
	oh := o.Y1 - o.Y0 + 1
	if oh != 1 {
		t.Errorf("options strip height %d, want 1", oh)
	}
	// Options strip sits BELOW the status bar (last row).
	if o.Y0 < d.Y0 {
		t.Errorf("options strip Y0=%d above status bar Y0=%d", o.Y0, d.Y0)
	}
}

// TestRenderOptionsStrip_FocusedPanelEntries pins AC3 on a per-focus
// basis: for each side panel and the Detail focus, at least one
// expected high-traffic binding appears in the options strip.
func TestRenderOptionsStrip_FocusedPanelEntries(t *testing.T) {
	gu := &Gui{st: newState()}
	specs := gu.bindingSpecs()
	cases := []struct {
		name        string
		focused     string
		wantContain []string
	}{
		{"tasks", winTasks, []string{"new", "edit", "done"}},
		{"sessions", winSessions, []string{"abort"}},
		// Workflows + Agents are READ-ONLY in v2: no panel write verbs, only
		// the global bindings appear in the strip.
		{"workflows", winWorkflows, []string{"quit", "help"}},
		{"agents", winAgents, []string{"quit", "help"}},
		{"detail", winDetail, []string{"help", "quit"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := renderOptionsStrip(specs, c.focused, 300)
			visible := ansiutil.Strip(out)
			for _, want := range c.wantContain {
				if !strings.Contains(visible, want) {
					t.Errorf("focused=%s strip missing %q\nstrip=%q", c.name, want, visible)
				}
			}
			// Strip must use " | " as the separator.
			if !strings.Contains(visible, " | ") {
				t.Errorf("focused=%s strip missing separator\nstrip=%q", c.name, visible)
			}
		})
	}
}

// TestRenderOptionsStrip_TruncatesWithEllipsis pins AC3: when the
// running width exceeds the strip's inner width, the rendering is
// truncated and " | \u2026" is appended.
func TestRenderOptionsStrip_TruncatesWithEllipsis(t *testing.T) {
	gu := &Gui{st: newState()}
	specs := gu.bindingSpecs()
	// Narrow strip \u2014 the Tasks bindings are wider than 30 cells.
	out := renderOptionsStrip(specs, winTasks, 30)
	visible := ansiutil.Strip(out)
	if !strings.Contains(visible, "\u2026") {
		t.Errorf("strip not truncated with \u2026 at narrow width:\n%q", visible)
	}
	// Truncation marker is preceded by the separator.
	if !strings.HasSuffix(visible, " | \u2026") && !strings.HasSuffix(visible, "\u2026") {
		t.Errorf("strip truncation marker malformed:\n%q", visible)
	}
}

// TestRenderOptionsStrip_LocalBeforeGlobal pins the ordering
// contract: Local entries come first, Global entries follow.
func TestRenderOptionsStrip_LocalBeforeGlobal(t *testing.T) {
	gu := &Gui{st: newState()}
	specs := gu.bindingSpecs()
	out := renderOptionsStrip(specs, winTasks, 500)
	visible := ansiutil.Strip(out)
	// "new" is a Tasks-Local short label; "help" is a Global short
	// label. The "new" entry's first occurrence must precede the
	// "help" entry's first occurrence.
	newIdx := strings.Index(visible, "new")
	helpIdx := strings.Index(visible, "help")
	if newIdx < 0 || helpIdx < 0 {
		t.Fatalf("expected both \"new\" and \"help\" in strip:\n%q", visible)
	}
	if newIdx > helpIdx {
		t.Errorf("Local entry \"new\" (at %d) must precede Global entry \"help\" (at %d):\n%q", newIdx, helpIdx, visible)
	}
}

// TestRenderOptionsStrip_NoLeadingSeparatorOnNarrow pins R3(a):
// when the very first entry overflows the available budget, the
// renderer does NOT emit a stray leading " | …" (a space-pipe-
// space-ellipsis with nothing to its left). Lazygit's
// formatBindingInfos gates the truncation path on i > 0; we
// follow the same rule so the first entry is always written
// verbatim and the strip never starts with whitespace.
func TestRenderOptionsStrip_NoLeadingSeparatorOnNarrow(t *testing.T) {
	gu := &Gui{st: newState()}
	specs := gu.bindingSpecs()
	// Sweep widths from 1..15 — these are narrow enough that the
	// first Tasks entry ("n: new") may or may not fit, but the
	// renderer must never produce " | …" as a prefix.
	for w := 1; w <= 15; w++ {
		out := renderOptionsStrip(specs, winTasks, w)
		visible := ansiutil.Strip(out)
		if strings.HasPrefix(visible, " | ") {
			t.Errorf("innerWidth=%d: strip starts with leading separator %q", w, visible)
		}
		if strings.HasPrefix(visible, " ") {
			t.Errorf("innerWidth=%d: strip has leading space %q", w, visible)
		}
	}
}

// TestRenderOptionsStrip_WidthWithinBudget pins R3(b): the
// rendered visible width never exceeds innerWidth across a sweep
// of realistic terminal widths. The previous implementation
// reserved only 2 cells of slack but actually wrote 4 (sep + … =
// " | …" = 4 visible cells), so the truncation marker itself was
// the first thing clipped on a precisely-sized terminal. The new
// budget reserves sepVisible + ellipsisVisible cells.
//
// Exception: when the strip fits entirely (no truncation) the
// content can be shorter than innerWidth — we only assert the
// upper bound. When truncation kicks in but the first entry
// alone already exceeds the budget, the first entry is still
// written verbatim (lazygit semantics), so allow up to the
// widest first-entry width plus the truncation suffix.
func TestRenderOptionsStrip_WidthWithinBudget(t *testing.T) {
	gu := &Gui{st: newState()}
	specs := gu.bindingSpecs()
	// Compute the widest first entry across focused panels so we
	// know the unavoidable lower bound when truncation kicks in.
	focuses := []string{winTasks, winSessions, winWorkflows, winAgents, winDetail}
	for _, focus := range focuses {
		for w := 30; w <= 120; w += 5 {
			out := renderOptionsStrip(specs, focus, w)
			visible := ansiutil.Strip(out)
			actualW := lipgloss.Width(out)
			hasEllipsis := strings.Contains(visible, "…")
			if !hasEllipsis {
				// No truncation: strict upper bound.
				if actualW > w {
					t.Errorf("focused=%s innerWidth=%d: visibleWidth=%d > innerWidth (no truncation)\nstrip=%q", focus, w, actualW, visible)
				}
			} else {
				// Truncated: strip must still be <= innerWidth
				// (subject to the unavoidable first-entry overflow
				// when even one entry doesn't fit — lazygit
				// semantics allow that; here innerWidth >= 30 is
				// well above any single Tasks/Jobs/Workflows/Agents
				// option, so we require strict <=).
				if actualW > w {
					t.Errorf("focused=%s innerWidth=%d: visibleWidth=%d > innerWidth (truncated)\nstrip=%q", focus, w, actualW, visible)
				}
			}
		}
	}
}

// TestBindKeys_AllSpecsRegister: bindKeys() must succeed even
// though we register many entries with overlapping (view, key)
// pairs across different gocui views. This is mostly a smoke
// check that no spec carries a malformed key.
func TestBindKeys_AllSpecsRegister(t *testing.T) {
	g, err := gocui.NewGui(gocui.NewGuiOpts{Headless: true, Width: 80, Height: 24})
	if err != nil {
		t.Fatalf("gocui init: %v", err)
	}
	defer g.Close()
	gu := &Gui{g: g, st: newState()}
	if err := gu.bindKeys(); err != nil {
		t.Fatalf("bindKeys: %v", err)
	}
}

// TestLayout_BottomStripsHaveRenderableRow is the regression test
// for the post-ask-ed8035 bug where both the status bar and the
// new options strip rendered blank: the Size:1 + Frame=false
// combo collapses gocui's InnerHeight to 0 (Height - 2 clamped to
// 0), so the view's draw loop iterates zero content rows.
//
// layout.go compensates with lazygit's frameOffset trick — when
// SetView is called for a Frame=false view it expands the
// rectangle outward by 1 cell on each side, so the writeable area
// lands exactly on the boxlayout dimensions. The check below
// captures the post-fix invariant: after one layout pass each
// strip must report InnerHeight >= 1 (room for the painted row)
// AND a buffer that actually carries content (renderViews ran
// and wrote into the view).
func TestLayout_BottomStripsHaveRenderableRow(t *testing.T) {
	g, err := gocui.NewGui(gocui.NewGuiOpts{Headless: true, Width: 120, Height: 40})
	if err != nil {
		t.Fatalf("gocui init: %v", err)
	}
	defer g.Close()
	gu := &Gui{g: g, st: newState()}
	if err := gu.layout(g); err != nil {
		t.Fatalf("layout: %v", err)
	}
	for _, name := range []string{winStatusBar, winOptionsStrip} {
		v, err := g.View(name)
		if err != nil {
			t.Errorf("%s: View missing after layout: %v", name, err)
			continue
		}
		_, ih := v.InnerSize()
		if ih < 1 {
			t.Errorf("%s: InnerHeight=%d, want >=1 (Frame=false view collapsed; layout missing frameOffset trick)", name, ih)
		}
		if v.Frame {
			t.Errorf("%s: Frame=true, want false", name)
		}
		if strings.TrimSpace(v.Buffer()) == "" {
			t.Errorf("%s: buffer empty after layout; renderViews did not write anything visible", name)
		}
	}
}

// TestLayout_BottomStripsPaintAtScreenBottom pins where the two
// strips actually land on screen: the status bar must paint its
// row at height-2 and the options strip its row at height-1, so
// they appear stacked at the very bottom with no blank padding
// row between them or below them. Computed from the post-fix
// SetView coordinates (expanded by 1 cell on each side) plus
// gocui's `screen_y = v.y0 + content_y + 1` translation: a Size:1
// Frame=false box at boxlayout y=Y renders its single content row
// at screen y=Y.
func TestLayout_BottomStripsPaintAtScreenBottom(t *testing.T) {
	const w, h = 120, 40
	g, err := gocui.NewGui(gocui.NewGuiOpts{Headless: true, Width: w, Height: h})
	if err != nil {
		t.Fatalf("gocui init: %v", err)
	}
	defer g.Close()
	gu := &Gui{g: g, st: newState()}
	if err := gu.layout(g); err != nil {
		t.Fatalf("layout: %v", err)
	}
	cases := []struct {
		name    string
		screenY int
	}{
		{winStatusBar, h - 2},
		{winOptionsStrip, h - 1},
	}
	for _, c := range cases {
		v, err := g.View(c.name)
		if err != nil {
			t.Errorf("%s: View missing: %v", c.name, err)
			continue
		}
		_, y0, _, y1 := v.Dimensions()
		// gocui paints Frame=false content at (v.y0+y+1); for the
		// row 0 of a single-row strip the paint y is v.y0+1.
		if paintY := y0 + 1; paintY != c.screenY {
			t.Errorf("%s: content row paints at screen y=%d, want %d (Dimensions y0=%d, y1=%d)", c.name, paintY, c.screenY, y0, y1)
		}
	}
}
