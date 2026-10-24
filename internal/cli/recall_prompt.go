package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/recall"
)

type recallPromptResult struct {
	Project  string       `json:"project"`
	Query    string       `json:"query"`
	Ranked   []recall.Hit `json:"ranked"`   // after the floors, best first (top 10)
	Injected []string     `json:"injected"` // what the hook would inject, before seen/sticky
	Block    string       `json:"block"`
}

// newRecallPromptCmd is the replay gate's view of the prompt path (decision
// 0054, E23): the hook's query, scopes, floors and selection for one prompt
// typed in cwd, without session state or sticky muting. Hidden.
func newRecallPromptCmd(e *env) *cobra.Command {
	var cwd string
	var fromStdin bool
	cmd := &cobra.Command{
		Use:    "recall-prompt --cwd <dir> [<prompt> | --stdin]",
		Short:  "What prompt recall finds for one prompt (replay gate)",
		Args:   usageArgs(cobra.ArbitraryArgs),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			input, _ := json.Marshal(map[string]string{"cwd": cwd})
			h, ok := newHookEnv(cmd.Context(), input, "")
			if !ok {
				return &output.Error{Code: output.NeedsUser, Message: "no usable config: run backsight server install"}
			}
			prompt := strings.Join(args, " ")
			if fromStdin { // keeps the prompt out of the process list
				raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
				if err != nil {
					return blocked(err)
				}
				prompt = string(raw)
			}
			if strings.TrimSpace(prompt) == "" {
				return &output.Error{Code: output.Usage, Message: "no prompt: give it as arguments or with --stdin"}
			}
			q, res := h.searchPrompt(prompt)
			block, chosen := selectHits(res.Hits, h.cfg.Recall, func(recall.Hit) bool { return false })
			out := recallPromptResult{Project: h.project, Query: q, Ranked: res.Hits, Injected: []string{}, Block: block}
			if len(out.Ranked) > 10 {
				out.Ranked = out.Ranked[:10]
			}
			if out.Ranked == nil {
				out.Ranked = []recall.Hit{}
			}
			for _, hit := range chosen {
				out.Injected = append(out.Injected, hit.ID)
			}
			return e.emit("recall-prompt", out, func() string {
				return fmt.Sprintf("project %s, %d ranked, %d injected\n%s\n", out.Project, len(out.Ranked), len(out.Injected), out.Block)
			})
		},
	}
	cmd.Flags().StringVar(&cwd, "cwd", "", "the directory the prompt was typed in (default: here)")
	cmd.Flags().BoolVar(&fromStdin, "stdin", false, "read the prompt from stdin")
	return cmd
}
