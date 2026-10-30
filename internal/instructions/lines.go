package instructions

import "strings"

// The feedback lines (v9): every line a tool hands the agent to print, or
// a text asks it to write, reads "> 🧠<emoji> **<label> (<project> ·
// <what>)** — <text>". 🧠 alone: a memory was read; 🧠 and one emoji: a
// write or a problem, at most two emoji per line as on the status line.
const (
	emojiSaved      = "💾"
	emojiUpdated    = "🔄"
	emojiWrong      = "❌"
	emojiOutdated   = "⌛"
	emojiHidden     = "🙈"
	emojiNotRetired = "⚠️"
	emojiNothing    = "🔍"
	emojiRetro      = "📚"
)

// NothingLine answers an empty search; RetroLine opens a retro's summary.
// Constants: the guide and the gate reason are.
const (
	NothingLine = "> 🧠" + emojiNothing + " **Nothing in memory** — <topic>"
	RetroLine   = "> 🧠" + emojiRetro + " **Retro (<project>)** — <n> saved, <n> corrected, <n> to promote"
)

func line(emoji, label, scope, text string) string {
	return "> 🧠" + emoji + " **" + label + " (" + scope + ")** — " + text
}

// short is an id as the memories shown carry it: its first 8 characters.
func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// CreditLine is the line memory_credit hands back for the agent to print:
// the memories of one project it used, by short id.
func CreditLine(project string, ids8 []string) string {
	return line("", "From memory", project+" · "+strings.Join(ids8, ", "), "<the facts you used, in your words>")
}

// SavedLine and UpdatedLine are memory_store's: a new fact, or one that
// updates a memory the tool listed as similar. A note has no memory id
// yet, so the line names its kind.
func SavedLine(project, kind string) string {
	return line(emojiSaved, "Saved", project+" · "+kind, "<the fact, in a few words>")
}

func UpdatedLine(project, kind string) string {
	return line(emojiUpdated, "Updated", project+" · "+kind+" · replaces <id8 of the memory it updates>", "<the fact, in a few words>")
}

// why is what a wrong or outdated verdict's line says.
func why(verdict string) string {
	if verdict == "outdated" {
		return "<what changed, and what shows it>"
	}
	return "<what was wrong, and what shows it>"
}

func undo(id string) string { return " · undo: `backsight forget --restore " + id + "`" }

// RetiredLine is a memory a wrong or outdated verdict invalidated on the
// server.
func RetiredLine(verdict, project, id string) string {
	emoji, label := emojiWrong, "Wrong, retired"
	if verdict == "outdated" {
		emoji, label = emojiOutdated, "Outdated, retired"
	}
	return line(emoji, label, project+" · "+short(id), why(verdict)+undo(id))
}

// HiddenLine is a memory a verdict hid on this machine only; unread: the
// server could not be read, so the user can retire it there.
func HiddenLine(verdict, project, id string, unread bool) string {
	text := why(verdict)
	if unread {
		text += " · retire it on the server: `backsight forget " + id + "`"
	}
	return line(emojiHidden, "Hidden on this PC", project+" · "+short(id), text+undo(id))
}

// NotRetiredLine is a memory a verdict could not retire, with the reason
// and, when the user can, the command that retires it.
func NotRetiredLine(project, id, reason string, retire bool) string {
	if retire {
		reason += " · retire it: `backsight forget " + id + "`"
	}
	return line(emojiNotRetired, "Not retired", project+" · "+short(id), reason)
}
