package cli

import (
	"cmp"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/output"
)

// replayMovers is how many of the biggest boost movers replay lists.
const replayMovers = 10

// replayMover is one memory's boost under the current parameters and
// under the ones set.
type replayMover struct {
	ID     string  `json:"id"`
	Before float64 `json:"before"`
	After  float64 `json:"after"`
}

// replayRecalls counts the journaled recalls ordered again under both
// parameter sets: those whose set changes (a prompt's top
// recall.prompt_hits), those whose order alone changes, and those whose
// hits lack the score without the boost (journaled by an earlier
// version).
type replayRecalls struct {
	Compared      int `json:"compared"`
	SetChanged    int `json:"set_changed"`
	OrderChanged  int `json:"order_changed"`
	NotComparable int `json:"not_comparable"`
}

type replayResult struct {
	At             time.Time          `json:"at"` // the boosts are read at it
	Set            map[string]float64 `json:"set"`
	Memories       int                `json:"memories"`
	TurnedNegative []string           `json:"turned_negative"`
	TurnedPositive []string           `json:"turned_positive"`
	Movers         []replayMover      `json:"movers"`
	Recalls        replayRecalls      `json:"recalls"`
	Skipped        int                `json:"skipped,omitempty"` // journal lines that did not parse
}

func newFeedbackReplayCmd(e *env) *cobra.Command {
	var sets []string
	cmd := &cobra.Command{
		Use:   "replay",
		Short: "Measure a change of the feedback parameters on the journal's history, writing nothing",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(*cobra.Command, []string) error {
			dir, cfg, err := feedbackConfig()
			if err != nil {
				return err
			}
			changed := cfg
			set := map[string]float64{}
			for _, kv := range sets {
				key, v, err := setFeedbackKey(&changed.Feedback, kv)
				if err != nil {
					return &output.Error{Code: output.Usage, Message: err.Error()}
				}
				set[key] = v
			}
			if err := changed.Validate(); err != nil {
				return &output.Error{Code: output.Usage, Message: "--set: " + err.Error()}
			}
			events, skipped, err := feedback.Read(dir, time.Time{})
			if err != nil {
				return blocked(err)
			}
			at := now().UTC()
			p, q := feedbackParams(cfg), feedbackParams(changed)
			res := compareReplays(events, feedback.Replay(events, p, at), feedback.Replay(events, q, at), p, q, at, cfg.Recall.PromptHits)
			res.Set, res.Skipped = set, skipped
			return e.emit("feedback replay", res, func() string { return replayText(res) })
		},
	}
	cmd.Flags().StringArrayVar(&sets, "set", nil, "a parameter to change, key=value (repeatable): "+strings.Join(slices.Sorted(maps.Keys(replayKeys)), ", "))
	return cmd
}

// replayKeys are the [feedback] keys replay can change, each with its
// field: a float or a whole number.
var replayKeys = map[string]func(f *config.Feedback) (*float64, *int){
	"boost_weight":          func(f *config.Feedback) (*float64, *int) { return &f.BoostWeight, nil },
	"prior_credits":         func(f *config.Feedback) (*float64, *int) { return &f.PriorCredits, nil },
	"prior_misses":          func(f *config.Feedback) (*float64, *int) { return &f.PriorMisses, nil },
	"confident_rank":        func(f *config.Feedback) (*float64, *int) { return nil, &f.ConfidentRank },
	"miss_half_life_days":   func(f *config.Feedback) (*float64, *int) { return nil, &f.MissHalfLifeDays },
	"credit_half_life_days": func(f *config.Feedback) (*float64, *int) { return nil, &f.CreditHalfLifeDays },
}

// setFeedbackKey sets in f the key=value kv names, a replay key with a
// finite number (a whole one for an integer key); Validate judges it.
func setFeedbackKey(f *config.Feedback, kv string) (string, float64, error) {
	key, raw, ok := strings.Cut(kv, "=")
	if !ok {
		return "", 0, fmt.Errorf("--set %s: give key=value", kv)
	}
	field, known := replayKeys[key]
	if !known {
		return "", 0, fmt.Errorf("--set %s: %q is not a key replay can change (%s)", kv, key, strings.Join(slices.Sorted(maps.Keys(replayKeys)), ", "))
	}
	fl, in := field(f)
	if in != nil {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return "", 0, fmt.Errorf("--set %s: %q is not a whole number", kv, raw)
		}
		*in = n
		return key, float64(n), nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return "", 0, fmt.Errorf("--set %s: %q is not a number", kv, raw)
	}
	*fl = v
	return key, v, nil
}

