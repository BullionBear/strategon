package api

import (
	"fmt"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	agentcron "github.com/bullionbear/strategon/internal/agent/cron"
)

func validateSchedules(schedules []*pb.CronSchedule) error {
	seen := map[string]struct{}{}
	for i, s := range schedules {
		if s == nil {
			return fmt.Errorf("schedules[%d]: nil", i)
		}
		name := s.GetName()
		if name == "" {
			return fmt.Errorf("schedules[%d]: name is required", i)
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("schedules: duplicate name %q", name)
		}
		seen[name] = struct{}{}
		if _, err := agentcron.Parse(s.GetCronExpr(), s.GetTimezone()); err != nil {
			return fmt.Errorf("schedules[%d] %q: %w", i, name, err)
		}
		switch s.GetAction() {
		case pb.CronAction_CRON_ACTION_RESTART, pb.CronAction_CRON_ACTION_RELOAD_CONFIG:
			// ok
		case pb.CronAction_CRON_ACTION_RUN_SCRIPT:
			if s.GetScriptRef() == "" {
				return fmt.Errorf("schedules[%d] %q: script_ref required for RUN_SCRIPT", i, name)
			}
		default:
			return fmt.Errorf("schedules[%d] %q: action is required", i, name)
		}
		if s.GetJitterSeconds() < 0 {
			return fmt.Errorf("schedules[%d] %q: jitter_seconds must be >= 0", i, name)
		}
	}
	return nil
}
