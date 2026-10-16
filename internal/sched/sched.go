// Package sched is the durable scheduler (DESIGN 5.9): cron and delayed triggers
// stored in Postgres and fired by one locking transaction per tick.
package sched

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"yuno/internal/store"
)

// MinCronInterval is the shortest cadence a schedule may have (DESIGN 5.9 rule 5).
const MinCronInterval = time.Minute

// ErrNoFutureOccurrence is a cron expression that parses but names no date that comes,
// such as "0 0 31 2 *" (the cron library answers the zero time for it).
var ErrNoFutureOccurrence = errors.New("no future occurrence")

// NextCron returns the next occurrence of a standard 5-field cron expression after
// `after`, interpreted in loc (DESIGN 5.9 rule 1).
func NextCron(expr string, loc *time.Location, after time.Time) (time.Time, error) {
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return time.Time{}, err
	}
	if loc == nil {
		loc = time.UTC
	}
	next := sched.Next(after.In(loc))
	if next.IsZero() {
		return time.Time{}, ErrNoFutureOccurrence
	}
	return next.UTC(), nil
}

// ValidateCron checks an expression the way the scheduler will read it: the cron
// library's parser, no inline time zone (the schedule's tz is the only one), no @every
// shorter than a minute, which the standard parser would otherwise accept, and at least
// one occurrence to come.
func ValidateCron(expr string) error {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return errors.New("is required for a cron schedule")
	}
	if strings.HasPrefix(expr, "CRON_TZ=") || strings.HasPrefix(expr, "TZ=") {
		return errors.New("must not carry a time zone; set tz instead")
	}
	if _, err := cron.ParseStandard(expr); err != nil {
		return err
	}
	if d, ok := strings.CutPrefix(expr, "@every "); ok {
		every, err := time.ParseDuration(strings.TrimSpace(d))
		if err != nil {
			return err
		}
		if every < MinCronInterval {
			return fmt.Errorf("must not fire more often than every %s", MinCronInterval)
		}
	}
	_, err := NextCron(expr, time.UTC, time.Now())
	return err
}

// LoadZone resolves an IANA zone name; empty means UTC. "Local" is refused because it
// is whatever the server's zone happens to be.
func LoadZone(tz string) (*time.Location, error) {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		return time.UTC, nil
	}
	if tz == "Local" {
		return nil, errors.New("must be an IANA time zone name such as America/Sao_Paulo")
	}
	return time.LoadLocation(tz)
}

// NextRun is when an enabled schedule fires next, from now (DESIGN 5.9 rules 1 and 3):
// the next cron occurrence strictly after now in the schedule's zone, or run_at for a
// delay. Disabled schedules have none.
func NextRun(s *store.Schedule, now time.Time) (*time.Time, error) {
	if !s.Enabled {
		return nil, nil
	}
	switch s.Kind {
	case store.ScheduleCron:
		if s.CronExpr == nil {
			return nil, errors.New("cron schedule without an expression")
		}
		loc, err := LoadZone(s.TZ)
		if err != nil {
			return nil, err
		}
		next, err := NextCron(*s.CronExpr, loc, now)
		if err != nil {
			return nil, err
		}
		return &next, nil
	case store.ScheduleDelay:
		if s.RunAt == nil {
			return nil, errors.New("delay schedule without run_at")
		}
		at := s.RunAt.UTC()
		return &at, nil
	}
	return nil, fmt.Errorf("unknown schedule kind %q", s.Kind)
}
