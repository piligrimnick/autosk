// glamour StyleConfig derivation from theme.Palette.
//
// We base the mapping on glamour's bundled TokyoNightStyleConfig (the
// hex colours match autosk's TokyoNight palette closely already) but
// rewrite every element slot to pull from theme.Active() instead so
// a future palette swap actually changes the markdown render. The
// invariant "glamour uses the same palette as the rest of the TUI"
// is asserted by style_test.go \u2014 a slot drifting back to a hardcoded
// hex would fail TestStyleConfig_FromPalette.
package markdown

import (
	"github.com/charmbracelet/glamour/ansi"

	"autosk/internal/lazy/theme"
)

// margin0 / indent2 are the only two pointer literals we hand glamour
// for layout shape. Hex-colour pointers are produced inline by
// stringPtr(theme.Palette.X.Hex) so a typo in a palette literal
// surfaces as a render-time miscolour, not a compile-time pointer to
// the wrong hex.
var (
	margin0      uint = 0
	indent2      uint = 2
	boldOn            = true
	italicOn          = true
	underlineOn       = true
	crossedOutOn      = true
)

// StyleConfig returns a glamour ansi.StyleConfig wired to the given
// palette. Pure function: the same palette produces an equal
// StyleConfig every call. Exported so tests in this package (and a
// future palette test in theme/) can pin the mapping without going
// through the cached TermRenderer.
//
// Element \u2192 slot mapping (see internal/lazy/theme for the slot
// rationale; the markdown render reuses the existing semantics rather
// than introducing new ones):
//
//	Heading / H1\u2026H6     \u2192 Header  (bold)
//	Emph                 \u2192 italic
//	Strong               \u2192 bold
//	Link / LinkText      \u2192 Accent  (underline)
//	Code (inline)        \u2192 Filter  (orange, the "string literal" hue \u2014
//	                                    matches Code's role as a literal
//	                                    inside running prose)
//	CodeBlock fallback   \u2192 Muted
//	CodeBlock chroma     \u2192 manual mapping below; uses Filter / Accent /
//	                       Scope / Warn so syntax-highlighted code stays
//	                       inside the same palette
//	BlockQuote bar       \u2192 Scope
//	List bullets         \u2192 default fg (per-item Color pulls Header
//	                       so a nested-list scan groups by indentation)
//	HorizontalRule       \u2192 Muted
//	Item enumeration     \u2192 Focus
func StyleConfig(p theme.Palette) ansi.StyleConfig {
	hexHeader := p.Header.Hex
	hexAccent := p.Accent.Hex
	hexMuted := p.Muted.Hex
	hexFilter := p.Filter.Hex
	hexScope := p.Scope.Hex
	hexWarn := p.Warn.Hex
	hexOK := p.OK.Hex
	hexErr := p.Err.Hex

	return ansi.StyleConfig{
		// Document margin is 0: the detail pane already has a frame
		// and a one-line section header above us. A 2-cell default
		// margin would push every paragraph two cells in from the
		// pane's left edge, making the rendered description visibly
		// out of column with the surrounding header rows.
		Document: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				// No BlockPrefix/Suffix: Render() trims them anyway,
				// but skipping them saves the newline write.
			},
			Margin: &margin0,
		},
		BlockQuote: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Color: stringPtr(hexScope),
			},
			Indent:      &indent2,
			IndentToken: stringPtr("\u2502 "),
		},
		Paragraph: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{},
		},
		List: ansi.StyleList{
			StyleBlock: ansi.StyleBlock{
				StylePrimitive: ansi.StylePrimitive{},
			},
			LevelIndent: 2,
		},

		// Headings: all six levels share the Header hue + bold. Glamour
		// also lets each level set a prefix ("# ", "## ", ...) so the
		// rendered heading still reads as a heading without colour.
		Heading: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				BlockSuffix: "\n",
				Color:       stringPtr(hexHeader),
				Bold:        &boldOn,
			},
		},
		H1: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "# ",
				Bold:   &boldOn,
			},
		},
		H2: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Prefix: "## "}},
		H3: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Prefix: "### "}},
		H4: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Prefix: "#### "}},
		H5: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Prefix: "##### "}},
		H6: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Prefix: "###### "}},

		Strikethrough: ansi.StylePrimitive{
			CrossedOut: &crossedOutOn,
		},
		Emph: ansi.StylePrimitive{
			Italic: &italicOn,
		},
		Strong: ansi.StylePrimitive{
			Bold: &boldOn,
		},
		HorizontalRule: ansi.StylePrimitive{
			Color:  stringPtr(hexMuted),
			Format: "\n\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\n",
		},
		Item: ansi.StylePrimitive{
			BlockPrefix: "\u2022 ",
		},
		Enumeration: ansi.StylePrimitive{
			BlockPrefix: ". ",
			Color:       stringPtr(hexHeader),
		},
		Task: ansi.StyleTask{
			StylePrimitive: ansi.StylePrimitive{},
			Ticked:         "[\u2713] ",
			Unticked:       "[ ] ",
		},
		Link: ansi.StylePrimitive{
			Color:     stringPtr(hexAccent),
			Underline: &underlineOn,
		},
		LinkText: ansi.StylePrimitive{
			Color: stringPtr(hexAccent),
		},
		Image: ansi.StylePrimitive{
			Color:     stringPtr(hexAccent),
			Underline: &underlineOn,
		},
		ImageText: ansi.StylePrimitive{
			Color:  stringPtr(hexAccent),
			Format: "Image: {{.text}} \u2192",
		},
		// Inline code: padded with a space on either side and tinted
		// in the Filter hue (orange) on no background. Glamour's
		// stock TokyoNight uses a background colour here too, but the
		// detail pane's frame is already a frame, and stacking a
		// per-glyph background inside it tends to bleed into the
		// pane's selection highlight when the cursor lands on the
		// row \u2014 cleaner to just colour the text.
		Code: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Color: stringPtr(hexFilter),
			},
		},
		CodeBlock: ansi.StyleCodeBlock{
			StyleBlock: ansi.StyleBlock{
				StylePrimitive: ansi.StylePrimitive{
					Color: stringPtr(hexMuted),
				},
				Margin: &indent2,
			},
			Chroma: chromaStyle(hexHeader, hexAccent, hexMuted, hexFilter, hexScope, hexWarn, hexOK, hexErr),
		},
		Table: ansi.StyleTable{
			StyleBlock: ansi.StyleBlock{
				StylePrimitive: ansi.StylePrimitive{},
			},
		},
		DefinitionDescription: ansi.StylePrimitive{
			BlockPrefix: "\n\u2192 ",
		},
	}
}

