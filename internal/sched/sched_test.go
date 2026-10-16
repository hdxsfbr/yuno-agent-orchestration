package sched

import (
	"errors"
	"strings"
	"testing"
	"time"

	"yuno/internal/store"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func mustZone(t *testing.T, tz string) *time.Location {
	t.Helper()
	loc, err := LoadZone(tz)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// T5.9.1: the next run on both sides of an occurrence; strictly after now.
func TestNextCronBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, expr, now, want string
	}{
		{"one second before", "0 9 * * *", "2026-10-15T08:59:59Z", "2026-10-15T09:00:00Z"},
		{"at the occurrence", "0 9 * * *", "2026-10-15T09:00:00Z", "2026-10-16T09:00:00Z"},
		{"just after", "0 9 * * *", "2026-10-15T09:00:01Z", "2026-10-16T09:00:00Z"},
		{"every 15 minutes", "*/15 * * * *", "2026-10-15T09:14:59Z", "2026-10-15T09:15:00Z"},
		{"weekdays only, from Friday", "0 9 * * 1-5", "2026-10-16T10:00:00Z", "2026-10-19T09:00:00Z"},
		{"month end", "30 23 31 * *", "2026-10-31T23:30:00Z", "2026-12-31T23:30:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NextCron(tc.expr, time.UTC, mustTime(t, tc.now))
			if err != nil {
				t.Fatal(err)
			}
			if want := mustTime(t, tc.want); !got.Equal(want) || got.Location() != time.UTC {
				t.Fatalf("next = %s, want %s (UTC)", got, want)
			}
		})
	}
}

// T5.9.6: one expression, two zones, two instants.
func TestNextCronTimeZones(t *testing.T) {
	now := mustTime(t, "2026-10-15T08:00:00Z")
	utc, err := NextCron("0 9 * * *", mustZone(t, "UTC"), now)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := NextCron("0 9 * * *", mustZone(t, "America/Sao_Paulo"), now)
	if err != nil {
		t.Fatal(err)
	}
	if want := mustTime(t, "2026-10-15T09:00:00Z"); !utc.Equal(want) {
		t.Fatalf("UTC next = %s, want %s", utc, want)
	}
	// São Paulo is UTC-3 all year since 2019: 09:00 there is 12:00Z.
	if want := mustTime(t, "2026-10-15T12:00:00Z"); !sp.Equal(want) {
		t.Fatalf("America/Sao_Paulo next = %s, want %s", sp, want)
	}
	if utc.Equal(sp) {
		t.Fatal("the zone made no difference")
	}
}

// T5.9.4 (parser side): invalid expressions carry the parser's message; inline zones
// and sub-minute @every are refused (rule 5).
func TestValidateCron(t *testing.T) {
	for _, tc := range []struct {
		expr, want string
	}{
		{"0 9 * * *", ""},
		{"@daily", ""},
		{"@every 5m", ""},
		{"", "is required"},
		{"61 * * * *", "end of range (61) above maximum (59)"},
		{"* * * *", "expected exactly 5 fields"},
		{"not a cron", "expected exactly 5 fields"},
		{"0 0 0 9 * * *", "expected exactly 5 fields"},
		{"@every 30s", "every 1m0s"},
		{"CRON_TZ=Europe/Paris 0 9 * * *", "set tz instead"},
		{"0 0 31 2 *", "no future occurrence"},
		{"0 12 31 4,6,9,11 *", "no future occurrence"},
		{"0 0 29 2 *", ""},
	} {
		err := ValidateCron(tc.expr)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%q: unexpected error %v", tc.expr, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%q: error %v, want one containing %q", tc.expr, err, tc.want)
		}
	}
}

func TestLoadZone(t *testing.T) {
	for _, tz := range []string{"", "UTC", "America/Sao_Paulo", "Europe/Berlin"} {
		if _, err := LoadZone(tz); err != nil {
			t.Errorf("%q: %v", tz, err)
		}
	}
	for _, tz := range []string{"Mars/Olympus", "Local", "utc+3"} {
		if _, err := LoadZone(tz); err == nil {
			t.Errorf("%q: accepted", tz)
		}
	}
}

func TestNextRun(t *testing.T) {
	now := mustTime(t, "2026-10-15T08:00:00Z")
	expr := "30 8 * * *"
	runAt := mustTime(t, "2026-10-20T10:00:00Z")
	for _, tc := range []struct {
		name string
		s    store.Schedule
		want *time.Time
	}{
		{"cron", store.Schedule{Kind: store.ScheduleCron, CronExpr: &expr, TZ: "UTC", Enabled: true}, ptr(mustTime(t, "2026-10-15T08:30:00Z"))},
		{"cron in zone", store.Schedule{Kind: store.ScheduleCron, CronExpr: &expr, TZ: "America/Sao_Paulo", Enabled: true}, ptr(mustTime(t, "2026-10-15T11:30:00Z"))},
		{"delay", store.Schedule{Kind: store.ScheduleDelay, RunAt: &runAt, TZ: "UTC", Enabled: true}, &runAt},
		{"disabled", store.Schedule{Kind: store.ScheduleCron, CronExpr: &expr, TZ: "UTC"}, nil},
	} {
		got, err := NextRun(&tc.s, now)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if (got == nil) != (tc.want == nil) || (got != nil && !got.Equal(*tc.want)) {
			t.Fatalf("%s: next = %v, want %v", tc.name, got, tc.want)
		}
	}
	// An expression with no date to come has no next run, never the zero time.
	never := "0 0 31 2 *"
	got, err := NextRun(&store.Schedule{Kind: store.ScheduleCron, CronExpr: &never, TZ: "UTC", Enabled: true}, now)
	if !errors.Is(err, ErrNoFutureOccurrence) || got != nil {
		t.Fatalf("next = %v, %v; want ErrNoFutureOccurrence", got, err)
	}
}

func ptr[T any](v T) *T { return &v }
