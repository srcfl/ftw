package main

import (
	"time"

	"github.com/srcfl/ftw/go/internal/mpc"
)

// missingPlanRetryDue reports whether the control tick should start a
// missing-plan replan. A new request cancels the one in flight, so retrying
// while a solve is pending would stop every solve slower than the cooldown
// and no plan would ever land.
func missingPlanRetryDue(svc *mpc.Service, planMissing bool, last time.Time, cooldown time.Duration) bool {
	return svc != nil && planMissing && time.Since(last) > cooldown && !svc.PlanSnapshot().Pending
}
