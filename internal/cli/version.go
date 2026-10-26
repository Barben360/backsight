package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/licenses"
	"github.com/Barben360/backsight/internal/server"
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
	// TestHooks: built with the testhooks tag (CI's update proof).
	TestHooks bool `json:"test_hooks"`
	// HindsightImage: the server version this binary deploys; a release's
	// is the source's pin (make verify-keys).
	HindsightImage string `json:"hindsight_image"`
}

func newVersionCmd(e *env) *cobra.Command {
	var showLicenses bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the backsight version",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if showLicenses {
				// Embedded: a bare binary ships without the archive's copy.
				data := struct {
					ThirdPartyLicenses string `json:"third_party_licenses"`
				}{licenses.Text}
				return e.emit("version", data, func() string { return licenses.Text })
			}
			v := versionInfo{buildinfo.Version, buildinfo.Commit, buildinfo.Date, runtime.Version(), runtime.GOOS, runtime.GOARCH, signing.Keys(), signing.TestKey != "", buildinfo.TestHooks, server.HindsightImage}
			return e.emit("version", v, func() string {
				s := fmt.Sprintf("backsight %s (%s, %s) %s %s/%s\nHindsight %s\nThird-party licenses: backsight version --licenses\n", v.Version, v.Commit, v.Date, v.Go, v.OS, v.Arch, v.HindsightImage)
				for _, k := range v.SigningKeys {
					if k.Role == "test" {
						s += "test build: trusts the test signing key " + k.ID + "\n"
					}
				}
				if v.TestHooks {
					s += "test build: has the CI test hooks\n"
				}
				return s
			})
		},
	}
	cmd.Flags().BoolVar(&showLicenses, "licenses", false, "print the license texts and notices of the third-party code in this binary")
	return cmd
}
