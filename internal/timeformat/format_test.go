package timeformat

import (
	"testing"
	"time"
)

// withLocal temporarily swaps time.Local for the duration of a sub-
// test so we can exercise the "local TZ" branches without depending
// on the host's wall-clock zone.
func withLocal(t *testing.T, loc *time.Location) {
	t.Helper()
	orig := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = orig })
}

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("tzdata %q not available: %v", name, err)
	}
	return loc
}

func TestFormatDate_Zero(t *testing.T) {
	if got := FormatDate(time.Time{}); got != "" {
		t.Fatalf("FormatDate(zero) = %q, want \"\"", got)
	}
}

func TestFormatTime_Zero(t *testing.T) {
	if got := FormatTime(time.Time{}); got != "" {
		t.Fatalf("FormatTime(zero) = %q, want \"\"", got)
	}
}

func TestFormatDateTime_Zero(t *testing.T) {
	if got := FormatDateTime(time.Time{}); got != "" {
		t.Fatalf("FormatDateTime(zero) = %q, want \"\"", got)
	}
}

func TestFormatDateTimeSmart_Zero(t *testing.T) {
	if got := FormatDateTimeSmart(time.Time{}); got != "" {
		t.Fatalf("FormatDateTimeSmart(zero) = %q, want \"\"", got)
	}
	if got := FormatDateTimeSmartAt(time.Time{}, time.Now()); got != "" {
		t.Fatalf("FormatDateTimeSmartAt(zero,_) = %q, want \"\"", got)
	}
}

func TestFormatDateTime_UTCToLocal(t *testing.T) {
	// Moscow is UTC+3 year-round (no DST since 2014), so the math
	// is stable regardless of the calendar date the test runs on.
	moscow := mustLoad(t, "Europe/Moscow")
	withLocal(t, moscow)

	in := time.Date(2026, 5, 21, 11, 5, 12, 0, time.UTC)
	if got, want := FormatDateTime(in), "2026-05-21 14:05:12"; got != want {
		t.Errorf("FormatDateTime(UTC→Moscow) = %q, want %q", got, want)
	}
	if got, want := FormatDate(in), "2026-05-21"; got != want {
		t.Errorf("FormatDate(UTC→Moscow) = %q, want %q", got, want)
	}
	if got, want := FormatTime(in), "14:05:12"; got != want {
		t.Errorf("FormatTime(UTC→Moscow) = %q, want %q", got, want)
	}
}

func TestFormatDateTime_DateBoundary_PushesDayForward(t *testing.T) {
	// 23:30 UTC + 3h = 02:30 of the next local day. Make sure the
	// local-TZ conversion is reflected in the date portion too.
	moscow := mustLoad(t, "Europe/Moscow")
	withLocal(t, moscow)

	in := time.Date(2026, 5, 21, 23, 30, 0, 0, time.UTC)
	if got, want := FormatDate(in), "2026-05-22"; got != want {
		t.Errorf("FormatDate boundary = %q, want %q", got, want)
	}
	if got, want := FormatDateTime(in), "2026-05-22 02:30:00"; got != want {
		t.Errorf("FormatDateTime boundary = %q, want %q", got, want)
	}
}

func TestFormatDateTimeSmart_TodayUsesTimeOnly(t *testing.T) {
	moscow := mustLoad(t, "Europe/Moscow")
	withLocal(t, moscow)

	now := time.Date(2026, 5, 21, 14, 0, 0, 0, moscow)
	// Same local day, earlier time.
	in := time.Date(2026, 5, 21, 9, 5, 12, 0, moscow)
	if got, want := FormatDateTimeSmartAt(in, now), "09:05:12"; got != want {
		t.Errorf("smart(today) = %q, want %q", got, want)
	}
	// Same local day fed in UTC: 06:05:12 UTC == 09:05:12 Moscow.
	inUTC := time.Date(2026, 5, 21, 6, 5, 12, 0, time.UTC)
	if got, want := FormatDateTimeSmartAt(inUTC, now), "09:05:12"; got != want {
		t.Errorf("smart(today UTC) = %q, want %q", got, want)
	}
}

