package instructions

import (
	"strconv"
	"strings"
)

// The lines a user sees in a session (hook systemMessage, and the pause
// command the /backsight:pause skill relays). Claude never reads them, so
// they are not part of the instruction set's Version. One leading emoji per
// state, the same in every message (issue 7):
const (
	EmojiOn          = "🧠"  // memory on: recall and recording
	EmojiNotRecorded = "⏸️" // this session or project is not recorded
	EmojiOff         = "🔕"  // recording turned off (components.write)
	EmojiWarning     = "⚠️" // something needs fixing: the line names the command
	EmojiMaintenance = "🛠️" // temporary: memory is back when it ends
	EmojiUpdate      = "⬆️" // a newer release
	EmojiProject     = "🗂️" // marks the project name
)

// statusLine is "<emoji> backsight · 🗂️ <project> · <text>"; a prompt-time
// line has no project, and an empty text ends the line at the project.
func statusLine(emoji, project, text string) string {
	parts := []string{emoji + " backsight"}
	if project != "" {
		parts = append(parts, EmojiProject+" "+project)
	}
	if text != "" {
		parts = append(parts, text)
	}
	return strings.Join(parts, " · ")
}

// StatusOn: recall and recording are on. 🧠 says so, so the line has no
// words of its own: "🧠 backsight · 🗂️ <project>".
func StatusOn(project string) string { return statusLine(EmojiOn, project, "") }

// Rosewood (#C9507A, bold) reads on dark and light terminals alike;
// Claude Code renders it in a hook's systemMessage.
const brandOn, brandOff = "\x1b[1;38;2;201;80;122m", "\x1b[0m"

// Colored paints the brand word of each "<emoji> backsight" line rosewood.
// Other lines, and a project that is itself called backsight, are left
// as they are.
func Colored(banner string) string {
	lines := strings.Split(banner, "\n")
	for i, l := range lines {
		emoji, rest, ok := strings.Cut(l, " ")
		if ok && (rest == "backsight" || strings.HasPrefix(rest, "backsight ")) {
			lines[i] = emoji + " " + brandOn + "backsight" + brandOff + strings.TrimPrefix(rest, "backsight")
		}
	}
	return strings.Join(lines, "\n")
}

// StatusNotRecorded: the project is excluded (privacy.exclude_paths, a
// .backsight.toml with retain = false, or a broken one); recall still runs.
func StatusNotRecorded(project string) string {
	return statusLine(EmojiNotRecorded, project, "recall on, this project is not recorded")
}

// StatusRecordingOff: components.write = false.
func StatusRecordingOff(project string) string {
	return statusLine(EmojiOff, project, "recall on, recording off")
}

// MemoryUnavailable is the visible failure, once per session; project is
// "" at prompt time.
func MemoryUnavailable(project, why string) string {
	return statusLine(EmojiWarning, project, "memory unavailable ("+why+") · run `backsight doctor`")
}

// MemoryMaintenance: a maintenance lock is held (backup, restore, update).
func MemoryMaintenance(project string) string {
	return statusLine(EmojiMaintenance, project, "memory unavailable (maintenance in progress), back when it ends")
}

// UpdateLine names the version only: release notes are not signed.
func UpdateLine(version string) string {
	return EmojiUpdate + " backsight update " + version + " available · see `backsight changelog`"
}

// DriftLine: the plugin, the permission rule or the shim changed.
func DriftLine(what string) string {
	return EmojiWarning + " Claude Code wiring drifted (" + what + ") · run `backsight client install`"
}

// QuarantineLine: sessions the flusher holds back.
func QuarantineLine(n int) string {
	noun := "sessions"
	if n == 1 {
		noun = "session"
	}
	return EmojiWarning + " " + strconv.Itoa(n) + " " + noun + " quarantined (not sent to memory) · run `backsight doctor`"
}
