package harness

import "github.com/Barben360/backsight/internal/state"

// RecordedProfile is the profile client install recorded for harness id
// ("" its default, or nothing recorded). It is the one place that reads
// the recorded profile from the state, so that moving it per harness is a
// one-place edit. The profile is recorded once at install, never re-read
// from a shell: a scheduled job or an uninstall has no shell's profile.
func RecordedProfile(st state.State, id string) string {
	if id != ClaudeCode || st.Client == nil {
		return ""
	}
	return st.Client.Profile
}
