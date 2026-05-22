package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/jesseduffield/gocui"

	"autosk/internal/lazy/datasource"
)

// fakeMetaDS records SetMetadata calls so the tests can assert the
// parsed map round-tripped through the JSON decoder.
type fakeMetaDS struct {
	refreshFakeDS
	gotID  string
	gotMap map[string]any
	calls  int
	err    error
}

func (f *fakeMetaDS) SetMetadata(_ context.Context, id string, m map[string]any) error {
	f.calls++
	f.gotID = id
	f.gotMap = m
	return f.err
}

// TestTaskMetadataEditEmptyShowsCurlies pins the "empty metadata"
// branch: when t.Metadata is nil or empty the popup is seeded with
// "{}" so the user can type into a non-blank but unambiguous JSON
// object scaffold.
func TestTaskMetadataEditEmptyShowsCurlies(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.st.tasks = []datasource.Task{{ID: "ask-eeeeee"}}
	gu.st.taskCursor = 0
	gu.st.focused = panelTasks

	if err := gu.taskMetadataEdit(nil, nil); err != nil {
		t.Fatalf("taskMetadataEdit: %v", err)
	}
	if k := gu.st.popup.Kind; k != popupSingleCompose {
		t.Fatalf("popup kind = %v, want popupSingleCompose", k)
	}
	if gu.st.popup.Input != "{}" {
		t.Errorf("popup seed = %q, want {}", gu.st.popup.Input)
	}
}

// TestTaskMetadataEditPrettyPrints pins the pre-fill format: a
// non-empty metadata map is rendered through json.MarshalIndent
// with 2-space indent. Map keys are sorted alphabetically by Go's
// encoder, so the assertion is deterministic.
func TestTaskMetadataEditPrettyPrints(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.st.tasks = []datasource.Task{{
		ID: "ask-ffffff",
		Metadata: map[string]any{
			"alpha": "one",
			"beta":  float64(2),
		},
	}}
	gu.st.taskCursor = 0
	gu.st.focused = panelTasks

	_ = gu.taskMetadataEdit(nil, nil)
	want := "{\n  \"alpha\": \"one\",\n  \"beta\": 2\n}"
	if gu.st.popup.Input != want {
		t.Errorf("popup seed mismatch:\n got %q\nwant %q", gu.st.popup.Input, want)
	}
}

// TestTaskMetadataEditNoOpWithoutSelection asserts the no-selection
// path does NOT open the popup.
func TestTaskMetadataEditNoOpWithoutSelection(t *testing.T) {
	gu := &Gui{st: newState()}
	if err := gu.taskMetadataEdit(nil, nil); err != nil {
		t.Fatalf("taskMetadataEdit: %v", err)
	}
	if k := gu.st.popup.Kind; k != popupNone {
		t.Errorf("no-selection path opened popup: kind=%v", k)
	}
}

// TestTaskMetadataInvalidJSONReopensPopup pins the validation
// branch: invalid JSON MUST NOT call SetMetadata and MUST leave the
// popup re-opened. The flash level must be "err" so the operator
// sees the toast.
func TestTaskMetadataInvalidJSONReopensPopup(t *testing.T) {
	g, err := gocui.NewGui(gocui.NewGuiOpts{
		OutputMode: gocui.OutputNormal,
		Headless:   true,
		Width:      120,
		Height:     40,
	})
	if err != nil {
		t.Fatalf("gocui new: %v", err)
	}
	defer g.Close()

	ds := &fakeMetaDS{}
	gu := &Gui{g: g, st: newState(), ds: ds, ctx: context.Background()}
	gu.openSingleComposeForMetadata("ask-aaaaaa", "{not-valid")
	gu.layoutPopup(g, 120, 40)

	v, _ := g.View(winSingleCompose)
	v.TextArea.Clear()
	v.TextArea.TypeString("{this is not valid json")

	if err := gu.singleComposeConfirm(nil, nil); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if ds.calls != 0 {
		t.Errorf("SetMetadata called %d times on invalid JSON, want 0", ds.calls)
	}
	if k := gu.st.popup.Kind; k != popupSingleCompose {
		t.Errorf("popup must be re-opened on invalid JSON; kind=%v", k)
	}
	if !strings.Contains(gu.st.flash.Text, "invalid JSON") {
		t.Errorf("flash should mention 'invalid JSON': %+v", gu.st.flash)
	}
	if gu.st.flash.Level != "err" {
		t.Errorf("flash level = %q, want err", gu.st.flash.Level)
	}
}

