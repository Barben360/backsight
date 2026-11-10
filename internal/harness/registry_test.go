package harness

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Barben360/backsight/internal/state"
)

// other is a second harness: Claude Code's methods under another id.
type other struct{ Claude }

func (other) ID() string  { return "other-tool" }
func (other) Tag() string { return "harness:other-tool" }

func TestClaudeCodeIsAlwaysRegisteredFirst(t *testing.T) {
	all := All()
	if len(all) == 0 || all[0].ID() != ClaudeCode {
		t.Fatalf("%v", all)
	}
	if a, ok := ByID(ClaudeCode); !ok || a.Tag() != "harness:claude-code" {
		t.Errorf("%v %v", a, ok)
	}
	if Default().ID() != ClaudeCode || FromContext(context.Background()).ID() != ClaudeCode {
		t.Error("the default is not Claude Code's")
	}
}

func TestRegisterAddsASecondHarnessAndRemovesIt(t *testing.T) {
	if _, ok := ByID("other-tool"); ok {
		t.Fatal("registered before the test")
	}
	unregister := Register(other{})
	all := All()
	if len(all) != 2 || all[0].ID() != ClaudeCode || all[1].ID() != "other-tool" {
		t.Fatalf("%v", all)
	}
	if a, ok := ByID("other-tool"); !ok || a.Tag() != "harness:other-tool" || Of("other-tool").ID() != "other-tool" || TagOf("other-tool") != "harness:other-tool" {
		t.Errorf("%v %v", a, ok)
	}
	ctx := WithAdapter(context.Background(), other{})
	if FromContext(ctx).ID() != "other-tool" {
		t.Error("the context's adapter is lost")
	}
	// All hands out a copy.
	all[1] = Claude{}
	if All()[1].ID() != "other-tool" {
		t.Error("All shares the registry")
	}
	unregister()
	if _, ok := ByID("other-tool"); ok || len(All()) != 1 {
		t.Errorf("still registered: %v", All())
	}
}

func TestRegisterRefusesAnIDTwice(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Claude Code's id registered a second time")
		}
		if len(All()) != 1 {
			t.Errorf("%v", All())
		}
	}()
	Register(Claude{})
}

// An id no longer registered keeps its own tag; none is Claude Code's.
func TestOfAndTagOfOldRecords(t *testing.T) {
	if Of("").ID() != ClaudeCode || Of("gone").ID() != ClaudeCode {
		t.Error("Of")
	}
	if TagOf("") != "harness:claude-code" || TagOf("gone") != "harness:gone" || TagOf(ClaudeCode) != "harness:claude-code" {
		t.Errorf("%q %q", TagOf(""), TagOf("gone"))
	}
}

// The adapter's id is the key the state records Claude Code's profile
// under (an older state's client entry included).
func TestClaudesIDReadsItsRecordedProfile(t *testing.T) {
	st := state.State{Client: &state.Client{Profile: "/dev/profile"}}
	if p := st.ProfileOf(Claude{}.ID()); p != "/dev/profile" {
		t.Errorf("%q", p)
	}
	if p := st.ProfileOf(other{}.ID()); p != "" {
		t.Errorf("Claude Code's profile given to another harness: %q", p)
	}
}

func TestMissingError(t *testing.T) {
	err := error(MissingError{Command: "claude"})
	if err.Error() != "the claude command was not found" {
		t.Error(err)
	}
	if !errors.Is(err, ErrMissing) || !errors.Is(fmt.Errorf("wrapped: %w", err), ErrMissing) {
		t.Error("a MissingError is not ErrMissing")
	}
}