// chromaStyle returns the chroma-token colour map used for fenced
// code blocks. Mirrors glamour's bundled TokyoNightStyleConfig but
// every token category routes through one of our palette slots so a
// future palette swap actually recolours the syntax highlighting.
//
// The token \u2192 slot mapping leans on the natural semantic match
// between TokyoNight's chroma legend and our slot names:
//
//	Comment          \u2192 Muted    (TokyoNight's iconic comment grey)
//	Keyword*         \u2192 Scope    (cyan; "language keyword")
//	Name*            \u2192 Header   (blue; identifiers)
//	LiteralString    \u2192 Warn     (yellow; string literals)
//	LiteralNumber    \u2192 Filter   (orange; number literals)
//	NameAttribute    \u2192 OK       (green; tag attributes / decorators)
//	NameDecorator    \u2192 OK
//	NameFunction     \u2192 OK
//	NameConstant     \u2192 Accent   (magenta; constants pop)
//	GenericDeleted   \u2192 Err      (diff-deleted)
//	GenericInserted  \u2192 OK       (diff-inserted)
//	GenericSubheading \u2192 Accent
//	Background       \u2192 left unset \u2014 chroma's default no-background read is
//	                   better behaviour inside the detail-pane frame.
func chromaStyle(header, accent, muted, filter, scope, warn, ok, errH string) *ansi.Chroma {
	return &ansi.Chroma{
		Text: ansi.StylePrimitive{
			Color: stringPtr(header),
		},
		Error: ansi.StylePrimitive{
			Color: stringPtr(errH),
		},
		Comment: ansi.StylePrimitive{
			Color:  stringPtr(muted),
			Italic: &italicOn,
		},
		CommentPreproc: ansi.StylePrimitive{
			Color: stringPtr(scope),
		},
		Keyword: ansi.StylePrimitive{
			Color: stringPtr(scope),
		},
		KeywordReserved: ansi.StylePrimitive{
			Color: stringPtr(scope),
		},
		KeywordNamespace: ansi.StylePrimitive{
			Color: stringPtr(scope),
		},
		KeywordType: ansi.StylePrimitive{
			Color: stringPtr(header),
		},
		Operator: ansi.StylePrimitive{
			Color: stringPtr(scope),
		},
		Punctuation: ansi.StylePrimitive{
			Color: stringPtr(header),
		},
		Name: ansi.StylePrimitive{
			Color: stringPtr(header),
		},
		NameConstant: ansi.StylePrimitive{
			Color: stringPtr(accent),
		},
		NameBuiltin: ansi.StylePrimitive{
			Color: stringPtr(header),
		},
		NameTag: ansi.StylePrimitive{
			Color: stringPtr(scope),
		},
		NameAttribute: ansi.StylePrimitive{
			Color: stringPtr(ok),
		},
		NameClass: ansi.StylePrimitive{
			Color: stringPtr(header),
		},
		NameDecorator: ansi.StylePrimitive{
			Color: stringPtr(ok),
		},
		NameFunction: ansi.StylePrimitive{
			Color: stringPtr(ok),
		},
		LiteralNumber: ansi.StylePrimitive{
			Color: stringPtr(filter),
		},
		LiteralString: ansi.StylePrimitive{
			Color: stringPtr(warn),
		},
		LiteralStringEscape: ansi.StylePrimitive{
			Color: stringPtr(scope),
		},
		GenericDeleted: ansi.StylePrimitive{
			Color: stringPtr(errH),
		},
		GenericEmph: ansi.StylePrimitive{
			Italic: &italicOn,
		},
		GenericInserted: ansi.StylePrimitive{
			Color: stringPtr(ok),
		},
		GenericStrong: ansi.StylePrimitive{
			Bold: &boldOn,
		},
		GenericSubheading: ansi.StylePrimitive{
			Color: stringPtr(accent),
		},
	}
}

// stringPtr returns a pointer to s. Glamour's StyleConfig stores
// *string for nullable hex colours; we never mutate the target, but
// the pointer is the only shape the type accepts.
func stringPtr(s string) *string { return &s }
