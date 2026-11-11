package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/fileimport"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/telemetry"
)

// Warnings an import plan can carry.
const (
	warnStrategyMissing   = "strategy_missing"
	warnBackfillPending   = "backfill_pending"
	warnServerUnreachable = "server_unreachable"
)

type filesImportFlags struct {
	plan, allowSubscription bool
	exclude, ext            []string
	project                 string
	from, codexHome         string
	maxCost                 float64
	maxTokens               int64
}

// filesImportResult is what `import <folder>` reports.
type filesImportResult struct {
	Plan      fileimport.Plan        `json:"plan"`
	Merge     fileimport.MergeResult `json:"merge"`
	Queued    bool                   `json:"queued"`
	MaxCost   float64                `json:"max_cost,omitempty"`
	MaxTokens int64                  `json:"max_tokens,omitempty"`
	Warnings  []string               `json:"warnings,omitempty"`
	// Reused: the settings saved with this import, used again because the
	// run gave none.
	Reused []string `json:"reused,omitempty"`
}

func newImportCmd(e *env) *cobra.Command {
	var fl filesImportFlags
	cmd := &cobra.Command{
		Use:   "import [<folder>]",
		Short: "Import a folder of text files, or Codex's own memories, into the memory, in the background (see the cost first with --plan)",
		Args:  usageArgs(cobra.RangeArgs(0, 1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder := ""
			if len(args) == 1 {
				folder = args[0]
			}
			return runFilesImport(cmd, e, folder, fl)
		},
	}
	cmd.Flags().StringVar(&fl.from, "from", "", "import another tool's memories instead of a folder: codex (each file to its project)")
	cmd.Flags().StringVar(&fl.codexHome, "codex-home", "", "with --from codex: the Codex home to read (default: the one setup recorded, else $CODEX_HOME or ~/.codex)")
	cmd.Flags().BoolVar(&fl.plan, "plan", false, "show what would be sent and what it would cost; send and write nothing")
	cmd.Flags().StringArrayVar(&fl.exclude, "exclude", nil, "skip paths matching this pattern, relative to the folder (** for any depth; repeatable)")
	cmd.Flags().StringArrayVar(&fl.ext, "ext", nil, "also read files with this extension, e.g. jsonl (repeatable)")
	cmd.Flags().StringVar(&fl.project, "project", "", "the project the memories belong to (default: the transverse project)")
	cmd.Flags().Float64Var(&fl.maxCost, "max-cost", 0, "spending cap in the provider's currency (default 1.5 × the estimate)")
	cmd.Flags().Int64Var(&fl.maxTokens, "max-tokens", 0, "token cap, for models without a price")
	cmd.Flags().BoolVar(&fl.allowSubscription, "allow-subscription", false, "accept importing on the Claude subscription lane")
	cmd.AddCommand(newImportListCmd(e), newImportPurgeCmd(e))
	return cmd
}

func runFilesImport(cmd *cobra.Command, e *env, folder string, fl filesImportFlags) error {
	ctx := cmd.Context()
	switch {
	case fl.from == "" && folder == "":
		return &output.Error{Code: output.Usage, Message: "give the folder to import, or --from codex for Codex's own memories"}
	case fl.from != "" && folder != "":
		return &output.Error{Code: output.Usage, Message: "give a folder or --from, not both"}
	case fl.from != "" && fl.from != fileimport.PresetCodex:
		return &output.Error{Code: output.Usage, Message: fmt.Sprintf("--from %q: the only tool known is codex", fl.from)}
	case fl.codexHome != "" && fl.from == "":
		return &output.Error{Code: output.Usage, Message: "--codex-home goes with --from codex"}
	case fl.from != "" && (cmd.Flags().Changed("project") || cmd.Flags().Changed("ext")):
		return &output.Error{Code: output.Usage, Message: "--from codex reads Codex's memory files only and sends each one to its own project: --project and --ext do not apply"}
	}
	preset := fl.from
	d, home, err := machine()
	if err != nil {
		return err
	}
	cfg, sec, err := loadServerConfig()
	if err != nil {
		return err
	}
	for _, g := range fl.exclude {
		if err := fileimport.ValidGlob(g); err != nil {
			return &output.Error{Code: output.Usage, Message: fmt.Sprintf("--exclude %q is not a valid pattern: %v", g, err)}
		}
	}
	if preset == fileimport.PresetCodex {
		st, err := state.Load(d.StateFile())
		if err != nil {
			return blocked(err)
		}
		if folder, err = importCodexHome(home, fl.codexHome, st, os.Getenv); err != nil {
			return err
		}
	}
	folder = expandHome(folder)
	if st, err := os.Stat(folder); err != nil || !st.IsDir() {
		return &output.Error{Code: output.Usage, Message: folder + " is not a folder"}
	}
	capGiven := cmd.Flags().Changed("max-cost") || cmd.Flags().Changed("max-tokens")
	prices := retain.PricesForLLM(cfg.LLM)
	if !fl.plan {
		if cfg.LLM.Provider == "claude-code" && !fl.allowSubscription {
			return &output.Error{Code: output.Usage, Message: "the server extracts with your Claude subscription: importing would send many files unattended, the automated use its terms restrict; rerun with --allow-subscription to accept"}
		}
		if state.MaintenanceHeld(d.MaintenanceLock()) {
			return &output.Error{Code: output.Blocked, Message: "maintenance in progress; run this again once it ends"}
		}
		if cmd.Flags().Changed("max-cost") && !prices.Known() {
			return &output.Error{Code: output.Usage, Message: "no price is known for model " + cfg.LLM.Model + ": cap with --max-tokens, or set [llm] price_input and price_output"}
		}
	}
	machineID := retain.ReadMachineID(d.State)
	if machineID == "" {
		if machineID, err = retain.MachineID(d.State); err != nil {
			return blocked(err)
		}
	}
	root, err := fileimport.ResolveRoot(folder)
	if err != nil {
		return &output.Error{Code: output.Usage, Message: err.Error()}
	}
	source := fileimport.SourceID(machineID, root)
	if preset != "" {
		source = fileimport.PresetSourceID(machineID, preset, root)
	}
	if !fl.plan {
		// One writer of the manifests at a time: a tick sending this folder
		// would write its older copy back over this merge.
		release, err := acquireImport(d, "import")
		if err != nil {
			return &output.Error{Code: output.Blocked, Message: "files of an import are being sent right now: run this again in a minute"}
		}
		defer release()
	}
	m, found, err := fileimport.LoadManifest(d.ImportDir(), source)
	if err != nil {
		return blocked(err)
	}
	res := filesImportResult{}
	exclude := fl.exclude
	exts := make([]string, 0, len(fl.ext))
	for _, x := range fl.ext {
		exts = append(exts, fileimport.NormalizeExt(x))
	}
	if found && !cmd.Flags().Changed("exclude") && len(m.Exclude) > 0 {
		exclude = m.Exclude
		for _, g := range exclude {
			res.Reused = append(res.Reused, "--exclude '"+g+"'")
		}
	}
	if found && preset == "" && !cmd.Flags().Changed("ext") && len(m.Ext) > 0 {
		exts = m.Ext
		for _, x := range exts {
			res.Reused = append(res.Reused, "--ext "+x)
		}
	}
	var scan fileimport.Scan
	if preset == fileimport.PresetCodex {
		scan, err = scanCodexImport(home, root, cfg.Projects.Transverse, cfg.Privacy.ExcludePaths, exclude)
	} else {
		scan, err = fileimport.ScanDir(root, fileimport.ScanOptions{Exclude: exclude, Ext: exts, Now: now()})
	}
	if err != nil {
		return &output.Error{Code: output.Usage, Message: err.Error()}
	}
	all, err := fileimport.ListManifests(d.ImportDir())
	if err != nil {
		return blocked(err)
	}
	scan.Drop(fileimport.HeldByOthers(all, source, scan.Root))
	st, err := state.Load(d.StateFile())
	if err != nil {
		return blocked(err)
	}
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, curateTimeout)
	if err != nil {
		return blocked(err)
	}
	unreachable := func(err error) error {
		if fl.plan {
			res.Warnings = appendOnce(res.Warnings, warnServerUnreachable)
			return nil
		}
		return blocked(fmt.Errorf("cannot reach the memory server: %w", err))
	}

	project := retain.CanonicalProject(cfg.Projects.Transverse, retain.TransverseKeyword)
	if fl.project != "" {
		project = retain.CanonicalProject(cfg.Projects.Transverse, fl.project)
	}
	if found && fl.project != "" && project != m.Project {
		return &output.Error{Code: output.Usage, Message: fmt.Sprintf("this folder was imported for project %s: remove it with backsight import purge %s first to import it for %s", m.Project, source, project)}
	}
	if !found {
		m = fileimport.Manifest{Schema: 1, Source: source, Root: scan.Root, Project: project, Preset: preset}
		docs, err := cl.ListDocuments(ctx, cfg.Server.Bank)
		if err != nil {
			if err := unreachable(err); err != nil {
				return err
			}
		}
		fileimport.SeedFromServer(&m, scan, docs)
	}
	m.Entries = append([]fileimport.Entry(nil), m.Entries...) // a plan must not touch the loaded copy's array
	res.Merge = m.Merge(scan, st.TombstonesOf(cfg.Server.Bank))
	unsent := map[string]bool{}
	for _, en := range m.Unsent() {
		unsent[en.Rel] = true
	}
	res.Plan = fileimport.BuildPlan(scan, func(f fileimport.File) bool { return unsent[f.Rel] }, source, m.Project, prices)
	if st.Backfill != nil && len(st.Backfill.Sessions) > 0 {
		res.Warnings = appendOnce(res.Warnings, warnBackfillPending)
	}
	if has, err := hasImportStrategy(ctx, cl, cfg.Server.Bank); err != nil {
		if err := unreachable(err); err != nil {
			return err
		}
	} else if !has {
		if fl.plan {
			res.Warnings = appendOnce(res.Warnings, warnStrategyMissing)
		} else if requireLocal(cfg, "import") != nil {
			return &output.Error{Code: output.Blocked, Message: "the server's memory settings do not know imported files yet: update backsight on the server's computer, then run this again"}
		} else if _, err := applyBank(ctx, d, cfg, sec, false); err != nil {
			return err
		}
	}
	tw := telemetry.New(d.State, telemetry.Common{BinaryVersion: buildinfo.Version, Project: res.Plan.Project})
	if fl.plan {
		tw.Emit(importEvent("plan", res.Plan))
		return e.emit("import", res, func() string { return filesImportText(res) })
	}

	if found && m.Paused == "budget" && !capGiven {
		return &output.Error{Code: output.Partial, Message: fmt.Sprintf("import paused: budget reached (%.4f %s spent); rerun with a higher --max-cost or --max-tokens", m.SpentCost, m.Currency)}
	}
	// Each run is a new spending window: what earlier runs spent is kept
	// apart, the cap is this run's, and the spend counts from its first
	// send (the tick sets the base then).
	if found {
		m.PriorCost, m.PriorTokens = m.PriorCost+m.SpentCost, m.PriorTokens+m.SpentTokens
	} else {
		m.QueuedAt = now().UTC()
	}
	m.SpentCost, m.SpentTokens, m.StartDay, m.StartBase = 0, 0, "", [4]int64{}
	m.MaxCost, m.MaxTokens = importCap(fl, res.Plan.Total, prices)
	m.Paused = ""
	m.Currency = prices.Currency
	m.AllowSubscription = m.AllowSubscription || fl.allowSubscription
	m.Ext, m.Exclude = exts, exclude
	if err := fileimport.SaveManifest(d.ImportDir(), m); err != nil {
		return blocked(err)
	}
	res.Queued, res.MaxCost, res.MaxTokens = true, m.MaxCost, m.MaxTokens
	tw.Emit(importEvent("queue", res.Plan))
	return e.emit("import", res, func() string { return filesImportText(res) })
}

