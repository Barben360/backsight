package project

import "path/filepath"

// Explicit reports whether cwd's project name comes from a .backsight.toml
// `project` key rather than from git or the directory. Session state
// records it: doctor's remotes check does not warn on a name every session
// took from such a key, a rename (decision 0045) or a deliberate share.
func (r *Resolver) Explicit(cwd string) bool {
	cwd = filepath.Clean(cwd)
	name, _ := r.Named(cwd)
	v := r.settings(cwd)
	return v.project != "" && v.project == name
}
