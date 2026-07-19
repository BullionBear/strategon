// Package cron evaluates StrategyAssignmentSpec schedules locally on the agent
// (ARCHITECTURE §10): crontab expression + explicit IANA timezone + optional
// jitter so multi-machine fleets do not restart in lockstep.
package cron

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/robfig/cron/v3"
)

// standard 5-field crontab (min hour dom month dow), plus descriptors (@daily…).
var parser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// Schedule is a parsed cron expression bound to an IANA timezone.
type Schedule struct {
	expr string
	tz   string
	sched cron.Schedule
	loc   *time.Location
}

// Parse validates cron_expr and timezone and returns a Schedule.
func Parse(cronExpr, timezone string) (*Schedule, error) {
	if cronExpr == "" {
		return nil, fmt.Errorf("cron: empty expression")
	}
	if timezone == "" {
		return nil, fmt.Errorf("cron: timezone is required (IANA, e.g. UTC or Asia/Taipei)")
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, fmt.Errorf("cron: timezone %q: %w", timezone, err)
	}
	sched, err := parser.Parse(cronExpr)
	if err != nil {
		return nil, fmt.Errorf("cron: parse %q: %w", cronExpr, err)
	}
	return &Schedule{expr: cronExpr, tz: timezone, sched: sched, loc: loc}, nil
}

// NextAfter returns the next fire time after `after`, optionally delayed by a
// uniform random jitter in [0, jitterSeconds]. randN, when non-nil, returns an
// integer in [0, n); when nil, math/rand is used (non-crypto; fine for stagger).
func (s *Schedule) NextAfter(after time.Time, jitterSeconds int32, randN func(n int32) int32) time.Time {
	base := s.sched.Next(after.In(s.loc))
	if jitterSeconds <= 0 {
		return base
	}
	var j int32
	if randN != nil {
		j = randN(jitterSeconds + 1)
	} else {
		j = rand.Int31n(jitterSeconds + 1)
	}
	if j < 0 {
		j = 0
	}
	return base.Add(time.Duration(j) * time.Second)
}

// Expr returns the original crontab expression.
func (s *Schedule) Expr() string { return s.expr }

// Timezone returns the IANA timezone name.
func (s *Schedule) Timezone() string { return s.tz }
