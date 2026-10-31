package metrics

// Threshold is an absolute rule on a metric: it is borderline or bad by its
// own value, without a baseline. On names what is compared: hi is the upper
// bound of a rate's interval (the rule fires when it is below Watch), lo its
// lower bound (fires when above Watch or Bad), p95 and value the figure
// itself (fires when above).
type Threshold struct {
	Watch *float64 `json:"watch,omitempty"`
	Bad   *float64 `json:"bad,omitempty"`
	On    string   `json:"on"` // hi | lo | p95 | value, empty when there is no absolute rule
}

// Def describes one metric: what it is called, what it answers, how it is
// computed and judged, and what to do when its value is bad.
type Def struct {
	ID          string    `json:"id"`
	Label       string    `json:"label"`
	Question    string    `json:"question"`
	Definition  string    `json:"definition"`
	Unit        string    `json:"unit"`
	Aggregation string    `json:"aggregation"` // raw | recent_weighted
	Direction   string    `json:"direction"`   // up | down | none
	MinN        int       `json:"min_n"`
	Absolute    Threshold `json:"absolute"`
	Relative    bool      `json:"relative"`
	ErrorClass  bool      `json:"error_class"`
	CI          string    `json:"ci"`    // wilson | wilson_approx | none
	Scope       string    `json:"scope"` // machine | server
	Source      string    `json:"source"`
	Action      string    `json:"action"`  // what a bad value means and what to do
	Command     string    `json:"command"` // the command that helps
	Dev         bool      `json:"dev"`
	Family      string    `json:"family"`          // reach | quality | cost | bank | pipeline
	Series      string    `json:"series"`          // none | daily | weekly
	Extra       []string  `json:"extra,omitempty"` // the Metric.Extra keys it may carry
	Drill       string    `json:"drill"`           // base command; the view appends the window and project flags
}

func f(x float64) *float64 { return &x }

// pctExtra is what a percentile metric reports beside its value.
var pctExtra = []string{"p50", "p95"}