// compareReplays compares the boosts of the ledgers replayed under p
// (before) and q (after), both read at t: the memories whose boost
// changes sign, the biggest movers, and the journaled recalls, prompt and
// gate shows ordered again by score without the boost plus each boost.
func compareReplays(events []feedback.Event, before, after feedback.Ledger, p, q feedback.Params, t time.Time, promptHits int) replayResult {
	res := replayResult{At: t, TurnedNegative: []string{}, TurnedPositive: []string{}, Movers: []replayMover{}}
	ids := map[string]bool{}
	for id := range before.Memories {
		ids[id] = true
	}
	for id := range after.Memories {
		ids[id] = true
	}
	res.Memories = len(ids)
	for _, id := range slices.Sorted(maps.Keys(ids)) {
		b, a := round4(before.Boost(id, p, t)), round4(after.Boost(id, q, t))
		switch {
		case b > 0 && a < 0:
			res.TurnedNegative = append(res.TurnedNegative, id)
		case b < 0 && a > 0:
			res.TurnedPositive = append(res.TurnedPositive, id)
		}
		if a != b {
			res.Movers = append(res.Movers, replayMover{ID: id, Before: b, After: a})
		}
	}
	slices.SortStableFunc(res.Movers, func(x, y replayMover) int {
		return cmp.Compare(math.Abs(y.After-y.Before), math.Abs(x.After-x.Before))
	})
	res.Movers = res.Movers[:min(len(res.Movers), replayMovers)]

	for _, e := range events {
		prompt := e.Kind == "shows" && e.Path == "prompt"
		recalled := prompt || e.Kind == feedback.KindRecall || e.Kind == "shows" && e.Path == "gate"
		if !recalled || e.TS.After(t) || len(e.Items) == 0 {
			continue
		}
		if slices.ContainsFunc(e.Items, func(it feedback.Item) bool { return it.ScoreWithoutBoost == nil }) {
			res.Recalls.NotComparable++
			continue
		}
		x, y := boostedOrder(e.Items, before, p, t), boostedOrder(e.Items, after, q, t)
		if prompt && len(x) > promptHits {
			x, y = x[:promptHits], y[:promptHits]
		}
		res.Recalls.Compared++
		switch {
		case !slices.Equal(slices.Sorted(slices.Values(x)), slices.Sorted(slices.Values(y))):
			res.Recalls.SetChanged++
		case !slices.Equal(x, y):
			res.Recalls.OrderChanged++
		}
	}
	return res
}

// boostedOrder is the ids of items as recall would order them under l:
// best score without the boost plus l's boost first, both to 4 decimals,
// a tie by id.
func boostedOrder(items []feedback.Item, l feedback.Ledger, p feedback.Params, t time.Time) []string {
	type scored struct {
		id    string
		score float64
	}
	all := make([]scored, 0, len(items))
	for _, it := range items {
		all = append(all, scored{it.ID, round4(*it.ScoreWithoutBoost + round4(l.Boost(it.ID, p, t)))})
	}
	slices.SortFunc(all, func(a, b scored) int {
		if c := cmp.Compare(b.score, a.score); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})
	ids := make([]string, len(all))
	for i, s := range all {
		ids[i] = s.id
	}
	return ids
}

// round4 rounds v to 4 decimals; + 0 turns a negative zero into 0.
func round4(v float64) float64 { return math.Round(v*10000)/10000 + 0 }

func replayText(r replayResult) string {
	var b strings.Builder
	with := "the current parameters"
	if len(r.Set) > 0 {
		var kv []string
		for _, k := range slices.Sorted(maps.Keys(r.Set)) {
			kv = append(kv, k+"="+strconv.FormatFloat(r.Set[k], 'f', -1, 64))
		}
		with = strings.Join(kv, ", ") + " against the current parameters"
	}
	fmt.Fprintf(&b, "Replay of the journal with %s (boosts as of %s UTC), nothing written\n", with, r.At.Format("2006-01-02 15:04"))
	fmt.Fprintf(&b, "%d memories, %d turning negative, %d turning positive\n", r.Memories, len(r.TurnedNegative), len(r.TurnedPositive))
	if len(r.Movers) == 0 {
		b.WriteString("no boost moves\n")
	} else {
		fmt.Fprintf(&b, "%-10s  %8s  %8s\n", "moves most", "before", "after")
		for _, m := range r.Movers {
			fmt.Fprintf(&b, "%-10s  %+8.4f  %+8.4f\n", shortID(m.ID), m.Before, m.After)
		}
	}
	rc := r.Recalls
	fmt.Fprintf(&b, "recalls: %d compared, %d would change their hits, %d their order only", rc.Compared, rc.SetChanged, rc.OrderChanged)
	if rc.NotComparable > 0 {
		fmt.Fprintf(&b, "; %d journaled by an earlier version, without the scores to compare", rc.NotComparable)
	}
	b.WriteString("\n")
	if r.Skipped > 0 {
		fmt.Fprintf(&b, "%d journal line(s) skipped: torn by a crash, or written by another version\n", r.Skipped)
	}
	return b.String()
}
