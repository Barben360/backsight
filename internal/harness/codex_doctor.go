package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/codexcfg"
)

// What doctor reads of Codex: block 8 (https://github.com/Barben360/backsight/issues/283),
// 0085 D3 (the name clash), D5 (hook trust, the heartbeat, the shim) and
// D10 (Codex's own memories). Read only, like every doctor probe.

// StartReader is a harness that tells how a conversation started (its
// entrypoint and first directory) from its head alone, without reading it
// whole (doctor's headless check reads one per sent session).
type StartReader interface {
	ConversationStart(path string) (entrypoint, cwd string)
}

var _ StartReader = Codex{}

// ConversationStart is a rollout's originator (codex-tui, codex_exec…)
// and the directory its thread started in, from its session_meta; empty
// when it has none.
func (Codex) ConversationStart(path string) (entrypoint, cwd string) {
	m, ok := ReadRolloutMeta(path)
	if !ok {
		return "", ""
	}
	return m.Originator, m.Cwd
}

// CodexPluginState is one of backsight's plugins as `codex plugin list
// --json` shows it; the zero value is a plugin it does not list.
type CodexPluginState struct {
	Installed, Enabled bool
	Version            string
}

// PluginStates are backsight's two plugins in env's Codex home, by id
// (CodexPluginIDs); a plugin Codex does not list is there with its zero
// value.
func (c Codex) PluginStates(ctx context.Context, env Env) (map[string]CodexPluginState, error) {
	raw, err := c.run(ctx, env, "plugin", "list", "--json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Installed []codexPlugin `json:"installed"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("codex plugin list --json: unexpected output: %w", err)
	}
	out := map[string]CodexPluginState{}
	for _, id := range CodexPluginIDs() {
		out[id] = CodexPluginState{}
		for _, p := range list.Installed {
			if p.ID == id {
				out[id] = CodexPluginState{Installed: p.Installed, Enabled: p.Enabled, Version: p.Version}
			}
		}
	}
	return out, nil
}

// UntrustedHooks are the trust keys of backsight's hooks (CodexTrustKeys)
// that env's config.toml records no trusted_hash for; an error when
// config.toml cannot be read or is not valid TOML. A config.toml that is
// not there trusts nothing.
func (c Codex) UntrustedHooks(env Env) ([]string, error) {
	raw, err := os.ReadFile(c.Locate(env).Settings)
	if errors.Is(err, fs.ErrNotExist) {
		raw, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, key := range CodexTrustKeys() {
		_, ok, err := codexcfg.Lookup(raw, []string{"hooks", "state", key, "trusted_hash"})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.Locate(env).Settings, err)
		}
		if !ok {
			out = append(out, key)
		}
	}
	return out, nil
}

// latestScan bounds how many rollouts LatestRoot opens, newest first:
// subagent threads are skipped, and a home full of them must not make
// doctor slow.
const latestScan = 64

// LatestRoot is the newest root thread of env's Codex that started at or
// before until (a subagent's thread is not one: it runs no SessionStart),
// and its rollout's path; ok false when there is none among the newest
// rollouts.
func (c Codex) LatestRoot(env Env, until time.Time) (RolloutMeta, string, bool) {
	dir := c.codexSessionsDir(env)
	if dir == "" {
		return RolloutMeta{}, "", false
	}
	var files []string
	for _, pat := range []string{"rollout-*.jsonl", "rollout-*.jsonl.zst"} {
		m, _ := filepath.Glob(filepath.Join(dir, "*", "*", "*", pat))
		files = append(files, m...)
	}
	// The date folders, then the name's own time: newest last.
	sort.Strings(files)
	slices.Reverse(files)
	opened := 0
	for _, f := range files {
		if opened == latestScan {
			break
		}
		if c.SessionOf(f) == "" || isRolloutDuplicate(f) {
			continue
		}
		if fi, err := os.Lstat(f); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		opened++
		m, ok := ReadRolloutMeta(f)
		if !ok || m.Subagent() || m.Started.IsZero() || m.Started.After(until) {
			continue
		}
		return m, f, true
	}
	return RolloutMeta{}, "", false
}

// Features are the effective states `codex features list` prints, by
// feature (its rows: name, stage, true or false).
func (c Codex) Features(ctx context.Context, env Env) (map[string]bool, error) {
	out, err := c.run(ctx, env, "features", "list")
	if err != nil {
		return nil, err
	}
	feats := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch f[len(f)-1] {
		case "true":
			feats[f[0]] = true
		case "false":
			feats[f[0]] = false
		}
	}
	if len(feats) == 0 {
		return nil, fmt.Errorf("codex features list: unexpected output %q", strings.TrimSpace(string(out)))
	}
	return feats, nil
}
