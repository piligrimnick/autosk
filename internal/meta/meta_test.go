package meta_test

import (
	"encoding/json"
	"testing"

	"autosk/internal/meta"
)

func TestGetStepVisits_JSONRoundTrip(t *testing.T) {
	// json.Unmarshal widens int → float64. GetStepVisits must tolerate
	// that so the engine can read counters written by an earlier process.
	body := []byte(`{"step_visits": {"st-a": 3, "st-b": 1}}`)
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	sv := meta.GetStepVisits(m)
	if sv["st-a"] != 3 || sv["st-b"] != 1 {
		t.Fatalf("round-trip lost counters: %+v", sv)
	}
}

func TestGetStepVisits_NilOrMissing(t *testing.T) {
	if got := meta.GetStepVisits(nil); len(got) != 0 {
		t.Errorf("nil map: %+v", got)
	}
	if got := meta.GetStepVisits(map[string]any{}); len(got) != 0 {
		t.Errorf("empty map: %+v", got)
	}
}

func TestGetStepVisits_MalformedValueDropped(t *testing.T) {
	m := map[string]any{
		meta.StepVisitsKey: map[string]any{
			"st-a": 2.0,
			"st-b": "not a number",
			"st-c": []any{1, 2},
		},
	}
	sv := meta.GetStepVisits(m)
	if sv["st-a"] != 2 {
		t.Errorf("good entry lost: %+v", sv)
	}
	if _, ok := sv["st-b"]; ok {
		t.Errorf("string leaf should be dropped")
	}
	if _, ok := sv["st-c"]; ok {
		t.Errorf("array leaf should be dropped")
	}
}

func TestSetStepVisits_EmptyDeletesKey(t *testing.T) {
	m := map[string]any{"step_visits": map[string]any{"st-x": 1.0}}
	meta.SetStepVisits(m, meta.StepVisits{})
	if _, ok := m[meta.StepVisitsKey]; ok {
		t.Fatal("empty StepVisits should delete the reserved key")
	}
}

func TestMutateStepVisits_BumpAndPersist(t *testing.T) {
	m := map[string]any{}
	meta.MutateStepVisits(m, func(sv meta.StepVisits) {
		sv["st-a"]++
		sv["st-a"]++
	})
	got := meta.GetStepVisits(m)
	if got["st-a"] != 2 {
		t.Fatalf("st-a = %d (want 2); m=%+v", got["st-a"], m)
	}
	// Round-trip through JSON should preserve int values too.
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var re map[string]any
	if err := json.Unmarshal(body, &re); err != nil {
		t.Fatal(err)
	}
	if meta.GetStepVisits(re)["st-a"] != 2 {
		t.Fatalf("round-trip lost int: %s", body)
	}
}

func TestMutateStepVisits_EmptyAfterMutation(t *testing.T) {
	m := map[string]any{"step_visits": map[string]any{"st-a": 1.0}}
	meta.MutateStepVisits(m, func(sv meta.StepVisits) { delete(sv, "st-a") })
	if _, ok := m[meta.StepVisitsKey]; ok {
		t.Fatal("empty after mutation should delete the reserved key")
	}
}

func TestValidateStepVisitsLeaf(t *testing.T) {
	cases := []struct {
		name    string
		in      any
		wantErr bool
	}{
		{"int", 3, false},
		{"int64", int64(5), false},
		{"float64-int", 4.0, false},
		{"float64-fractional", 1.5, true},
		{"negative", -1, true},
		{"string", "no", true},
		{"slice", []any{1}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := meta.ValidateStepVisitsLeaf(c.in)
			if c.wantErr && err == nil {
				t.Fatalf("want error, got nil for %v", c.in)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("unexpected error %v for %v", err, c.in)
			}
		})
	}
}

func TestValidateStepVisitsObject(t *testing.T) {
	ok := map[string]any{"st-a": 1, "st-b": 2.0}
	if err := meta.ValidateStepVisitsObject(ok); err != nil {
		t.Fatalf("happy path: %v", err)
	}
	bad := map[string]any{"st-a": "x"}
	if err := meta.ValidateStepVisitsObject(bad); err == nil {
		t.Fatal("want error for string leaf")
	}
	if err := meta.ValidateStepVisitsObject("not-an-object"); err == nil {
		t.Fatal("want error for non-object")
	}
	if err := meta.ValidateStepVisitsObject(nil); err != nil {
		t.Fatalf("nil should be ok: %v", err)
	}
}
