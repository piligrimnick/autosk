package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"autosk/internal/daemon/api"
	"autosk/internal/lazy/ansiutil"
	"autosk/internal/lazy/datasource"
)

// jobDetailFixedTS gives every job-detail fixture a deterministic
// timestamp so timeformat output stays stable across CI clocks.
var jobDetailFixedTS = time.Date(2026, 5, 22, 14, 30, 0, 0, time.UTC)

// makeRunningJob constructs a minimal running-job fixture for the
// renderJobDetail tests.
func makeRunningJob(id string) datasource.Job {
	return datasource.Job{
		JobResponse: api.JobResponse{
			JobID:           id,
			TaskID:          "ask-aa1100",
			Status:          "running",
			CreatedAt:       jobDetailFixedTS,
			CorrectionsUsed: 1,
			MaxCorrections:  5,
			AttachCount:     2,
			Streaming:       true,
			SessionPath:     "/tmp/pi/session-" + id + ".jsonl",
		},
		WorkflowName: "feature-dev-generic",
		StepName:     "dev",
		AgentName:    "@autogent/generic",
	}
}

// TestRenderJobDetail_Header_FieldsPresent pins the header layout
// for a fully-populated job: the scan line carries the jobID, the
// status glyph, wf:step, and agent; the meta row has created and
// (when present) started/finished; the corrections+attached+pid row
// is there; the muted session row is there.
func TestRenderJobDetail_Header_FieldsPresent(t *testing.T) {
	j := makeRunningJob("job-aa11")
	started := jobDetailFixedTS.Add(5 * time.Minute)
	j.StartedAt = &started
	pid := 4242
	j.PID = &pid
	out := renderJobDetail(j, nil, 120)
	visible := ansiutil.Strip(out)

	for _, want := range []string{
		"job-aa11",
		"feature-dev-generic",
		"dev",
		"@autogent/generic",
		"created ",
		"started ",
		"attached 2",
		"corrections 1/5",
		"pid 4242",
		"session /tmp/pi/session-job-aa11.jsonl",
	} {
		if !strings.Contains(visible, want) {
			t.Errorf("missing %q in header output:\n%s", want, visible)
		}
	}

	// Empty fields are omitted: a job with no WorkflowName / AgentName
	// drops those tokens from the scan line.
	j2 := makeRunningJob("job-bb22")
	j2.WorkflowName = ""
	j2.AgentName = ""
	out2 := renderJobDetail(j2, nil, 120)
	visible2 := ansiutil.Strip(out2)
	if strings.Contains(visible2, "@autogent/generic") {
		t.Errorf("agent token leaked when AgentName==\"\": %q", visible2)
	}
	if strings.Contains(visible2, "feature-dev-generic") {
		t.Errorf("workflow token leaked when WorkflowName==\"\": %q", visible2)
	}
	// pid omitted when nil.
	if strings.Contains(visible2, "pid ") {
		t.Errorf("pid token leaked when PID==nil: %q", visible2)
	}
}

// TestRenderJobDetail_Header_NoTranscript pins the te==nil branch:
// the header still renders and the transcript section shows the
// (loading...) muted line.
func TestRenderJobDetail_Header_NoTranscript(t *testing.T) {
	j := makeRunningJob("job-cc33")
	out := renderJobDetail(j, nil, 120)
	visible := ansiutil.Strip(out)
	if !strings.Contains(visible, "(loading...)") {
		t.Errorf("missing (loading...) placeholder for nil transcript entry: %q", visible)
	}
}

