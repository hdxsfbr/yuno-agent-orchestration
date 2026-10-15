// Package sched is the durable scheduler (DESIGN 5.9): cron and delayed triggers
// stored in Postgres and fired by one transaction per tick. Wave 3 implements it; this
// file keeps the cron parser dependency pinned until then.
package sched

import (
	"time"

	"github.com/robfig/cron/v3"
)

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
	return sched.Next(after.In(loc)), nil
}
