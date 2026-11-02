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
}

func newImportCmd(e *env) *cobra.Command {
	var fl filesImportFlags
	cmd := &cobra.Command{
		Use:   "import <folder>",
		Short: "Import a folder of text files into the memory, in the background (see the cost first with --plan)",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFilesImport(cmd, e, args[0], fl)
		},
	}
	cmd.Flags().BoolVar(&fl.plan, "plan", false, "show what would be sent and what it would cost; send and write nothing")
	cmd.Flags().StringArrayVar(&fl.exclude, "exclude", nil, "skip paths matching this pattern, relative to the folder (** for any depth; repeatable)")
	cmd.Flags().StringArrayVar(&fl.ext, "ext", nil, "also read files with this extension, e.g. jsonl (repeatable)")
	cmd.Flags().StringVar(&fl.project, "project", "", "the project the memories belong to (default: the transverse project)")
	cmd.Flags().Float64Var(&fl.maxCost, "max-cost", 0, "spending cap in the provider's currency (default 1.5 × the estimate)")
	cmd.Flags().Int64Var(&fl.maxTokens, "max-tokens", 0, "token cap, for models without a price")
	cmd.Flags().BoolVar(&fl.allowSubscription, "allow-subscription", false, "accept importing on the Claude subscription lane")
	return cmd
}

func runFilesImport(cmd *cobra.Command, e *env, folder string, fl filesImportFlags) error {
	ctx := cmd.Context()
	d, _, err := machine()
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
	if st, err := os.Stat(folder); err != nil || !st.IsDir() {
		return &output.Error{Code: output.Usage, Message: folder + " is not a folder"}
	}
	capGiven := cmd.Flags().Changed("max-cost") || cmd.Flags().Changed("max-tokens")
	prices := retain.PricesFor(cfg.LLM.Model, cfg.LLM.PriceInput, cfg.LLM.PriceCached, cfg.LLM.PriceOutput, cfg.LLM.Currency)
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
	exts := make([]string, 0, len(fl.ext))
	for _, x := range fl.ext {
		exts = append(exts, fileimport.NormalizeExt(x))
	}
	scan, err := fileimport.ScanDir(folder, fileimport.ScanOptions{Exclude: fl.exclude, Ext: exts, Now: now()})
	if err != nil {
		return &output.Error{Code: output.Usage, Message: err.Error()}
	}
	source := fileimport.SourceID(machineID, scan.Root)
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
	res := filesImportResult{}
	unreachable := func(err error) error {
		if fl.plan {
			res.Warnings = appendOnce(res.Warnings, warnServerUnreachable)
			return nil
		}
		return blocked(fmt.Errorf("cannot reach the memory server: %w", err))
	}

	m, found, err := fileimport.LoadManifest(d.ImportDir(), source)
	if err != nil {
		return blocked(err)
	}
	project := retain.CanonicalProject(cfg.Projects.Transverse, retain.TransverseKeyword)
	if fl.project != "" {
		project = retain.CanonicalProject(cfg.Projects.Transverse, fl.project)
	}
	if found && fl.project != "" && project != m.Project {
		return &output.Error{Code: output.Usage, Message: fmt.Sprintf("this folder was imported for project %s: remove it with backsight import purge %s first to import it for %s", m.Project, source, project)}
	}
	if !found {
		m = fileimport.Manifest{Schema: 1, Source: source, Root: scan.Root, Project: project}
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
	tw := telemetry.New(d.State, telemetry.Common{BinaryVersion: buildinfo.Version})
	if fl.plan {
		tw.Emit(importEvent("plan", res.Plan))
		return e.emit("import", res, func() string { return filesImportText(res) })
	}

	if found && m.Paused == "budget" && !capGiven {
		return &output.Error{Code: output.Partial, Message: fmt.Sprintf("import paused: budget reached (%.4f %s spent); rerun with a higher --max-cost or --max-tokens", m.SpentCost, m.Currency)}
	}
	if !found {
		day := now().UTC().Format("2006-01-02")
		days, err := cl.LLMTokensByDay(ctx, cfg.Server.Bank, "30d")
		if err != nil {
			return blocked(fmt.Errorf("cannot read the server's LLM usage, so the spending cap could not work: %w", err))
		}
		t := days[day]
		m.QueuedAt, m.StartDay, m.StartBase = now().UTC(), day, [4]int64{t.Input, t.Cached, t.Output, t.Thoughts}
	}
	if !found || capGiven {
		m.MaxCost, m.MaxTokens = importCap(fl, res.Plan.Total, prices)
		m.Paused = ""
	}
	m.Currency = prices.Currency
	m.AllowSubscription = m.AllowSubscription || fl.allowSubscription
	m.Ext, m.Exclude = exts, fl.exclude
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
	case prices.Known():
		return 1.5 * est.Cost, 0
	}
	return 0, (est.InputTokens + est.OutputTokens) * 3 / 2
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
	return telemetry.Raw{Name: "import", Fields: map[string]any{"action": action, "source": p.Source, "files": p.Files,
		"chars": p.Total.Chars, "est_tokens": p.Total.InputTokens + p.Total.OutputTokens, "est_cost": p.Total.Cost, "skipped": p.Skipped}}
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
}

func filesImportText(r filesImportResult) string {
	var b strings.Builder
	p := r.Plan
	money := func(v float64) string {
		if !p.PricesKnown {
			return "price unknown"
		}
		return fmt.Sprintf("%.2f %s", v, p.Currency)
	}
	fmt.Fprintf(&b, "%s (import %s, project %s)\n", p.Root, p.Source, p.Project)
	if p.Files == 0 {
		b.WriteString("nothing to send\n")
	} else {
		fmt.Fprintf(&b, "%-28s %6s %12s %8s %14s\n", "folder", "files", "characters", "facts", "cost")
		for _, f := range p.Folders {
			fmt.Fprintf(&b, "%-28s %6d %12d %8d %14s\n", truncate(f.Folder, 28), f.Files, f.Chars, f.Facts, money(f.Cost))
		}
		fmt.Fprintf(&b, "%-28s %6d %12d %8d %14s\n", "total", p.Files, p.Total.Chars, p.Facts, money(p.Total.Cost))
		fmt.Fprintf(&b, "facts are approximate: the server decides; about %s of work\n", p.Total.Duration.Round(time.Minute))
	}
	var skipped []string
	for _, k := range []string{fileimport.SkipExtension, fileimport.SkipTooLarge, fileimport.SkipNotText, fileimport.SkipEmpty,
		fileimport.SkipExcluded, fileimport.SkipHidden, fileimport.SkipSymlink, fileimport.SkipUnreadable, fileimport.SkipOtherSource} {
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
	if r.Merge.Unchanged+r.Merge.Changed+r.Merge.Gone > 0 {
		fmt.Fprintf(&b, "since the last import: %d new, %d changed, %d unchanged, %d no longer in the folder (kept in memory)\n",
			r.Merge.New, r.Merge.Changed, r.Merge.Unchanged, r.Merge.Gone)
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

// truncate shortens s to n runes with "…".
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

