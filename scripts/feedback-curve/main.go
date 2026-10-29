// Command feedback-curve writes the README's chart of how a memory's rank
// moves, docs/assets/feedback-curve-light.svg and -dark.svg, from the
// configuration's defaults. Run it from the repository root:
//
//	go run ./scripts/feedback-curve
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/paths"
)

func main() {
	f := config.Default(paths.Dirs{}, "").Feedback
	p := feedback.Params{Weight: f.BoostWeight, PriorCredits: f.PriorCredits, PriorMisses: f.PriorMisses}
	for _, theme := range []string{"light", "dark"} {
		path := filepath.Join("docs", "assets", "feedback-curve-"+theme+".svg")
		if err := os.WriteFile(path, feedback.CurveSVG(p, theme), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "feedback-curve:", err)
			os.Exit(1)
		}
		fmt.Println("wrote", path)
	}
}