// Catalog lists every metric, in the order a view lists them. Sources:
// telemetry, journal, ledger, local (this PC's state files), server, doctor.
// context.per_session carries no Watch value here: it is 1.5 times the
// configured recall.session_budget_chars (9,000 by default), computed when
// a view runs.
var Catalog = withDrills([]Def{
	{
		ID: "recall.hit_rate", Label: "Prompts that got memory", Unit: "ratio", Direction: "up", MinN: 20,
		Absolute: Threshold{Watch: f(0.10), On: "hi"}, Relative: true, CI: "wilson", Scope: "machine", Source: "telemetry",
		Family: "reach", Series: "daily", Command: "backsight stats quality", Drill: "backsight stats sessions --has memory",
		Question:   "Does memory show up when you ask something?",
		Definition: "Prompt recalls that put at least one memory in front of Claude, out of all prompt recalls. Shown with a 95% range; it is watched when even the interval's upper bound stays under 10%.",
		Action:     "Memory may be thin or the recall cut-offs too strict. Look at why prompts got nothing, and import past sessions.",
	},
	{
		ID: "recall.empty_reasons", Label: "Why prompts got nothing", Unit: "count", Direction: "none", MinN: 0,
		CI: "none", Scope: "machine", Source: "telemetry", Family: "reach", Series: "daily", Command: "backsight stats quality",
		Extra:      []string{"empty", "skips"},
		Question:   "Why did some prompts get no memory?",
		Definition: "Recalls that injected nothing, counted by reason (no candidates, below the cut-off, all already seen, capped), and prompts skipped, counted by reason.",
		Action:     "Tells which fix applies: thin memory, cut-offs that are too strict, or memories already shown this session.",
	},
	{
		ID: "recall.by_lang", Label: "Prompts that got memory, by language", Unit: "ratio", Direction: "up", MinN: 20,
		CI: "wilson", Scope: "machine", Source: "telemetry", Dev: true, Family: "reach", Series: "none", Command: "backsight stats quality --dev",
		Extra:      []string{"by_lang"},
		Question:   "Does memory show up as often in every language?",
		Definition: "The share of prompt recalls that injected a memory, split by the language of the prompt. Each language needs its own 20 prompts before it is judged.",
		Action:     "A language far below the others means memory misses across languages.",
	},
	{
		ID: "search.rate", Label: "Prompts where Claude searched memory", Unit: "ratio", Direction: "up", MinN: 20,
		Relative: true, CI: "wilson", Scope: "machine", Source: "telemetry", Family: "reach", Series: "daily", Command: "backsight stats quality",
		Question:   "Does Claude look in memory on its own?",
		Definition: "Typed prompts of at least 40 characters whose turn used memory_search, memory_read or memory_store (main agent), out of all typed prompts of at least 40 characters.",
		Action:     "Claude ignores the memory tools. Check the instructions it is given.",
	},
	{
		ID: "search.empty_rate", Label: "Searches that found nothing", Unit: "ratio", Direction: "down", MinN: 20,
		Relative: true, CI: "wilson", Scope: "machine", Source: "telemetry", Family: "reach", Series: "daily", Command: "backsight stats quality",
		Extra:      []string{"failed"},
		Question:   "How often does a memory search come back empty?",
		Definition: "memory_search calls that succeeded with no hit, out of memory_search calls that succeeded. Failed calls are counted apart.",
		Action:     "Gaps in memory: what Claude looks for is not stored.",
	},
	{
		ID: "used.turn_rate", Label: "Turns where Claude used memory", Unit: "ratio", Direction: "up", MinN: 20,
		Relative: true, CI: "wilson", Scope: "machine", Source: "journal", Family: "quality", Series: "daily",
		Command: "backsight stats quality", Drill: "backsight stats sessions --has used",
		Extra:      []string{"worst_case", "pending", "check_skipped", "subagent_turns"},
		Question:   "When memory was shown, did Claude use it?",
		Definition: "Main-agent turns that showed memory and whose final stop declared at least one memory used, out of those turns that declared (a turn declaring only wrong, outdated or none counts as declared without use). The worst case, counting undeclared turns as unused, is given beside it. Unreliable while the declaring coverage's upper bound is under 80%.",
		Action:     "Recall is noisy. Tighten the recall cut-offs and review the memories that are never used.",
	},
	{
		ID: "declare.coverage", Label: "Turns where Claude said what it used", Unit: "ratio", Direction: "up", MinN: 20,
		Absolute: Threshold{Watch: f(0.80), On: "hi"}, CI: "wilson", Scope: "machine", Source: "journal", Family: "quality", Series: "daily",
		Command: "backsight stats quality", Drill: "backsight stats sessions --has undeclared",
		Extra:      []string{"pending", "check_skipped"},
		Question:   "Does Claude tell us which memories it used?",
		Definition: "Main-agent turns that showed memory and ended with a declaration, out of those that ended declared or undeclared. Turns whose check was skipped are counted apart, and turns still open are pending.",
		Action:     "Every quality number is unreliable until Claude declares more often. Check that the declaration instructions are in place.",
	},
	{
		ID: "used.rubber_stamp", Label: "Turns where every memory was marked used", Unit: "ratio", Direction: "down", MinN: 30,
		Absolute: Threshold{Watch: f(0.80), On: "lo"}, CI: "wilson", Scope: "machine", Source: "journal", Dev: true, Family: "quality", Series: "none",
		Command:    "backsight stats quality --dev",
		Question:   "Does Claude mark everything as used without looking?",
		Definition: "Declared turns where every shown memory was credited, out of declared turns that showed at least two memories. Watched when even the interval's lower bound is above 80%.",
		Action:     "Declarations no longer tell good memories from bad ones. Review how Claude is asked to declare.",
	},
	{
		ID: "used.share_confident", Label: "Confidently ranked memories that were used", Unit: "ratio", Direction: "up", MinN: 20,
		CI: "none", Scope: "machine", Source: "journal", Dev: true, Family: "quality", Series: "daily", Command: "backsight stats quality --dev",
		Extra:      []string{"unknown", "shown", "forgotten", "pending", "path"},
		Question:   "When memory is sure of itself, is it right?",
		Definition: "Settled shows credited as used, out of shows credited or confidently missed, for the prompt path by default. Shows with an unknown outcome, shows of memories forgotten since, and shows still pending are reported beside it.",
		Action:     "Confident rankings that go unused mean the ranking over-trusts some memories.",
	},
	{
		ID: "used.rank", Label: "Position of the memories used", Unit: "count", Direction: "none", MinN: 20,
		CI: "none", Scope: "machine", Source: "journal", Dev: true, Family: "quality", Series: "none", Command: "backsight stats quality --dev",
		Extra:      []string{"histogram", "hit_at_1", "hit_at_3"},
		Question:   "Are the memories Claude uses ranked near the top?",
		Definition: "How the credited shows were ranked (1 to 5, 6 and beyond, unranked), and how often a used memory was the first, or among the first three, in turns with a credit.",
		Action:     "Used memories ranked low call for a better ranking.",
	},
	{
		ID: "used.by_path", Label: "Used, by where memory was shown", Unit: "ratio", Direction: "up", MinN: 20,
		CI: "none", Scope: "machine", Source: "journal", Dev: true, Family: "quality", Series: "none", Command: "backsight stats quality --dev",
		Extra:      []string{"by_path"},
		Question:   "Which way of showing memory pays off?",
		Definition: "The share of confidently ranked memories that were used, for each way memory is shown (prompt, search and others). Each path needs its own 20 shows.",
		Action:     "Shows which channel earns its place and which one does not.",
	},
	{
		ID: "search.rescue_rate", Label: "Memories Claude found itself", Unit: "ratio", Direction: "none", MinN: 20,
		CI: "none", Scope: "machine", Source: "journal", Dev: true, Family: "quality", Series: "none", Command: "backsight stats quality --dev",
		Question:   "How much does Claude find that the prompt recall missed?",
		Definition: "Credits on the search path in turns where the prompt recall also showed memories, out of all credits in those turns.",
		Action:     "A high value means prompt recall misses what Claude then finds by searching.",
	},
	{
		ID: "verdict.wrong_rate", Label: "Memories reported wrong or out of date", Unit: "ratio", Direction: "down", MinN: 20,
		Absolute: Threshold{Watch: f(0.05), Bad: f(0.15), On: "lo"}, CI: "wilson_approx", Scope: "machine", Source: "journal", Family: "quality", Series: "daily",
		Command: "backsight memory stale", Drill: "backsight stats sessions --has wrong",
		Extra:      []string{"wrong", "outdated", "used"},
		Question:   "How many memories that Claude relied on were wrong?",
		Definition: "Wrong and out-of-date verdicts, out of used, wrong and out-of-date verdicts. Watched when even the interval's lower bound is above 5%, bad above 15%.",
		Action:     "Stale facts are in memory. Go through the reported list and forget or fix them.",
	},
	{
		ID: "stale.never_used", Label: "Often shown, never used", Unit: "count", Direction: "none", MinN: 0,
		Aggregation: "recent_weighted", CI: "none", Scope: "machine", Source: "ledger", Family: "quality", Series: "none", Command: "backsight memory stale",
		Question:   "Which memories are shown again and again for nothing?",
		Definition: "The memories shown often without ever being used, counted from the feedback ledger with recent feedback weighted more.",
		Action:     "Review them, then forget those that no longer help with backsight forget <id>.",
	},
	{
		ID: "stale.hidden", Label: "Hidden", Unit: "count", Direction: "none", MinN: 0,
		Aggregation: "recent_weighted", CI: "none", Scope: "machine", Source: "ledger", Family: "quality", Series: "none", Command: "backsight memory stale",
		Question:   "How many memories are hidden from recall?",
		Definition: "The memories hidden from recall, counted from the feedback ledger with recent feedback weighted more.",
		Action:     "Check that each is still better hidden; backsight forget --restore <id> brings one back.",
	},
	{
		ID: "stale.reported", Label: "Reported wrong", Unit: "count", Direction: "none", MinN: 0,
		Aggregation: "recent_weighted", CI: "none", Scope: "machine", Source: "ledger", Family: "quality", Series: "none", Command: "backsight memory stale",
		Question:   "How many memories were reported wrong?",
		Definition: "The memories Claude reported wrong or out of date, counted from the feedback ledger with recent feedback weighted more.",
		Action:     "Review them and forget those that are wrong with backsight forget <id>.",
	},
	{
		ID: "context.per_session", Label: "Context added per session", Unit: "chars", Direction: "down", MinN: 100,
		Absolute: Threshold{On: "p95"}, CI: "none", Scope: "machine", Source: "telemetry", Family: "cost", Series: "daily",
		Command: "backsight config set recall.session_budget_chars <n>", Drill: "backsight stats sessions",
		Extra:      []string{"p50", "p95", "tokens_p50", "tokens_p95", "histogram"},
		Question:   "How much does backsight add to Claude's context in a session?",
		Definition: "Characters backsight added to Claude's context per session (prompt recall plus briefing), typical (median) and high (slowest 1 in 20), with approximate tokens (characters divided by 4). Watched when the high value passes 1.5 times the session budget.",
		Action:     "Too much context per session. Lower the session budget.",
	},
	{
		ID: "context.per_prompt", Label: "Context added per prompt that got memory", Unit: "chars", Direction: "down", MinN: 20,
		CI: "none", Scope: "machine", Source: "telemetry", Family: "cost", Series: "none", Command: "backsight stats cost",
		Extra:      pctExtra,
		Question:   "How big is a recall when memory shows up?",
		Definition: "Characters injected by prompt recalls that put memory in front of Claude. The value is the typical (median) size; the high value (slowest 1 in 20) is given beside it once there are 100 recalls.",
		Action:     "Large recalls cost context without being asked for; lower the per-prompt caps.",
	},
	{
		ID: "recall.latency", Label: "Time memory adds to a prompt", Unit: "ms", Direction: "down", MinN: 100,
		Absolute: Threshold{Watch: f(2000), Bad: f(2500), On: "p95"}, CI: "none", Scope: "machine", Source: "telemetry", Family: "cost", Series: "daily",
		Command: "backsight doctor", Drill: "backsight stats days",
		Extra:      pctExtra,
		Question:   "How long does memory make a prompt wait?",
		Definition: "Wall time of the prompt recall, typical (median) and slowest 1 in 20. Watched above 2,000 ms, bad above 2,500 ms.",
		Action:     "The server or the embeddings are slow. Run backsight doctor.",
	},
	{
		ID: "search.latency", Label: "Time of a memory search", Unit: "ms", Direction: "down", MinN: 100,
		Absolute: Threshold{Watch: f(3000), On: "p95"}, CI: "none", Scope: "machine", Source: "telemetry", Family: "cost", Series: "daily",
		Command:    "backsight doctor",
		Extra:      pctExtra,
		Question:   "How long does a memory search take?",
		Definition: "Time of a memory_search call, typical (median) and slowest 1 in 20. Watched above 3,000 ms.",
		Action:     "The server is slow. Run backsight doctor.",
	},
	{
		ID: "extraction.tokens", Label: "What the server's model spent", Unit: "tokens", Direction: "none", MinN: 0,
		CI: "none", Scope: "server", Source: "server", Family: "cost", Series: "daily", Command: "backsight stats cost",
		Extra:      []string{"input", "cached", "output", "by_operation", "days_covered", "failed_operations"},
		Question:   "How many tokens does the server's model use to build memory?",
		Definition: "Tokens (input, of which cached, and output; the total is input plus output plus thoughts, which count with the output, so it is a little above the server's own total) the server's model spent per day and per operation, for all PCs using the server. The server keeps 30 days.",
		Action:     "Use it to see which operation costs the most before changing the model or the projects sent.",
	},
	{
		ID: "extraction.cost", Label: "What the server's model spent", Unit: "money", Direction: "none", MinN: 0,
		CI: "none", Scope: "server", Source: "server", Family: "cost", Series: "daily", Command: "backsight stats cost",
		Extra:      []string{"price_set", "currency", "by_operation", "failed_operations"},
		Question:   "How much does the server's model cost?",
		Definition: "The tokens spent, priced from the [llm] prices in your backsight config; it reads \"price not set\" when no price is set. For all PCs using the server.",
		Action:     "Set the model prices to see the cost; a high cost calls for a cheaper model or fewer projects sent.",
	},
	{
		ID: "extraction.per_session", Label: "Extraction per session sent", Unit: "tokens", Direction: "none", MinN: 0,
		CI: "none", Scope: "server", Source: "server", Dev: true, Family: "cost", Series: "none", Command: "backsight stats cost --dev",
		Extra:      []string{"sessions_sent", "window_days"},
		Question:   "How much does turning one session into memories cost?",
		Definition: "Extraction tokens divided by the sessions sent successfully, over a window of at least 28 days.",
		Action:     "A high value means sessions are costly to extract; look at which projects are sent.",
	},
	{
		ID: "bank.size", Label: "Memories", Unit: "count", Direction: "none", MinN: 0,
		CI: "none", Scope: "server", Source: "server", Family: "bank", Series: "none", Command: "backsight stats bank",
		Extra:      []string{"world", "experience", "observation", "documents", "links"},
		Question:   "How many memories are stored?",
		Definition: "Memories by kind (facts about you and your work, facts from sessions, summaries) and the documents they come from.",
		Action:     "No judgement; small memory explains a low share of prompts that got memory.",
	},
	{
		ID: "bank.growth", Label: "New memories per day", Unit: "count", Direction: "none", MinN: 0,
		CI: "none", Scope: "server", Source: "server", Family: "bank", Series: "daily", Command: "backsight stats bank",
		Extra:      []string{"world", "experience", "observation", "days_covered"},
		Question:   "Is memory still growing?",
		Definition: "Memories added per day, by kind, from the server's time series.",
		Action:     "A flat line while you work means memory is not learning; run backsight doctor.",
	},
	{
		ID: "bank.by_project", Label: "Memories per project", Unit: "count", Direction: "none", MinN: 0,
		CI: "none", Scope: "server", Source: "server", Family: "bank", Series: "none", Command: "backsight stats projects",
		Question:   "Which projects have memories?",
		Definition: "Memories per project, from the server's project tags.",
		Action:     "A project with no memories gets nothing from recall; check that its sessions are sent.",
	},
	{
		ID: "bank.consolidation", Label: "Summaries waiting", Unit: "count", Direction: "none", MinN: 0,
		ErrorClass: true, CI: "none", Scope: "server", Source: "server", Family: "bank", Series: "none", Command: "backsight doctor",
		Extra:      []string{"pending", "failed", "last_consolidated_at"},
		Question:   "Are the summaries being built?",
		Definition: "Summaries pending and failed, and when the last one was built. The failed count is the server's current total. Watched when any failed.",
		Action:     "Failed summaries need the server looked at. Run backsight doctor.",
	},
	{
		ID: "bank.churn", Label: "Retired, hidden, restored", Unit: "count", Direction: "none", MinN: 0,
		CI: "none", Scope: "machine", Source: "journal", Family: "bank", Series: "weekly", Command: "backsight memory stale",
		Extra:      []string{"forget", "retire", "hide", "unhide"},
		Question:   "How many memories were retired, hidden or restored?",
		Definition: "Forget, retire, hide and restore actions recorded per week.",
		Action:     "A spike means a clean-up or a problem in what is stored; review with backsight memory stale.",
	},
	{
		ID: "notes.stored", Label: "Facts stored", Unit: "count", Direction: "none", MinN: 0,
		CI: "none", Scope: "machine", Source: "telemetry", Family: "bank", Series: "none", Command: "backsight stats bank",
		Extra:      []string{"by_result", "by_kind"},
		Question:   "How many facts did Claude store?",
		Definition: "Store events by result (saved, updated, refused) and by kind.",
		Action:     "Many refusals mean Claude stores things the rules reject.",
	},
	{
		ID: "pipeline.flow", Label: "Sessions sent", Unit: "ratio", Direction: "up", MinN: 0,
		ErrorClass: true, CI: "wilson", Scope: "machine", Source: "telemetry", Family: "pipeline", Series: "daily",
		Command: "backsight doctor", Drill: "backsight stats pipeline",
		Extra:      []string{"ok", "failed", "set_aside", "waiting", "backlog_oldest_ms"},
		Question:   "Are finished sessions reaching the server?",
		Definition: "Documents whose last status is ok, out of documents with a send in the window. The interval counts documents (a session's documents per project), not independent sends. Failed, set aside and waiting ones are given beside it. Watched when any failed or was set aside; bad when the backlog is older than 24 hours.",
		Action:     "Sessions are not being sent. Run backsight doctor, then look at the pipeline.",
	},
	{
		ID: "retain.outcomes", Label: "Sends by result", Unit: "count", Direction: "none", MinN: 0,
		CI: "none", Scope: "machine", Source: "telemetry", Family: "pipeline", Series: "daily", Command: "backsight stats pipeline",
		Extra:      []string{"by_status", "p95_ms"},
		Question:   "How did each day's sends end?",
		Definition: "Per day, documents by their last status. The duration (slowest 1 in 20) counts successful sends only.",
		Action:     "Days with many failures show when sending broke.",
	},
	{
		ID: "local.backlog", Label: "Waiting on this PC", Unit: "count", Direction: "none", MinN: 0,
		ErrorClass: true, CI: "none", Scope: "machine", Source: "local", Family: "pipeline", Series: "none", Command: "backsight jobs tick",
		Extra:      []string{"notes_queued", "sessions_waiting", "quarantined", "oldest_ms"},
		Question:   "Is anything stuck on this PC?",
		Definition: "Notes queued, sessions waiting to be sent, items set aside, and the age of the oldest. Watched when the oldest is over 1 hour, bad over 24 hours.",
		Action:     "Work is not leaving this PC. Run backsight jobs tick.",
	},
	{
		ID: "server.queue", Label: "Waiting on the server", Unit: "count", Direction: "none", MinN: 0,
		ErrorClass: true, CI: "none", Scope: "server", Source: "server", Family: "pipeline", Series: "none", Command: "backsight doctor",
		Extra:      []string{"pending", "processing", "failed", "failed_total", "failed_capped"},
		Question:   "Is the server keeping up?",
		Definition: "Operations queued and running on the server, from its operations list. The failed count covers operations updated in the last 7 days, whatever the window; when the server holds more failed operations than are read (failed_capped), it is a lower bound. Watched when any failed.",
		Action:     "Failed operations need the server looked at. Run backsight doctor.",
	},
	{
		ID: "server.extraction_delay", Label: "Time to turn a session into memories", Unit: "ms", Direction: "down", MinN: 100,
		Absolute: Threshold{Watch: f(1800000), On: "p95"}, CI: "none", Scope: "server", Source: "server", Family: "pipeline", Series: "none",
		Command:    "backsight doctor",
		Extra:      pctExtra,
		Question:   "How long until a session becomes memories?",
		Definition: "Time between a completed operation's creation and its last update, typical (median) and slowest 1 in 20, over the last 200 in the window. Watched above 30 minutes.",
		Action:     "The server is slow to extract. Run backsight doctor.",
	},
	{
		ID: "hooks.failures", Label: "Steps that crashed", Unit: "count", Direction: "none", MinN: 0,
		ErrorClass: true, CI: "none", Scope: "machine", Source: "telemetry", Family: "pipeline", Series: "daily", Command: "backsight doctor",
		Extra:      []string{"by_kind", "last_24h"},
		Question:   "Are backsight's steps crashing?",
		Definition: "Hook failures by kind (panic, fatal, timeout, killed, exit, invalid output, input too large, spawn). In the last 24 hours, a panic, fatal, exit or spawn failure, or any kind three times, is watched; ten or more is bad.",
		Action:     "Steps are failing inside Claude Code sessions. Run backsight doctor.",
	},
	{
		ID: "server.reach", Label: "Memory server answering", Unit: "count", Direction: "none", MinN: 0,
		ErrorClass: true, CI: "none", Scope: "machine", Source: "local", Family: "pipeline", Series: "none", Command: "backsight doctor",
		Extra:      []string{"last_ok", "failing_since", "breaker_open_until", "skipped_prompts"},
		Question:   "Does the memory server answer this PC?",
		Definition: "The last answer, since when it is failing, and until when the breaker stays open, as this PC recorded them, with the prompts skipped while the breaker was open. Watched when failing or when the breaker is open; bad when a backlog older than 24 hours builds up with it.",
		Action:     "The server cannot be reached, so recall is skipped. Run backsight doctor.",
	},
	{
		ID: "doctor.check", Label: "Health checks", Unit: "count", Direction: "none", MinN: 0,
		ErrorClass: true, CI: "none", Scope: "machine", Source: "doctor", Family: "pipeline", Series: "none", Command: "backsight doctor",
		Extra:      []string{"ok", "info", "warn", "error"},
		Question:   "Do backsight's own health checks pass?",
		Definition: "The doctor's checks, one entry per check with its status. Watched when any check is an error.",
		Action:     "Fix the failing check; backsight doctor says how.",
	},
	{
		ID: "jobs.last_run", Label: "Background jobs", Unit: "ms", Direction: "none", MinN: 0,
		ErrorClass: true, CI: "none", Scope: "machine", Source: "telemetry", Family: "pipeline", Series: "none", Command: "backsight jobs tick",
		Extra:      []string{"tick", "maintain"},
		Question:   "Are the background jobs running?",
		Definition: "The last tick and maintain runs, with status and duration. Bad when a job is more than twice its cadence late and prompts or tool events happened since it was due (a sleeping laptop is not a fault).",
		Action:     "Background work is not running. Run backsight jobs tick.",
	},
})

// withDrills fills Drill with Command for the rows that have no drill of
// their own, and Aggregation with raw unless a row says otherwise.
func withDrills(defs []Def) []Def {
	for i := range defs {
		if defs[i].Drill == "" {
			defs[i].Drill = defs[i].Command
		}
		if defs[i].Aggregation == "" {
			defs[i].Aggregation = "raw"
		}
	}
	return defs
}

// DefOf returns the catalog entry of a metric id.
func DefOf(id string) (Def, bool) {
	for _, d := range Catalog {
		if d.ID == id {
			return d, true
		}
	}
	return Def{}, false
}
