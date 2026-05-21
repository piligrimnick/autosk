// Package timeformat centralises how autosk renders time values to a
// human operator. Anything user-facing (CLI text output, TUI panes,
// daemon-list output, flash toasts, command log) MUST format through
// these helpers so the project speaks one timezone and one set of
// layouts.
//
// Machine wire formats (JSON API responses, daemon HTTP API,
// RunContextSeed for agents, comment RenderForPrompt for LLM agents,
// TS extension types) stay on RFC3339 UTC and do NOT route through
// this package — see internal/comments/store.go and
// internal/daemon/api/types.go.
//
// Layouts:
//
//	Date     → 2006-01-02
//	Time     → 15:04:05
//	DateTime → 2006-01-02 15:04:05
//
// All Format* helpers convert their input to the operator's local
// timezone (time.Local) and return an empty string for the zero
// time.Time, so callers can pass through-the-DB nullable timestamps
// without an extra IsZero guard.
package timeformat

import "time"

// Layout constants exported for callers that need to feed them to
// time.Format directly (e.g. when building a composite string).
const (
	Date     = "2006-01-02"
	Time     = "15:04:05"
	DateTime = "2006-01-02 15:04:05"
)

// FormatDate renders t in the operator's local timezone as YYYY-MM-DD.
// A zero t returns the empty string.
func FormatDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(time.Local).Format(Date)
}

// FormatTime renders t in the operator's local timezone as HH:MM:SS.
// A zero t returns the empty string.
func FormatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(time.Local).Format(Time)
}

// FormatDateTime renders t in the operator's local timezone as
// "YYYY-MM-DD HH:MM:SS". A zero t returns the empty string.
func FormatDateTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(time.Local).Format(DateTime)
}

// FormatDateTimeSmart is the timeline-friendly form: it returns just
// the local Time (HH:MM:SS) when t falls on the operator's local
// "today", and the full local DateTime ("YYYY-MM-DD HH:MM:SS")
// otherwise. The intent is to keep the common case (today's events in
// a TUI timeline) compact while still carrying enough date for older
// events to be unambiguous.
//
// A zero t returns the empty string.
//
// "Today" is decided by time.Now() in time.Local. Callers that need
// deterministic output (tests) should use FormatDateTimeSmartAt.
func FormatDateTimeSmart(t time.Time) string {
	return FormatDateTimeSmartAt(t, time.Now())
}

// FormatDateTimeSmartAt is FormatDateTimeSmart with an injectable
// "now" reference, so tests can pin a known wall clock and still
// exercise the today/not-today branch deterministically.
//
// Both t and now are converted to time.Local before the Y-M-D
// comparison, so the boundary is the operator's midnight, not UTC.
func FormatDateTimeSmartAt(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	lt := t.In(time.Local)
	ln := now.In(time.Local)
	if sameLocalDay(lt, ln) {
		return lt.Format(Time)
	}
	return lt.Format(DateTime)
}

// sameLocalDay compares the Y-M-D parts of two times that are already
// in the same location. Callers must pass times already in time.Local
// (or any common location).
func sameLocalDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}