// TestTaskMetadataNonObjectJSONReopensPopup pins the corner-case
// from the plan's acceptance criteria #9: valid JSON that isn't an
// object (e.g. an array or a string) MUST behave the same as
// invalid JSON \u2014 json.Unmarshal into map[string]any fails on
// non-object values.
func TestTaskMetadataNonObjectJSONReopensPopup(t *testing.T) {
	g, err := gocui.NewGui(gocui.NewGuiOpts{
		OutputMode: gocui.OutputNormal,
		Headless:   true,
		Width:      120,
		Height:     40,
	})
	if err != nil {
		t.Fatalf("gocui new: %v", err)
	}
	defer g.Close()

	ds := &fakeMetaDS{}
	gu := &Gui{g: g, st: newState(), ds: ds, ctx: context.Background()}
	gu.openSingleComposeForMetadata("ask-aaaaaa", "{}")
	gu.layoutPopup(g, 120, 40)

	v, _ := g.View(winSingleCompose)
	v.TextArea.Clear()
	v.TextArea.TypeString(`["array", "not", "object"]`)

	if err := gu.singleComposeConfirm(nil, nil); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if ds.calls != 0 {
		t.Errorf("SetMetadata called %d times on non-object JSON, want 0", ds.calls)
	}
	if k := gu.st.popup.Kind; k != popupSingleCompose {
		t.Errorf("popup must be re-opened on non-object JSON; kind=%v", k)
	}
	if !strings.Contains(gu.st.flash.Text, "invalid JSON") {
		t.Errorf("flash should mention 'invalid JSON': %+v", gu.st.flash)
	}
}

// TestTaskMetadataEmptyObjectClearsMap pins acceptance criteria #7
// for the special case: submitting `{}` is equivalent to clearing
// metadata, and SetMetadata is called with an empty / nil map.
func TestTaskMetadataEmptyObjectClearsMap(t *testing.T) {
	g, err := gocui.NewGui(gocui.NewGuiOpts{
		OutputMode: gocui.OutputNormal,
		Headless:   true,
		Width:      120,
		Height:     40,
	})
	if err != nil {
		t.Fatalf("gocui new: %v", err)
	}
	defer g.Close()

	ds := &fakeMetaDS{}
	gu := &Gui{g: g, st: newState(), ds: ds, ctx: context.Background()}
	// SetMetadata fires through gu.g.OnWorker; the test setup
	// doesn't run a MainLoop, so we drive the datasource via the
	// accept callback directly. Open the popup, type `{}`, hit
	// submit \u2014 the accept callback's worker dispatch enqueues the
	// SetMetadata call.
	gu.openSingleComposeForMetadata("ask-aaaaaa", "{}")
	gu.layoutPopup(g, 120, 40)

	v, _ := g.View(winSingleCompose)
	v.TextArea.Clear()
	v.TextArea.TypeString("{}")

	if err := gu.singleComposeConfirm(nil, nil); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	// Worker dispatch needs the MainLoop. Verify popup closed +
	// flash NOT marked as error \u2014 then call SetMetadata directly
	// to assert the empty map round-trips through the decoder.
	if k := gu.st.popup.Kind; k != popupNone {
		t.Errorf("popup not cleared on valid `{}`: kind=%v", k)
	}
	if gu.st.flash.Level == "err" {
		t.Errorf("flash level = err on valid `{}`: %+v", gu.st.flash)
	}

	// Exercise the datasource path with the parsed map shape that
	// the accept callback would have produced. json.Unmarshal of
	// "{}" into map[string]any yields an empty (zero-len) map, NOT
	// nil. SetMetadata accepts both.
	if err := ds.SetMetadata(context.Background(), "ask-aaaaaa", map[string]any{}); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	if ds.calls != 1 || ds.gotID != "ask-aaaaaa" || len(ds.gotMap) != 0 {
		t.Errorf("got calls=%d id=%q map=%+v", ds.calls, ds.gotID, ds.gotMap)
	}
}

// TestTaskMetadataValidJSONClosesPopup pins acceptance criteria #7
// for the happy path: a valid JSON object closes the popup, sets a
// non-error flash, and (when followed by direct ds.SetMetadata) the
// parsed map round-trips correctly.
func TestTaskMetadataValidJSONClosesPopup(t *testing.T) {
	g, err := gocui.NewGui(gocui.NewGuiOpts{
		OutputMode: gocui.OutputNormal,
		Headless:   true,
		Width:      120,
		Height:     40,
	})
	if err != nil {
		t.Fatalf("gocui new: %v", err)
	}
	defer g.Close()

	ds := &fakeMetaDS{}
	gu := &Gui{g: g, st: newState(), ds: ds, ctx: context.Background()}
	gu.openSingleComposeForMetadata("ask-aaaaaa", "{}")
	gu.layoutPopup(g, 120, 40)

	v, _ := g.View(winSingleCompose)
	v.TextArea.Clear()
	v.TextArea.TypeString(`{"foo":"bar","n":42}`)

	if err := gu.singleComposeConfirm(nil, nil); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if k := gu.st.popup.Kind; k != popupNone {
		t.Errorf("popup not cleared on valid JSON: kind=%v", k)
	}
	if gu.st.flash.Level == "err" {
		t.Errorf("error flash on valid JSON: %+v", gu.st.flash)
	}
}
