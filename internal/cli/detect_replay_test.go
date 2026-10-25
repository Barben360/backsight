package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/output"
)

// The replay command gives the hook's own detector verdict per line.
func TestDetectReplay(t *testing.T) {
	in := `{"prompt":"From now on we always squash merges."}
{"prompt":"ok","last_assistant":"Shall I merge it?"}
{"prompt":"ok","last_assistant":"Merged."}
{"prompt":"ok","last_assistant":"Merged.","subagent_done":true}
{"prompt":"What does the export do?"}
`
	var out strings.Builder
	if err := detectLines(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	want := `{"detector":"rule"}
{"detector":"approval"}
{"detector":""}
{"detector":"approval"}
{"detector":""}
`
	if out.String() != want {
		t.Errorf("got\n%s", out.String())
	}
	var e *output.Error
	if err := detectLines(strings.NewReader("not json\n"), &out); !errors.As(err, &e) || e.Code != output.Usage {
		t.Errorf("a bad line is a usage error: %v", err)
	}
}
