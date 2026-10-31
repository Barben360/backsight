package metrics

import (
	"fmt"
	"testing"
	"time"
)

func TestErrorRules(t *testing.T) {
	check := func(t *testing.T, what string, got, want Status) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %s, want %s", what, got, want)
		}
	}
	h := time.Hour

	t.Run("consolidation", func(t *testing.T) {
		for failed, want := range map[int64]Status{0: StatusOK, 1: StatusWatch, 7: StatusWatch} {
			check(t, fmt.Sprintf("ruleConsolidation(%d)", failed), ruleConsolidation(failed), want)
		}
	})

	t.Run("server queue", func(t *testing.T) {
		for failed, want := range map[int64]Status{0: StatusOK, 1: StatusWatch, 40: StatusWatch} {
			check(t, fmt.Sprintf("ruleServerQueue(%d)", failed), ruleServerQueue(failed), want)
		}
	})

	t.Run("pipeline flow", func(t *testing.T) {
		for _, c := range []struct {
			failed, setAside int64
			age              time.Duration
			want             Status
		}{
			{0, 0, 0, StatusOK},
			{0, 0, 23 * h, StatusOK},
			{0, 0, 24 * h, StatusOK}, // "older than 24 h": at 24 h, not yet
			{0, 0, 24*h + time.Second, StatusBad},
			{1, 0, 0, StatusWatch},
			{0, 1, 0, StatusWatch},
			{3, 2, 24 * h, StatusWatch},
			{1, 1, 25 * h, StatusBad},
		} {
			check(t, fmt.Sprintf("rulePipelineFlow(%d, %d, %v)", c.failed, c.setAside, c.age),
				rulePipelineFlow(c.failed, c.setAside, c.age), c.want)
		}
	})

	t.Run("local backlog", func(t *testing.T) {
		for _, c := range []struct {
			oldest time.Duration
			want   Status
		}{
			{0, StatusOK},
			{59 * time.Minute, StatusOK},
			{h, StatusOK},
			{h + time.Second, StatusWatch},
			{61 * time.Minute, StatusWatch},
			{24 * h, StatusWatch},
			{24*h + time.Second, StatusBad},
			{25 * h, StatusBad},
		} {
			check(t, fmt.Sprintf("ruleLocalBacklog(%v)", c.oldest), ruleLocalBacklog(c.oldest), c.want)
		}
	})

	t.Run("hook failures", func(t *testing.T) {
		for _, c := range []struct {
			last24h map[string]int64
			want    Status
		}{
			{nil, StatusOK},
			{map[string]int64{}, StatusOK},
			{map[string]int64{"panic": 0, "exit": 0}, StatusOK},
			// Any kind: watch at 3, bad at 10.
			{map[string]int64{"timeout": 2}, StatusOK},
			{map[string]int64{"timeout": 3}, StatusWatch},
			{map[string]int64{"killed": 9}, StatusWatch},
			{map[string]int64{"invalid_output": 10}, StatusBad},
			{map[string]int64{"input_too_large": 2, "timeout": 2, "killed": 2}, StatusOK}, // per kind, not summed
			// The serious kinds watch at 1.
			{map[string]int64{"panic": 1}, StatusWatch},
			{map[string]int64{"fatal": 1}, StatusWatch},
			{map[string]int64{"exit": 1}, StatusWatch},
			{map[string]int64{"spawn": 1}, StatusWatch},
			{map[string]int64{"exit": 9}, StatusWatch},
			{map[string]int64{"exit": 10}, StatusBad},
			{map[string]int64{"timeout": 2, "panic": 1}, StatusWatch},
		} {
			check(t, fmt.Sprintf("ruleHookFailures(%v)", c.last24h), ruleHookFailures(c.last24h), c.want)
		}
	})

	t.Run("server reach", func(t *testing.T) {
		for _, c := range []struct {
			failing, open bool
			age           time.Duration
			want          Status
		}{
			{false, false, 0, StatusOK},
			{true, false, h, StatusWatch},
			{false, true, h, StatusWatch},
			{true, true, h, StatusWatch},
			{true, false, 24 * h, StatusWatch},
			{true, false, 24*h + time.Second, StatusBad},
			{true, false, 25 * h, StatusBad},
			{false, true, 25 * h, StatusBad},
			// An old backlog with the server answering is the backlog's
			// own rule, not reach's.
			{false, false, 25 * h, StatusOK},
		} {
			check(t, fmt.Sprintf("ruleServerReach(%v, %v, %v)", c.failing, c.open, c.age),
				ruleServerReach(c.failing, c.open, c.age), c.want)
		}
	})

	t.Run("doctor", func(t *testing.T) {
		for _, c := range []struct {
			checks []DoctorCheck
			want   Status
		}{
			{nil, StatusOK},
			{[]DoctorCheck{{ID: "a", Status: "ok"}, {ID: "b", Status: "info"}}, StatusOK},
			{[]DoctorCheck{{ID: "a", Status: "warn"}}, StatusOK},
			{[]DoctorCheck{{ID: "a", Status: "error"}}, StatusWatch},
			{[]DoctorCheck{{ID: "a", Status: "warn"}, {ID: "b", Status: "error", Fix: "backsight doctor"}}, StatusWatch},
		} {
			check(t, fmt.Sprintf("ruleDoctor(%v)", c.checks), ruleDoctor(c.checks), c.want)
		}
	})

	t.Run("jobs", func(t *testing.T) {
		m := time.Minute
		for _, c := range []struct {
			sinceLast, cadence time.Duration
			activity           bool
			want               Status
		}{
			{31 * m, 15 * m, true, StatusBad},
			{31 * m, 15 * m, false, StatusOK}, // a sleeping laptop is not a fault
			{29 * m, 15 * m, true, StatusOK},
			{30 * m, 15 * m, true, StatusOK}, // at 2 × cadence, not past it
			{30*m + time.Second, 15 * m, true, StatusBad},
			{3 * 24 * h, 24 * h, false, StatusOK},
			{49 * h, 24 * h, true, StatusBad},
		} {
			check(t, fmt.Sprintf("ruleJobs(%v, %v, %v)", c.sinceLast, c.cadence, c.activity),
				ruleJobs(c.sinceLast, c.cadence, c.activity), c.want)
		}
	})
}
