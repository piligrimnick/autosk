package theme

// TokyoNight is the canonical Tokyo Night (Night variant) palette, the
// same hex values enkia/tokyo-night-vscode-theme exports. Mapping into
// our semantic slots:
//
//	blue    #7aa2f7 → Focus + Header (focused frame/title, bold section heads)
//	magenta #bb9af7 → Accent          (cursor row, active tab, info flash, ▶)
//	purple  #9d7cd8 → PopupBox        (popup frame, distinct from Accent so
//	                                   the popup chrome isn't confused with
//	                                   the cursor row)
//	cyan    #7dcfff → Scope           (scope chip — TN cyan is iconic enough
//	                                   that a glance at the status bar is
//	                                   visually grounded in the theme)
//	orange  #ff9e64 → Filter          (input modality; TN paints string
//	                                   literals in this colour, which carries
//	                                   the "you're typing here" affordance)
//	yellow  #e0af68 → Warn            (state, not modality)
//	red     #f7768e → Err             (errors and daemon=down)
//	green   #9ece6a → OK              (daemon=ok, *live*, streaming)
//	comment #565f89 → Muted           (TN's iconic comment shade — purple-grey,
//	                                   noticeably more readable on a dark
//	                                   background than ANSI bright-black 8)
func TokyoNight() Palette {
	return Palette{
		Name:     "tokyo-night",
		Focus:    RGB("#7aa2f7"),
		PopupBox: RGB("#9d7cd8"),
		Header:   RGB("#7aa2f7"),
		Muted:    RGB("#565f89"),
		Accent:   RGB("#bb9af7"),
		Warn:     RGB("#e0af68"),
		Err:      RGB("#f7768e"),
		OK:       RGB("#9ece6a"),
		Scope:    RGB("#7dcfff"),
		Filter:   RGB("#ff9e64"),
	}
}
