package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/signing"
)

type versionInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	// SigningKeys: the keys this binary trusts for releases; TestKey is
	// true in a test build, which a release must never be (decision 0067).
	SigningKeys []signing.Key `json:"signing_keys"`
	TestKey     bool          `json:"test_key"`
}

func newVersionCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the backsight version",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			v := versionInfo{buildinfo.Version, buildinfo.Commit, buildinfo.Date, runtime.Version(), runtime.GOOS, runtime.GOARCH, signing.Keys(), signing.TestKey != ""}
			return e.emit("version", v, func() string {
				s := fmt.Sprintf("backsight %s (%s, %s) %s %s/%s\n", v.Version, v.Commit, v.Date, v.Go, v.OS, v.Arch)
				for _, k := range v.SigningKeys {
					if k.Role == "test" {
						s += "test build: trusts the test signing key " + k.ID + "\n"
					}
				}
				return s
			})
		},
	}
}
