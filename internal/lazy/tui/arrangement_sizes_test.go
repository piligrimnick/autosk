package tui

import "testing"

// TestDashboardArrangement_Sizes pins the layout at the four
// representative terminal sizes called out in the impl plan §8:
// minimum-supported 80x24, mid 120x40, large 200x60, narrow portrait
// 60x80. Failure modes we want to catch: views with negative
// dimensions, the focused side panel collapsing to 0 height in the
// portrait case, the status bar getting squeezed out at the minimum.
func TestDashboardArrangement_Sizes(t *testing.T) {
	cases := []struct {
		name string
		w, h int
	}{
		{"min_80x24", 80, 24},
		{"mid_120x40", 120, 40},
		{"large_200x60", 200, 60},
		{"portrait_60x80", 60, 80},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dims := arrange(arrangeArgs{
				width:       tc.w,
				height:      tc.h,
				focusedSide: winTasks,
				state:       StateDashboard,
			})
			required := []string{winTasks, winSessions, winWorkflows, winAgents, winDetail, winStatusBar, winOptionsStrip}
			for _, w := range required {
				d, ok := dims[w]
				if !ok {
					t.Errorf("%s: missing window %q", tc.name, w)
					continue
				}
				if d.X1 < d.X0 || d.Y1 < d.Y0 {
					t.Errorf("%s: %s has negative dimensions %+v", tc.name, w, d)
				}
			}
			// The focused side panel must be tall enough to show at
			// least one row. The accordion pattern pinches the
			// unfocused panels, not the focused one.
			if tasks, ok := dims[winTasks]; ok {
				if h := tasks.Y1 - tasks.Y0; h < 2 {
					t.Errorf("%s: focused side panel height %d < 2 (no room)", tc.name, h)
				}
			}
		})
	}
}

// (The previous TestDashboardArrangement_Sizes_WithJobInput was
// retired alongside the boxlayout-based winJobInput allocation:
// the input view no longer participates in the box tree at all.
// Its overlay-on-detail positioning is exercised at the layout
// level in job_detail_layout_test.go.)
