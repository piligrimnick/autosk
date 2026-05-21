package markdown

import (
	"testing"

	"autosk/internal/lazy/theme"
)

// TestStyleConfig_FromPalette pins the invariant from the task plan:
// markdown element colours come from the active palette, not from a
// hardcoded constant. A regression here would mean a future palette
// swap fails to recolour the markdown render.
func TestStyleConfig_FromPalette(t *testing.T) {
	p := theme.TokyoNight()
	cfg := StyleConfig(p)

	// Heading slot \u2192 Header hue.
	if cfg.Heading.Color == nil {
		t.Fatal("Heading.Color is nil; expected palette Header hex")
	}
	if got := *cfg.Heading.Color; got != p.Header.Hex {
		t.Errorf("Heading.Color = %q, want %q (palette Header)", got, p.Header.Hex)
	}

	// Link slot \u2192 Accent hue with underline.
	if cfg.Link.Color == nil || *cfg.Link.Color != p.Accent.Hex {
		t.Errorf("Link.Color mismatch; want %q", p.Accent.Hex)
	}
	if cfg.Link.Underline == nil || !*cfg.Link.Underline {
		t.Errorf("Link.Underline expected true")
	}

	// Inline Code slot \u2192 Filter hue.
	if cfg.Code.Color == nil || *cfg.Code.Color != p.Filter.Hex {
		t.Errorf("Code.Color = %v, want %q", cfg.Code.Color, p.Filter.Hex)
	}

	// BlockQuote bar \u2192 Scope hue.
	if cfg.BlockQuote.Color == nil || *cfg.BlockQuote.Color != p.Scope.Hex {
		t.Errorf("BlockQuote.Color = %v, want %q", cfg.BlockQuote.Color, p.Scope.Hex)
	}

	// HorizontalRule + CodeBlock fallback \u2192 Muted hue.
	if cfg.HorizontalRule.Color == nil || *cfg.HorizontalRule.Color != p.Muted.Hex {
		t.Errorf("HorizontalRule.Color = %v, want %q", cfg.HorizontalRule.Color, p.Muted.Hex)
	}
	if cfg.CodeBlock.Color == nil || *cfg.CodeBlock.Color != p.Muted.Hex {
		t.Errorf("CodeBlock.Color = %v, want %q", cfg.CodeBlock.Color, p.Muted.Hex)
	}

	// Document margin must be 0 \u2014 the detail pane already has frame
	// + section header above, a 2-cell margin would push the body
	// out of column.
	if cfg.Document.Margin == nil || *cfg.Document.Margin != 0 {
		t.Errorf("Document.Margin should be 0, got %v", cfg.Document.Margin)
	}
}

// TestStyleConfig_ChromaUsesPalette pins the chroma token map: code
// keywords use the Scope hue, strings use Warn, comments use Muted.
// A regression here would mean fenced code stops following the
// palette swap.
func TestStyleConfig_ChromaUsesPalette(t *testing.T) {
	p := theme.TokyoNight()
	cfg := StyleConfig(p)
	if cfg.CodeBlock.Chroma == nil {
		t.Fatal("CodeBlock.Chroma is nil")
	}
	cases := []struct {
		name string
		got  *string
		want string
	}{
		{"Comment", cfg.CodeBlock.Chroma.Comment.Color, p.Muted.Hex},
		{"Keyword", cfg.CodeBlock.Chroma.Keyword.Color, p.Scope.Hex},
		{"LiteralString", cfg.CodeBlock.Chroma.LiteralString.Color, p.Warn.Hex},
		{"LiteralNumber", cfg.CodeBlock.Chroma.LiteralNumber.Color, p.Filter.Hex},
		{"NameFunction", cfg.CodeBlock.Chroma.NameFunction.Color, p.OK.Hex},
		{"GenericDeleted", cfg.CodeBlock.Chroma.GenericDeleted.Color, p.Err.Hex},
		{"GenericInserted", cfg.CodeBlock.Chroma.GenericInserted.Color, p.OK.Hex},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got == nil {
				t.Fatalf("%s.Color is nil; expected %q", tc.name, tc.want)
			}
			if *tc.got != tc.want {
				t.Errorf("%s.Color = %q, want %q", tc.name, *tc.got, tc.want)
			}
		})
	}
}

// TestStyleConfig_PaletteSwap: building a StyleConfig against a
// modified palette produces a config with the modified colours. This
// is the smoke test for "a future palette swap actually recolours
// the markdown render".
func TestStyleConfig_PaletteSwap(t *testing.T) {
	p := theme.TokyoNight()
	p.Header = theme.RGB("#deadbe")
	p.Accent = theme.RGB("#cafe00")
	cfg := StyleConfig(p)

	if got := *cfg.Heading.Color; got != "#deadbe" {
		t.Errorf("Heading.Color after swap = %q, want #deadbe", got)
	}
	if got := *cfg.Link.Color; got != "#cafe00" {
		t.Errorf("Link.Color after swap = %q, want #cafe00", got)
	}
}
