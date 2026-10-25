package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/session"
)

type detectInput struct {
	Prompt        string `json:"prompt"`
	LastAssistant string `json:"last_assistant"`
	SubagentDone  bool   `json:"subagent_done"`
}

// newDetectReplayCmd runs the prompt hook's save detector over JSON lines
// (decision 0056 block 4: its follow-up is measured on replayed
// transcripts). No config, no server, no state.
func newDetectReplayCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "detect-replay",
		Short:  "Run the save detector over JSON lines {prompt, last_assistant, subagent_done} on stdin",
		Args:   usageArgs(cobra.NoArgs),
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return detectLines(cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}

func detectLines(in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	enc := json.NewEncoder(out)
	for n := 1; sc.Scan(); n++ {
		var d detectInput
		if err := json.Unmarshal(sc.Bytes(), &d); err != nil {
			return &output.Error{Code: output.Usage, Message: fmt.Sprintf("line %d: %v", n, err)}
		}
		if err := enc.Encode(map[string]string{"detector": detect(session.State{LastAssistant: d.LastAssistant, SubagentDone: d.SubagentDone}, d.Prompt)}); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return &output.Error{Code: output.Usage, Message: err.Error()}
	}
	return nil
}
