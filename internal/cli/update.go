package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/release"
	"github.com/Barben360/backsight/internal/state"
)

// githubAPI is where the GitHub release source reads; tests point it at a
// fake (TestMain at a closed port, so no test reaches GitHub).
var githubAPI = release.GitHubAPI

const (
	releaseOwner, releaseRepo = "Barben360", "backsight"
	// updateCheckTimeout bounds the releases list of jobs maintain.
	updateCheckTimeout = 20 * time.Second
)

// versionEntry is one release in update check and changelog output.
type versionEntry struct {
	Version     string    `json:"version"`
	Tag         string    `json:"tag"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Summary     string    `json:"summary"`
	Notes       string    `json:"notes"`
}

type updateCheckResult struct {
	Installed       string         `json:"installed"`
	Source          string         `json:"source"`
	Prereleases     bool           `json:"prereleases"` // pre-releases were considered
	UpdateAvailable bool           `json:"update_available"`
	Latest          string         `json:"latest,omitempty"`
	Versions        []versionEntry `json:"versions"` // newer than installed, newest first
}

type changelogResult struct {
	Source      string         `json:"source"`
	Since       string         `json:"since,omitempty"`
	Prereleases bool           `json:"prereleases"`
	Versions    []versionEntry `json:"versions"`
}

func newUpdateCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{Use: "update", Short: "Check for backsight releases (SPEC §11.7)", Args: cobra.NoArgs}
	var source string
	check := &cobra.Command{
		Use:   "check",
		Short: "List the releases newer than this binary, with their changelog",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, cfg, err := updateConfig()
			if err != nil {
				return err
			}
			res, err := checkUpdates(cmd.Context(), releaseSource(cfg, source), buildinfo.Version, cfg.Update.Prereleases)
			if source == "" { // a one-off --source does not feed the banner
				recordUpdateCheck(d, res, err)
			}
			return e.report("update check", res, func() string { return updateCheckText(res, err) }, err)
		},
	}
	check.Flags().StringVar(&source, "source", "", "read releases from this local release directory instead of the configured source")
	cmd.AddCommand(check)
	return cmd
}

func newChangelogCmd(e *env) *cobra.Command {
	var source, since string
	cmd := &cobra.Command{
		Use:   "changelog",
		Short: "Print release notes, newest first",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if since != "" {
				if _, err := release.Parse(since); err != nil {
					return usageError{fmt.Errorf("--since: %w", err)}
				}
			}
			_, cfg, err := updateConfig()
			if err != nil {
				return err
			}
			src := releaseSource(cfg, source)
			pre := release.WantPrereleases(buildinfo.Version, cfg.Update.Prereleases)
			res := changelogResult{Source: src.Name(), Since: since, Prereleases: pre, Versions: []versionEntry{}}
			rels, err := src.Releases(cmd.Context())
			if err != nil {
				return e.report("changelog", res, func() string { return "" }, sourceError(err))
			}
			res.Versions = entries(release.Newer(rels, since, pre))
			return e.emit("changelog", res, func() string {
				if len(res.Versions) == 0 {
					return "no release notes" + map[bool]string{true: " after " + since, false: ""}[since != ""] + " on " + res.Source + "\n"
				}
				return notesText(res.Versions)
			})
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "only releases newer than this version")
	cmd.Flags().StringVar(&source, "source", "", "read releases from this local release directory instead of the configured source")
	return cmd
}

// updateConfig is the config update commands read: the defaults when
// backsight is not set up (a check works before setup).
func updateConfig() (paths.Dirs, config.Config, error) {
	d, home, err := machine()
	if err != nil {
		return d, config.Config{}, err
	}
	cfg, err := config.Load(d.ConfigFile(), d, home)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return d, cfg, &output.Error{Code: output.Blocked, Message: err.Error()}
	}
	return d, cfg, nil
}

// releaseSource is the --source directory, else update.source, else the
// GitHub releases (with GH_TOKEN or GITHUB_TOKEN while the repository is
// private, decision 0063 Q2).
func releaseSource(cfg config.Config, flag string) release.Source {
	dir := cfg.Update.Source
	if flag != "" {
		dir = flag
		if abs, err := filepath.Abs(flag); err == nil {
			dir = abs
		}
	}
	if dir != "" {
		return release.Dir{Path: dir}
	}
	g := release.NewGitHub(releaseOwner, releaseRepo, release.TokenFromEnv(os.Getenv))
	g.BaseURL = githubAPI
	return g
}

func sourceError(err error) error {
	// The source's errors name it (the repository, or the directory).
	return &output.Error{Code: output.Blocked, Message: "release source unreadable: " + err.Error()}
}

// checkUpdates lists the releases newer than installed under the
// pre-release policy; an unreadable source is a Blocked error.
func checkUpdates(ctx context.Context, src release.Source, installed string, optIn bool) (updateCheckResult, error) {
	res := updateCheckResult{Installed: installed, Source: src.Name(), Prereleases: release.WantPrereleases(installed, optIn), Versions: []versionEntry{}}
	rels, err := src.Releases(ctx)
	if err != nil {
		return res, sourceError(err)
	}
	res.Versions = entries(release.Newer(rels, installed, res.Prereleases))
	if len(res.Versions) > 0 {
		res.UpdateAvailable, res.Latest = true, res.Versions[0].Version
	}
	return res, nil
}

func entries(rels []release.Release) []versionEntry {
	out := make([]versionEntry, 0, len(rels))
	for _, r := range rels {
		out = append(out, versionEntry{Version: r.Version, Tag: r.Tag, Prerelease: r.Prerelease, PublishedAt: r.Published,
			Summary: release.Summary(r.Notes), Notes: r.Notes})
	}
	return out
}

// recordUpdateCheck saves the outcome in state.json for the banner and
// doctor. A failure keeps the newest release the last good check of the
// same source found. Recording is best effort: a busy state lock loses
// one record, never the command.
func recordUpdateCheck(d paths.Dirs, res updateCheckResult, checkErr error) {
	_ = state.Update(d, "update check", func(s *state.State) error {
		uc := &state.UpdateCheck{CheckedAt: now().UTC(), Source: res.Source, Installed: res.Installed}
		if checkErr != nil {
			uc.Error = checkErr.Error()
			if prev := s.UpdateCheck; prev != nil && prev.Source == res.Source {
				uc.Latest, uc.Summary, uc.Newer = prev.Latest, prev.Summary, prev.Newer
			}
		} else if res.UpdateAvailable {
			uc.Latest, uc.Summary, uc.Newer = res.Latest, res.Versions[0].Summary, len(res.Versions)
		}
		s.UpdateCheck = uc
		return nil
	})
}

// updateBanner is the SessionStart line for a recorded release newer than
// the running binary (SPEC §11.7), or "".
func updateBanner(uc *state.UpdateCheck, running string) string {
	if uc == nil || !release.NewerThan(uc.Latest, running) {
		return ""
	}
	line := "backsight: update " + uc.Latest + " available"
	if uc.Summary != "" {
		line += " (" + uc.Summary + ")"
	}
	return line
}

func updateCheckText(res updateCheckResult, err error) string {
	switch {
	case err != nil:
		return ""
	case !res.UpdateAvailable:
		return fmt.Sprintf("backsight %s is up to date (%s)\n", res.Installed, res.Source)
	}
	return fmt.Sprintf("backsight %s: %d newer release(s) on %s\n\n", res.Installed, len(res.Versions), res.Source) + notesText(res.Versions)
}

// notesText prints each release's header and its notes, indented.
func notesText(vs []versionEntry) string {
	var b strings.Builder
	for i, v := range vs {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(v.Version)
		if !v.PublishedAt.IsZero() {
			b.WriteString(" · " + v.PublishedAt.Format("2006-01-02"))
		}
		if v.Prerelease {
			b.WriteString(" · pre-release")
		}
		b.WriteString("\n")
		for _, line := range strings.Split(strings.TrimSpace(v.Notes), "\n") {
			if line = strings.TrimRight(line, " \r"); line != "" && line != "## Changelog" {
				b.WriteString("  " + line + "\n")
			}
		}
	}
	return b.String()
}