// importCap is the cap a new or re-capped import gets: the flags, else 1.5
// times the estimate, in tokens for a model without a price.
func importCap(fl filesImportFlags, est retain.Estimate, prices retain.Prices) (float64, int64) {
	switch {
	case fl.maxCost > 0:
		return fl.maxCost, 0
	case fl.maxTokens > 0:
		return 0, fl.maxTokens
	case prices.Known() && est.Cost > 0:
		return 1.5 * est.Cost, 0
	}
	return 0, max((est.InputTokens+est.OutputTokens)*3/2, 1)
}

// hasImportStrategy reports whether the bank's settings name the import
// strategy (backsight's template is held in the bank's overrides).
func hasImportStrategy(ctx context.Context, cl *hindsight.Client, bank string) (bool, error) {
	ov, err := cl.BankConfigOverrides(ctx, bank)
	if err != nil {
		return false, err
	}
	var strategies map[string]json.RawMessage
	if raw, ok := ov["retain_strategies"]; ok {
		if err := json.Unmarshal(raw, &strategies); err != nil {
			return false, nil
		}
	}
	_, ok := strategies["import"]
	return ok, nil
}

func importEvent(action string, p fileimport.Plan) telemetry.Raw {
	ev := telemetry.Raw{Name: "import", Fields: map[string]any{"action": action, "source": p.Source, "files": p.Files,
		"chars": p.Total.Chars, "est_tokens": p.Total.InputTokens + p.Total.OutputTokens, "est_cost": p.Total.Cost, "skipped": p.Skipped}}
	if p.Preset != "" {
		ev.Fields["preset"], ev.Fields["projects"] = p.Preset, len(p.Projects)
	}
	return ev
}