func TestFormatDateTimeSmart_YesterdayUsesFullDateTime(t *testing.T) {
	moscow := mustLoad(t, "Europe/Moscow")
	withLocal(t, moscow)

	now := time.Date(2026, 5, 21, 14, 0, 0, 0, moscow)
	in := time.Date(2026, 5, 20, 23, 59, 59, 0, moscow)
	if got, want := FormatDateTimeSmartAt(in, now), "2026-05-20 23:59:59"; got != want {
		t.Errorf("smart(yesterday 23:59:59) = %q, want %q", got, want)
	}
}

func TestFormatDateTimeSmart_DayBoundary(t *testing.T) {
	moscow := mustLoad(t, "Europe/Moscow")
	withLocal(t, moscow)

	now := time.Date(2026, 5, 21, 14, 0, 0, 0, moscow)
	// 00:00:01 of "today" — same local day → time only.
	startOfToday := time.Date(2026, 5, 21, 0, 0, 1, 0, moscow)
	if got, want := FormatDateTimeSmartAt(startOfToday, now), "00:00:01"; got != want {
		t.Errorf("smart(today 00:00:01) = %q, want %q", got, want)
	}
	// 23:59:59 of "yesterday" — different local day → full datetime.
	endOfYesterday := time.Date(2026, 5, 20, 23, 59, 59, 0, moscow)
	if got, want := FormatDateTimeSmartAt(endOfYesterday, now), "2026-05-20 23:59:59"; got != want {
		t.Errorf("smart(yesterday 23:59:59) = %q, want %q", got, want)
	}
}

func TestFormatDateTimeSmart_BoundaryMovesWithLocalTZ(t *testing.T) {
	// 23:30 UTC is *yesterday* in Honolulu (UTC-10) and *tomorrow*
	// in Tokyo (UTC+9). Smart formatting must use the local boundary
	// for the today/not-today decision.
	honolulu := mustLoad(t, "Pacific/Honolulu") // UTC-10, no DST
	tokyo := mustLoad(t, "Asia/Tokyo")          // UTC+9, no DST

	// We pick a known UTC instant and ask both zones what they think
	// of it relative to "now == same instant".
	utcNow := time.Date(2026, 5, 21, 23, 30, 0, 0, time.UTC)

	// In Honolulu, both timestamps fall on 2026-05-21 13:30 local.
	withLocal(t, honolulu)
	if got, want := FormatDateTimeSmartAt(utcNow, utcNow), "13:30:00"; got != want {
		t.Errorf("Honolulu smart = %q, want %q", got, want)
	}

	// In Tokyo, both timestamps fall on 2026-05-22 08:30 local.
	withLocal(t, tokyo)
	if got, want := FormatDateTimeSmartAt(utcNow, utcNow), "08:30:00"; got != want {
		t.Errorf("Tokyo smart = %q, want %q", got, want)
	}
}

func TestFormatDateTime_PreservesLocalLocation(t *testing.T) {
	// When the input already carries a non-UTC location, the helper
	// should still pin it to time.Local for display.
	moscow := mustLoad(t, "Europe/Moscow")
	tokyo := mustLoad(t, "Asia/Tokyo")
	withLocal(t, moscow)

	in := time.Date(2026, 5, 21, 18, 0, 0, 0, tokyo) // 12:00 Moscow
	if got, want := FormatDateTime(in), "2026-05-21 12:00:00"; got != want {
		t.Errorf("FormatDateTime(Tokyo→Moscow) = %q, want %q", got, want)
	}
}

func TestLayoutConstants(t *testing.T) {
	// Defensive check: somebody could regret-edit the layout literals.
	if Date != "2006-01-02" {
		t.Errorf("Date layout drifted: %q", Date)
	}
	if Time != "15:04:05" {
		t.Errorf("Time layout drifted: %q", Time)
	}
	if DateTime != "2006-01-02 15:04:05" {
		t.Errorf("DateTime layout drifted: %q", DateTime)
	}
}
