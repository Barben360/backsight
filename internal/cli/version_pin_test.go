package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/output"
)

// The release check reads the pin a binary runs with (decision 0067
// block review).
func TestVersionReportsTheHindsightPin(t *testing.T) {
	pinImage(t, newImage)
	code, out, _ := run(t, "version", "--json")
	var v struct {
		Data struct {
			HindsightImage string `json:"hindsight_image"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || code != output.OK || v.Data.HindsightImage != newImage {
		t.Errorf("exit %d, %v\n%s", code, err, out)
	}
	if code, out, _ := run(t, "version"); code != output.OK || !strings.Contains(out, "Hindsight "+newImage) {
		t.Errorf("text: exit %d\n%s", code, out)
	}
}
