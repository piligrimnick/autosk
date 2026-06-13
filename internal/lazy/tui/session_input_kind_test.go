package tui

import (
	"context"
	"testing"
	"time"

	"github.com/jesseduffield/gocui"

	"autosk/internal/lazy/datasource"
)

// recordingInputDS embeds refreshFakeDS (for the rest of the Datasource
// surface) and records the kind argument SessionInput is dispatched
// with, so a test can assert the TUI sends a value the v2 daemon
// accepts.
type recordingInputDS struct {
	*refreshFakeDS
	gotKind chan string
}

func (d *recordingInputDS) SessionInput(_ context.Context, _, _, kind string) error {
	d.gotKind <- kind
	return nil
}

// TestLiveDispatch_KindMatchesDaemonContract pins the BLOCKING regression
// review flagged: the Sessions detail pane's Ctrl-D (send/steer) and Ctrl-F
// (follow-up) must dispatch session.input with a kind the v2 daemon accepts.
// The daemon (daemon/core/src/rpc/daemon.ts) rejects anything that is not
// exactly "steer" or "followup" with INVALID_PARAMS, so the v1-era values
// "" and "follow_up" made both keys non-functional. The fake datasource
// records the kind WITHOUT validating it, which is exactly how the leak
// slipped through review the first time — this test closes that gap.
func TestLiveDispatch_KindMatchesDaemonContract(t *testing.T) {
	// daemonAcceptedKinds mirrors daemon/core/src/rpc/daemon.ts's strict
	// validation. Keep in sync with SessionInputParams on the wire.
	daemonAcceptedKinds := map[string]bool{"steer": true, "followup": true}

	cases := []struct {
		name string
		send func(gu *Gui, v *gocui.View) error
		want string
	}{
		{"ctrl-d-send", func(gu *Gui, v *gocui.View) error { return gu.liveSend(nil, v) }, "steer"},
		{"ctrl-f-followup", func(gu *Gui, v *gocui.View) error { return gu.liveFollowUp(nil, v) }, "followup"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gu := newHeadlessGui(t, 80, 24)
			ds := &recordingInputDS{refreshFakeDS: &refreshFakeDS{}, gotKind: make(chan string, 1)}
			gu.ds = ds
			gu.ctx = context.Background()

			// liveDispatch resolves the target from sessionInputOwner first.
			gu.st.sessions = []datasource.Session{{ID: "se-1"}}
			gu.st.sessionCursor = 0
			gu.st.sessionInputOwner = "se-1"

			v, err := gu.g.SetView(winSessionInput, 0, 0, 40, 5, 0)
			if err != nil && !isUnknownView(err) {
				t.Fatalf("SetView: %v", err)
			}
			if v == nil {
				t.Fatal("SetView returned nil view")
			}
			if _, err := v.Write([]byte("a message")); err != nil {
				t.Fatalf("seed buffer: %v", err)
			}

			if err := tc.send(gu, v); err != nil {
				t.Fatalf("dispatch: %v", err)
			}

			select {
			case got := <-ds.gotKind:
				if got != tc.want {
					t.Errorf("dispatched kind = %q, want %q", got, tc.want)
				}
				if !daemonAcceptedKinds[got] {
					t.Errorf("dispatched kind %q is not in the daemon's accepted set {steer, followup} — the v2 daemon rejects it with INVALID_PARAMS", got)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("SessionInput was never dispatched")
			}
		})
	}
}