func appendOnce(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

var skipLabels = map[string]string{
	fileimport.SkipExtension: "other file type", fileimport.SkipTooLarge: "over 1 MB", fileimport.SkipNotText: "not text",
	fileimport.SkipEmpty: "empty", fileimport.SkipExcluded: "excluded", fileimport.SkipHidden: "hidden",
	fileimport.SkipSymlink: "link", fileimport.SkipUnreadable: "unreadable", fileimport.SkipOtherSource: "already in another import",
	fileimport.SkipCodexWorking: "Codex working file", fileimport.SkipCodexSkills: "Codex skills",
	fileimport.SkipCodexNotes: "Codex note (not read yet)", fileimport.SkipCodexUnknown: "not a Codex memory file",
	fileimport.SkipRolloutOnDisk: "conversation still on disk (sent with past sessions)", fileimport.SkipBadHeader: "header unreadable",
	fileimport.SkipOptedOut: "folder kept out of memory", fileimport.SkipMixesExcluded: "holds a kept-out folder's work",
	fileimport.SkipDuplicate: "same as in the other store",
}

// skipOrder is the order the plan's text names skip reasons in.
var skipOrder = []string{fileimport.SkipExtension, fileimport.SkipTooLarge, fileimport.SkipNotText, fileimport.SkipEmpty,
	fileimport.SkipExcluded, fileimport.SkipHidden, fileimport.SkipSymlink, fileimport.SkipUnreadable, fileimport.SkipOtherSource,
	fileimport.SkipRolloutOnDisk, fileimport.SkipDuplicate, fileimport.SkipOptedOut, fileimport.SkipMixesExcluded, fileimport.SkipBadHeader,
	fileimport.SkipCodexWorking, fileimport.SkipCodexSkills, fileimport.SkipCodexNotes, fileimport.SkipCodexUnknown}

func filesImportText(r filesImportResult) string {
	var b strings.Builder
	p := r.Plan
	money := func(v float64) string { return importMoney(v, p.PricesKnown, p.Currency) }
	if p.Preset == fileimport.PresetCodex {
		fmt.Fprintf(&b, "Codex memories in %s (import %s; a conversation's summary goes to its folder's project, the rest to %s)\n", p.Root, p.Source, p.Project)
		if len(p.Stores) == 0 {
			b.WriteString("no Codex memory store here (memories_v2/ or memories/): Codex's memories may be off, or never ran\n")
		}
	} else {
		fmt.Fprintf(&b, "%s (import %s, project %s)\n", p.Root, p.Source, p.Project)
	}
	if p.Files == 0 {
		b.WriteString("nothing to send\n")
	} else {
		fmt.Fprintf(&b, "%-28s %6s %12s %8s %14s\n", "folder", "files", "characters", "facts", "cost")
		for _, f := range p.Folders {
			fmt.Fprintf(&b, "%-28s %6d %12d %8d %14s\n", truncate(f.Folder, 28), f.Files, f.Chars, f.Facts, money(f.Cost))
		}
		fmt.Fprintf(&b, "%-28s %6d %12d %8d %14s\n", "total", p.Files, p.Total.Chars, p.Facts, money(p.Total.Cost))
		if len(p.Projects) > 1 || p.Preset != "" {
			fmt.Fprintf(&b, "%-28s %6s %12s %8s %14s\n", "project", "files", "characters", "facts", "cost")
			for _, pp := range p.Projects {
				fmt.Fprintf(&b, "%-28s %6d %12d %8d %14s\n", truncate(pp.Project, 28), pp.Files, pp.Chars, pp.Facts, money(pp.Cost))
			}
		}
		fmt.Fprintf(&b, "facts are approximate: the server decides; %s of work on the server\n", importDuration(p.Total.Duration))
	}
	var skipped []string
	for _, k := range skipOrder {
		n := p.Skipped[k]
		if dirs := p.SkippedDirs[k]; dirs > 0 {
			skipped = append(skipped, fmt.Sprintf("%d %s folder(s)", dirs, skipLabels[k]))
		}
		if n > 0 {
			skipped = append(skipped, fmt.Sprintf("%d %s", n, skipLabels[k]))
		}
	}
	if len(skipped) > 0 {
		b.WriteString("skipped: " + strings.Join(skipped, ", ") + "\n")
	}
	if p.Files > 0 {
		fmt.Fprintf(&b, "dates: %d from the files' headers, %d from their modification time\n", p.DatesFromFrontmatter, p.DatesFromMtime)
	}
	if p.RedactedFiles > 0 {
		fmt.Fprintf(&b, "secrets hidden in %d file(s): %s\n", p.RedactedFiles, strings.Join(p.RedactedKinds, ", "))
	}
	if r.Merge.Unchanged+r.Merge.Changed+r.Merge.Gone+r.Merge.Moved > 0 {
		fmt.Fprintf(&b, "since the last import: %d new, %d changed, %d unchanged, %d no longer in the folder (kept in memory)\n",
			r.Merge.New, r.Merge.Changed, r.Merge.Unchanged, r.Merge.Gone)
		if r.Merge.Moved > 0 {
			fmt.Fprintf(&b, "%d file(s) now belong to another project: sent there again, what the old project drew from them cleared\n", r.Merge.Moved)
		}
	}
	if len(r.Reused) > 0 {
		b.WriteString("using the settings saved with this import: " + strings.Join(r.Reused, " ") + " (give --exclude or --ext to change them)\n")
	}
	for _, w := range r.Warnings {
		switch w {
		case warnBackfillPending:
			b.WriteString("past sessions are still being sent: the import waits for them; what both hold is merged by the server\n")
		case warnStrategyMissing:
			b.WriteString("the memory server does not know imported files yet: it is updated when you queue the import (on a client, update backsight on the server's computer first)\n")
		case warnServerUnreachable:
			b.WriteString("the memory server could not be reached: files already imported may be counted again\n")
		}
	}
	if r.Queued {
		limit := fmt.Sprintf("%d tokens", r.MaxTokens)
		if r.MaxCost > 0 {
			limit = fmt.Sprintf("%.2f %s", r.MaxCost, p.Currency)
		}
		fmt.Fprintf(&b, "Queued: it is sent in the background, within %s. Follow it with `backsight import list`.\n", limit)
	}
	return b.String()
}

// importMoney is a cost for the plan's text.
func importMoney(v float64, known bool, currency string) string {
	switch {
	case !known:
		return "price unknown"
	case v > 0 && v < 0.005:
		return "< 0.01 " + currency
	}
	return fmt.Sprintf("%.2f %s", v, currency)
}

// importDuration is the plan's work time, to the minute.
func importDuration(d time.Duration) string {
	if d < time.Minute {
		return "under a minute"
	}
	return "about " + strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
}

// truncate shortens s to n runes with "…".
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// filesImportSource is one imported folder, as `import list` shows it.
type filesImportSource struct {
	Source    string  `json:"source"`
	Root      string  `json:"root"`
	Project   string  `json:"project"`
	Preset    string  `json:"preset,omitempty"` // codex: Codex's memories, each file to its project
	Files     int     `json:"files"`
	Sent      int     `json:"sent"`
	Gone      int     `json:"gone"`
	Status    string  `json:"status"`           // sending | waiting | paused | done
	Reason    string  `json:"reason,omitempty"` // past_sessions | budget | subscription
	SpentCost float64 `json:"spent_cost"`
	MaxCost   float64 `json:"max_cost,omitempty"`
	MaxTokens int64   `json:"max_tokens,omitempty"`
	Currency  string  `json:"currency,omitempty"`
}

func newImportListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show each imported folder: files sent, what is left and why",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, _, err := machine()
			if err != nil {
				return err
			}
			cfg, _, err := loadServerConfig()
			if err != nil {
				return err
			}
			all, err := fileimport.ListManifests(d.ImportDir())
			if err != nil {
				return blocked(err)
			}
			st, err := state.Load(d.StateFile())
			if err != nil {
				return blocked(err)
			}
			pending := st.Backfill != nil && len(st.Backfill.Sessions) > 0
			out := make([]filesImportSource, 0, len(all))
			for _, m := range all {
				s := filesImportSource{Source: m.Source, Root: m.Root, Project: m.Project, Preset: m.Preset, Files: len(m.Entries),
					SpentCost: m.PriorCost + m.SpentCost, MaxCost: m.MaxCost, MaxTokens: m.MaxTokens, Currency: m.Currency}
				for _, en := range m.Entries {
					if en.Sent {
						s.Sent++
					}
					if en.Gone {
						s.Gone++
					}
				}
				switch {
				case len(m.Unsent()) == 0:
					s.Status = "done"
				case m.Paused != "":
					s.Status, s.Reason = "paused", m.Paused
				case cfg.LLM.Provider == "claude-code" && !m.AllowSubscription:
					s.Status, s.Reason = "paused", "subscription"
				case pending:
					s.Status, s.Reason = "waiting", "past_sessions"
				default:
					s.Status = "sending"
				}
				out = append(out, s)
			}
			return e.emit("import list", out, func() string { return importListText(out) })
		},
	}
}

