package id_test

import (
	"errors"
	"testing"

	"autosk/internal/id"
)

func TestNew_ShapeAndPrefix(t *testing.T) {
	for i := 0; i < 50; i++ {
		got, err := id.New("ask")
		if err != nil {
			t.Fatal(err)
		}
		if !id.Valid(got) {
			t.Fatalf("invalid id: %q", got)
		}
		if id.Prefix(got) != "ask" {
			t.Fatalf("want prefix=ask, got %q", id.Prefix(got))
		}
		// New defaults: prefix(3) + dash(1) + hex(6) = 10 chars.
		if len(got) != 10 {
			t.Fatalf("want len(id)=10, got %d for %q", len(got), got)
		}
	}
}

func TestNew_CustomPrefix(t *testing.T) {
	got, err := id.New("tk")
	if err != nil {
		t.Fatal(err)
	}
	if !id.Valid(got) {
		t.Fatalf("invalid id: %q", got)
	}
	if id.Prefix(got) != "tk" {
		t.Fatalf("want prefix=tk, got %q", id.Prefix(got))
	}
}

// TestNew_DefaultPrefixIsAsk pins the v0.2 default: id.New("") and
// id.NewUnique("") mint task-id-shaped values (`ask-` + 6 hex). The
// constants flip in this change set; this test catches an accidental
// revert that would otherwise only surface much later (in any place
// that minted ids without passing an explicit prefix).
func TestNew_DefaultPrefixIsAsk(t *testing.T) {
	got, err := id.New("")
	if err != nil {
		t.Fatal(err)
	}
	if id.Prefix(got) != "ask" {
		t.Fatalf("default prefix: got %q want ask", id.Prefix(got))
	}
	if len(got) != 10 {
		t.Fatalf("default length: got %d want 10 (got id=%q)", len(got), got)
	}
}

func TestNewUnique_RetriesOnCollision(t *testing.T) {
	calls := 0
	exists := func(s string) (bool, error) {
		calls++
		// First two attempts collide, third is free.
		return calls < 3, nil
	}
	got, err := id.NewUnique("ask", exists)
	if err != nil {
		t.Fatal(err)
	}
	if !id.Valid(got) {
		t.Fatalf("invalid id: %q", got)
	}
	if calls != 3 {
		t.Fatalf("want 3 attempts, got %d", calls)
	}
}

func TestNewUnique_Exhausts(t *testing.T) {
	exists := func(string) (bool, error) { return true, nil }
	_, err := id.NewUnique("ask", exists)
	if !errors.Is(err, id.ErrExhausted) {
		t.Fatalf("want ErrExhausted, got %v", err)
	}
}

func TestValid(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// New default shape: 3 bytes / 6 hex chars.
		{"ask-a1b2c3", true},
		{"ask-000000", true},
		{"ask-ffffff", true},
		// Legacy 4-hex shape stays valid — agent ids (`ag-XXXX`)
		// and any pre-007 task strings that linger in external
		// scripts / logs still parse through Valid().
		{"as-a1b2", true},
		{"ag-a1b2", true},
		{"foo-ffff", true},
		// 8 hex chars is also fine — pattern is open-ended (≥4, even).
		{"job-a1b2c3", true},
		{"job-a1b2c3d4", true},
		// Negatives.
		{"ask-A1B2C3", false}, // uppercase rejected
		{"ask-12345", false},  // odd hex length
		{"as-A1B2", false},    // uppercase rejected
		{"as-12345", false},   // odd hex length
		{"as-12", false},      // too short
		{"-a1b2", false},
		{"as", false},
		{"", false},
	}
	for _, c := range cases {
		if got := id.Valid(c.in); got != c.want {
			t.Errorf("Valid(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
