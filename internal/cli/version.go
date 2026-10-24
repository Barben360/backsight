package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
)

type versionInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

func newVersionCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the backsight version",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			v := versionInfo{buildinfo.Version, buildinfo.Commit, buildinfo.Date, runtime.Version(), runtime.GOOS, runtime.GOARCH}
			return e.emit("version", v, func() string {
				return fmt.Sprintf("backsight %s (%s, %s) %s %s/%s\n", v.Version, v.Commit, v.Date, v.Go, v.OS, v.Arch)
			})
		},
	}
}