func importListText(all []filesImportSource) string {
	if len(all) == 0 {
		return "no folder imported\n"
	}
	reasons := map[string]string{"past_sessions": "waits for past sessions to be sent", "budget": "spending cap reached: rerun backsight import <folder> with a higher --max-cost",
		"subscription": "rerun backsight import <folder> --allow-subscription to send on your Claude subscription"}
	var b strings.Builder
	for _, s := range all {
		if s.Preset == fileimport.PresetCodex {
			fmt.Fprintf(&b, "%s  Codex memories in %s (project %s, or a conversation's own): %d/%d sent", s.Source, s.Root, s.Project, s.Sent, s.Files)
		} else {
			fmt.Fprintf(&b, "%s  %s (project %s): %d/%d sent", s.Source, s.Root, s.Project, s.Sent, s.Files)
		}
		if s.Gone > 0 {
			fmt.Fprintf(&b, ", %d no longer in the folder", s.Gone)
		}
		fmt.Fprintf(&b, ", %s", s.Status)
		if r := reasons[s.Reason]; r != "" {
			if s.Preset != "" {
				r = strings.ReplaceAll(r, "backsight import <folder>", "backsight import --from "+s.Preset)
			}
			b.WriteString(": " + r)
		}
		if s.Currency != "" {
			fmt.Fprintf(&b, "; spent %.2f %s", s.SpentCost, s.Currency)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// importPurgeResult is what `import purge` deletes or would delete.
type importPurgeResult struct {
	Source    string     `json:"source"`
	Root      string     `json:"root,omitempty"`
	Documents []purgeDoc `json:"documents"`
	Purged    bool       `json:"purged"`
	Note      string     `json:"note,omitempty"`
}

func newImportPurgeCmd(e *env) *cobra.Command {
	var plan, yes bool
	cmd := &cobra.Command{
		Use:   "purge <import id>",
		Short: "Delete for good everything an imported folder put in the memory",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			d, _, err := machine()
			if err != nil {
				return err
			}
			all, err := fileimport.ListManifests(d.ImportDir())
			if err != nil {
				return blocked(err)
			}
			source, root, err := resolveImportSource(args[0], all)
			if err != nil {
				return err
			}
			if yes && !plan {
				// Nothing is sent while the documents are listed and deleted.
				release, err := acquireImport(d, "import purge")
				if err != nil {
					return &output.Error{Code: output.Blocked, Message: "files of an import are being sent right now: run this again in a minute"}
				}
				defer release()
			}
			cl, bank, gate, err := curateWriter()
			if err != nil {
				return err
			}
			ids := map[string]bool{}
			prefix := fileimport.DocumentPrefix + source + "/"
			docs, err := cl.ListDocuments(ctx, bank)
			if err != nil {
				return blocked(err)
			}
			for _, doc := range docs {
				if strings.HasPrefix(doc.ID, prefix) {
					ids[doc.ID] = true
				}
			}
			project := ""
			for _, m := range all {
				if m.Source == source {
					project = m.Project
					for _, en := range m.Entries {
						ids[en.Document] = true
					}
				}
			}
			list := make([]string, 0, len(ids))
			for id := range ids {
				list = append(list, id)
			}
			res := importPurgeResult{Source: source, Root: root}
			if res.Documents, err = purgeDocumentsByID(ctx, cl, bank, list, true); err != nil {
				return blocked(err)
			}
			if res.Documents == nil {
				res.Documents = []purgeDoc{}
			}
			if plan {
				return e.emit("import purge", res, func() string { return importPurgeText(res) })
			}
			if !yes {
				return e.report("import purge", res, func() string { return importPurgeText(res) }, &output.Error{Code: output.NeedsUser,
					Message: "purging deletes the documents listed (every memory extracted from them): confirm with backsight import purge " + source + " --yes",
					Actions: []output.Action{{Kind: "run_as_user", Cmd: "backsight import purge " + source + " --yes", Why: "delete for good what is listed"}}})
			}
			if len(res.Documents) > 0 {
				_, mirror, err := purgeWrite(ctx, cl, bank, gate, nil, res.Documents, nil, "run the same command again to finish")
				if err != nil {
					return err
				}
				res.Note = mirror
			}
			if err := fileimport.DeleteManifest(d.ImportDir(), source); err != nil {
				return blocked(err)
			}
			res.Purged = true
			telemetry.New(d.State, telemetry.Common{BinaryVersion: buildinfo.Version, Project: project}).Emit(telemetry.Raw{Name: "import",
				Fields: map[string]any{"action": "purge", "source": source, "documents": len(res.Documents)}})
			return e.emit("import purge", res, func() string { return importPurgeText(res) })
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "list what would be deleted; delete nothing")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the deletion")
	return cmd
}

// resolveImportSource finds the import an id or a unique prefix of at
// least 4 characters names.
func resolveImportSource(arg string, all []fileimport.Manifest) (source, root string, err error) {
	if len(arg) < 4 {
		return "", "", &output.Error{Code: output.Usage, Message: "give at least 4 characters of the import id (backsight import list shows them)"}
	}
	var matches []fileimport.Manifest
	for _, m := range all {
		if strings.HasPrefix(m.Source, arg) {
			matches = append(matches, m)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].Source, matches[0].Root, nil
	case 0:
		if len(arg) == 12 {
			return arg, "", nil // the manifest is gone; the server may still hold its documents
		}
		return "", "", &output.Error{Code: output.Usage, Message: "no import id starts with " + arg + " (backsight import list shows them)"}
	}
	var names []string
	for _, m := range matches {
		names = append(names, m.Source+" ("+m.Root+")")
	}
	return "", "", &output.Error{Code: output.Usage, Message: arg + " names several imports: " + strings.Join(names, ", ")}
}

func importPurgeText(r importPurgeResult) string {
	var b strings.Builder
	verb := "would delete"
	if r.Purged {
		verb = "deleted"
	}
	fmt.Fprintf(&b, "import %s %s: %s %d document(s)\n", r.Source, r.Root, verb, len(r.Documents))
	for _, doc := range r.Documents {
		fmt.Fprintf(&b, "  %s (%d memories)\n", doc.ID, doc.Memories)
	}
	if r.Note != "" {
		b.WriteString(r.Note + "\n")
	}
	return b.String()
}
