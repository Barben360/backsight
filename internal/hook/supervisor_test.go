package hook

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as the child: TestMain runs childMain when the
// helper variable is set, like the real binary's hidden hook-child command.
func TestMain(m *testing.M) {
	if os.Getenv("HOOK_TEST_CHILD") == "1" {
		os.Exit(RunChild(context.Background(), os.Args[len(os.Args)-1], os.Stdin, os.Stdout, func(_ context.Context, event string, in []byte) ([]byte, error) {
			switch os.Getenv("HOOK_TEST_BEHAVIOUR") {
			case "echo":
				return []byte(`{"hookSpecificOutput":{"hookEventName":"` + event + `","additionalContext":"ok"}}`), nil
			case "empty":
				return nil, nil
			case "garbage":
				return []byte("not json"), nil
			case "array":
				return []byte(`[1,2]`), nil
			case "slow":
				time.Sleep(300 * time.Millisecond)
				return []byte(`{"systemMessage":"late but in time"}`), nil
			}
			return nil, nil
		}))
	}
	os.Exit(m.Run())
}

func super(t *testing.T, behaviour, chaos string, deadline time.Duration) ([]byte, *Failure) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s := Supervisor{
		Command: func(ctx context.Context, event string) *exec.Cmd {
			c := exec.CommandContext(ctx, exe, "-test.run=^$", event)
			c.Env = append(os.Environ(), "HOOK_TEST_CHILD=1", "HOOK_TEST_BEHAVIOUR="+behaviour, ChaosEnv+"="+chaos)
			return c
		},
		Deadline: func(string, []byte) time.Duration { return deadline },
	}
	return s.Run(context.Background(), "user-prompt-submit", []byte(`{"session_id":"s1","prompt":"hi"}`))
}

func TestChildOutputPassesThroughWhenValid(t *testing.T) {
	out, f := super(t, "echo", "", 5*time.Second)
	if f != nil || !strings.Contains(string(out), `"additionalContext":"ok"`) {
		t.Errorf("out %q failure %+v", out, f)
	}
	if out, f := super(t, "empty", "", 5*time.Second); f != nil || len(out) != 0 {
		t.Errorf("empty: %q %+v", out, f)
	}
	if out, f := super(t, "slow", "", 5*time.Second); f != nil || !strings.Contains(string(out), "late but in time") {
		t.Errorf("slow within deadline: %q %+v", out, f)
	}
}

// Chaos (SPEC §6.1): every failure prints nothing and is reported once.
func TestEveryChildFailurePrintsNothing(t *testing.T) {
	for _, c := range []struct {
		behaviour, chaos, kind string
		deadline               time.Duration
	}{
		{"echo", "panic", "panic", 5 * time.Second},
		{"echo", "fatal", "fatal", 5 * time.Second},
		{"echo", "hang", "timeout", 400 * time.Millisecond},
		{"echo", "kill", "killed", 5 * time.Second},
		{"echo", "exit2", "exit", 5 * time.Second},
		{"garbage", "", "invalid_output", 5 * time.Second},
		{"array", "", "invalid_output", 5 * time.Second},
	} {
		start := time.Now()
		out, f := super(t, c.behaviour, c.chaos, c.deadline)
		if len(out) != 0 || f == nil || f.Kind != c.kind {
			t.Errorf("%s/%s: out %q failure %+v", c.behaviour, c.chaos, out, f)
		}
		if c.chaos == "hang" && time.Since(start) > 3*time.Second {
			t.Errorf("hang not cut at the deadline: %s", time.Since(start))
		}
	}
}

func TestStdinReachesTheChildAndIsCapped(t *testing.T) {
	big := strings.Repeat("x", MaxInput+10)
	exe, _ := os.Executable()
	s := Supervisor{
		Command: func(ctx context.Context, event string) *exec.Cmd {
			c := exec.CommandContext(ctx, exe, "-test.run=^$", event)
			c.Env = append(os.Environ(), "HOOK_TEST_CHILD=1", "HOOK_TEST_BEHAVIOUR=echo")
			return c
		},
		Deadline: func(string, []byte) time.Duration { return 5 * time.Second },
	}
	if _, f := s.Run(context.Background(), "stop", []byte(big)); f == nil || f.Kind != "input_too_large" {
		t.Errorf("%+v", f)
	}
}

func TestDeadlines(t *testing.T) {
	for _, c := range []struct {
		event, input string
		want         time.Duration
	}{
		{"session-start", `{"source":"startup"}`, 8 * time.Second},
		{"session-start", `{"source":"resume"}`, 3 * time.Second},
		{"session-start", `{"source":"compact"}`, 3 * time.Second},
		{"user-prompt-submit", `{}`, 20 * time.Second},
		{"pre-tool-use", `{}`, 9 * time.Second},
		{"post-tool-use", `{}`, 1500 * time.Millisecond},
		{"subagent-start", `{}`, 3 * time.Second},
		{"subagent-stop", `{}`, 3 * time.Second},
		{"stop", `{}`, 3 * time.Second},
		{"session-end", `{}`, 800 * time.Millisecond},
		{"unknown", `{}`, 3 * time.Second},
	} {
		if got := ChildDeadline(c.event, []byte(c.input)); got != c.want {
			t.Errorf("%s %s: %s, want %s", c.event, c.input, got, c.want)
		}
	}
}