// TestRenderJobDetail_Header_ArchiveError pins the err+empty branch:
// the archive-failed plashka shows in styleErr.
func TestRenderJobDetail_Header_ArchiveError(t *testing.T) {
	j := makeRunningJob("job-dd44")
	te := &jobTranscriptEntry{
		err:      errors.New("daemon: 503"),
		loadedAt: time.Now(),
	}
	out := renderJobDetail(j, te, 120)
	visible := ansiutil.Strip(out)
	if !strings.Contains(visible, "archive load failed") {
		t.Errorf("missing 'archive load failed' plashka: %q", visible)
	}
	if !strings.Contains(visible, "503") {
		t.Errorf("missing underlying error text: %q", visible)
	}
	// Must be painted in styleErr.
	if !strings.Contains(out, styleErr.Render("(archive load failed: daemon: 503)")) {
		t.Errorf("plashka not painted with styleErr (red): %q", out)
	}
}

// TestRenderJobDetail_EventBox_AssistantMarkdown: an assistant_text
// event runs through markdown.Render — we look for ANSI escapes in
// the body which markdown emits but a plain-text path does not.
func TestRenderJobDetail_EventBox_AssistantMarkdown(t *testing.T) {
	forceTrueColor(t)
	j := makeRunningJob("job-ee55")
	te := &jobTranscriptEntry{
		events: []datasource.MessageEvent{{
			Kind: "assistant_text",
			TS:   jobDetailFixedTS,
			Text: "# Title\n\n- one\n- two\n",
		}},
		loadedAt: time.Now(),
	}
	out := renderJobDetail(j, te, 120)
	visible := ansiutil.Strip(out)
	// The literal '# Title' (raw markdown) must NOT appear; glamour
	// strips the hash and styles the heading.
	if strings.Contains(out, "# Title\n") {
		t.Errorf("raw markdown leaked unstyled: %q", out)
	}
	if !strings.Contains(visible, "Title") {
		t.Errorf("heading payload missing: %q", visible)
	}
	// Glamour list bullet glyph proves the markdown path fired.
	if !strings.Contains(visible, "•") {
		t.Errorf("expected glamour bullet glyph in rendered list: %q", visible)
	}
}

// TestRenderJobDetail_EventBox_UserPlain: a user_text event preserves
// plain text without markdown styling on the body.
func TestRenderJobDetail_EventBox_UserPlain(t *testing.T) {
	forceTrueColor(t)
	j := makeRunningJob("job-ff66")
	te := &jobTranscriptEntry{
		events: []datasource.MessageEvent{{
			Kind: "user_text",
			TS:   jobDetailFixedTS,
			Text: "# Not a heading, just text",
		}},
		loadedAt: time.Now(),
	}
	out := renderJobDetail(j, te, 120)
	visible := ansiutil.Strip(out)
	// Plain text must survive verbatim (no glamour stripping).
	if !strings.Contains(visible, "# Not a heading, just text") {
		t.Errorf("plain text mangled: %q", visible)
	}
}

// TestRenderJobDetail_EventBox_EmptyBody: an event with Text=="" still
// produces a labeled box (just the frame + label, empty body).
func TestRenderJobDetail_EventBox_EmptyBody(t *testing.T) {
	j := makeRunningJob("job-gg77")
	te := &jobTranscriptEntry{
		events: []datasource.MessageEvent{{
			Kind: "session",
			TS:   jobDetailFixedTS,
			Text: "",
		}},
		loadedAt: time.Now(),
	}
	out := renderJobDetail(j, te, 120)
	visible := ansiutil.Strip(out)
	// The label appears on the top border of a labeled box.
	if !strings.Contains(visible, "session") {
		t.Errorf("label missing for empty-body event: %q", visible)
	}
	// Top and bottom box borders are both present.
	if !strings.Contains(visible, "╭") || !strings.Contains(visible, "╰") {
		t.Errorf("box frame missing for empty-body event: %q", visible)
	}
}

