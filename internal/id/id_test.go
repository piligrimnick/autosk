package id_test

import (
	"errors"
	"testing"

	"autosk/internal/id"
)

func TestNew_ShapeAndPrefix(t *testing.T) {
	for i := 0; i < 50; i++ {
		got, err := id.New("as")
		if err != nil {
			t.Fatal(err)
		}
		if !id.Valid(got) {
			t.Fatalf("invalid id: %q", got)
		}
		if id.Prefix(got) != "as" {
			t.Fatalf("want prefix=as, got %q", id.Prefix(got))
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

func TestNewUnique_RetriesOnCollision(t *testing.T) {
	calls := 0
	exists := func(s string) (bool, error) {
		calls++
		// First two attempts collide, third is free.
		return calls < 3, nil
	}
	got, err := id.NewUnique("as", exists)
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
	_, err := id.NewUnique("as", exists)
	if !errors.Is(err, id.ErrExhausted) {
		t.Fatalf("want ErrExhausted, got %v", err)
	}
}

func TestValid(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"as-a1b2", true},
		{"as-0000", true},
		{"foo-ffff", true},
		{"as-A1B2", false}, // uppercase rejected
		{"as-12345", false},
		{"as-12", false},
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