// TestRenderJobDetail_Truncated: te.truncated==true prepends the
// muted truncation note.
func TestRenderJobDetail_Truncated(t *testing.T) {
	j := makeRunningJob("job-hh88")
	te := &jobTranscriptEntry{
		events: []datasource.MessageEvent{{
			Kind: "user_text",
			Text: "x",
			TS:   jobDetailFixedTS,
		}},
		loadedAt:  time.Now(),
		truncated: true,
	}
	out := renderJobDetail(j, te, 120)
	visible := ansiutil.Strip(out)
	if !strings.Contains(visible, "transcript truncated") {
		t.Errorf("missing truncation note: %q", visible)
	}
}

// TestRenderDetail_PanelDetailFocus_KeepsJobBody pins the
// regression review flagged: when focused == panelDetail (via '0'
// or jobsEnter on a terminal job), renderDetail must keep emitting
// the Job Detail body — not fall through to "(nothing selected)".
//
// The mechanism is state.detailFocus: every transition INTO
// panelDetail stashes the previous focus, and renderDetail consults
// that field whenever focused reads panelDetail.
func TestRenderDetail_PanelDetailFocus_KeepsJobBody(t *testing.T) {
	s := newState()
	s.jobs = []datasource.Job{makeRunningJob("job-panel-det")}
	s.jobCursor = 0
	s.focused = panelJobs

	// Snapshot the body with focus on Jobs (baseline).
	before := renderDetail(s, 100)
	if !strings.Contains(ansiutil.Strip(before), "job-panel-det") {
		t.Fatalf("baseline panelJobs body missing jobID: %q", before)
	}

	// Simulate focus moving to Detail via jobsEnter (terminal-job
	// branch). The state field detailFocus must carry the source
	// panel so renderDetail can keep emitting the same body.
	s.detailFocus = panelJobs
	s.focused = panelDetail
	after := renderDetail(s, 100)
	if !strings.Contains(ansiutil.Strip(after), "job-panel-det") {
		t.Errorf("panelDetail focus dropped Job body: %q", after)
	}
	if strings.Contains(after, "(nothing selected)") {
		t.Errorf("panelDetail focus fell through to (nothing selected) plashka")
	}
}

// TestRenderJobDetail_ZeroWidth: width==0 falls back to a plain-text
// dump without panicking. Both te==nil and te-with-events branches
// are exercised (the latter routes through renderTranscript, not
// the per-event boxed renderer).
func TestRenderJobDetail_ZeroWidth(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("renderJobDetail panicked at width=0: %v", r)
		}
	}()
	j := makeRunningJob("job-ii99")

	// Branch 1: te == nil → (loading...) placeholder.
	out := renderJobDetail(j, nil, 0)
	if !strings.Contains(ansiutil.Strip(out), "job-ii99") {
		t.Errorf("zero-width nil-te output missing jobID header: %q", out)
	}
	if !strings.Contains(ansiutil.Strip(out), "(loading...)") {
		t.Errorf("zero-width nil-te output missing (loading...): %q", out)
	}

	// Branch 2: te with events → plain-text dump via renderTranscript.
	te := &jobTranscriptEntry{
		events: []datasource.MessageEvent{
			{Kind: "user_text", TS: jobDetailFixedTS, Text: "hello-world-payload"},
			{Kind: "assistant_text", TS: jobDetailFixedTS, Text: "acknowledged-reply"},
		},
		loadedAt: time.Now(),
	}
	out2 := renderJobDetail(j, te, 0)
	visible2 := ansiutil.Strip(out2)
	if !strings.Contains(visible2, "job-ii99") {
		t.Errorf("zero-width te-events output missing jobID header: %q", visible2)
	}
	if !strings.Contains(visible2, "hello-world-payload") {
		t.Errorf("zero-width te-events output missing first event body: %q", visible2)
	}
	if !strings.Contains(visible2, "acknowledged-reply") {
		t.Errorf("zero-width te-events output missing second event body: %q", visible2)
	}
	// The plain-text fallback must NOT route through drawLabeledBox.
	if strings.Contains(out2, "╭") {
		t.Errorf("zero-width fallback emitted a labeled box (rounded-corner rune): %q", out2)
	}
}
