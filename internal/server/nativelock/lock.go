// Package nativelock carries the native runtime's pins (decision 0078):
// the hash-locked Python set Hindsight runs from, the Python and uv that
// install it, and pg0's versions. It never imports package server (ruling
// R8): the lock's agreement with the server's pinned image is tested there.
package nativelock

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Requirements is the lock scripts/native-lock.sh wrote from the pinned
// Hindsight image: its header, then every package with its hashes, for
// `uv pip install --require-hashes --no-deps`. A testhooks build may load
// another file in its place (TestLock).
//
//go:embed requirements-linux-amd64.txt
var Requirements []byte

// pg0 and the Postgres it runs (Task 1, decision 0078).
const (
	PG0      = "0.15.2"
	Postgres = "PostgreSQL 18.1, pgvector 0.8.5"
)

func init() {
	if p := testLock(); p != "" {
		raw, err := os.ReadFile(p)
		if err != nil {
			panic(fmt.Sprintf("nativelock.TestLock: %v", err))
		}
		Requirements = raw
	}
}

// header is the value of the lock's "# key: value" line.
func header(key string) string {
	sc := bufio.NewScanner(bytes.NewReader(Requirements))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "#") {
			break // the header is the leading comment block
		}
		if v, ok := strings.CutPrefix(line, "# "+key+": "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Image is the Hindsight image the lock was made from.
func Image() string { return header("image") }

// Python is the Python the lock installs into ("3.11.17").
func Python() string {
	v, _, _ := strings.Cut(header("python"), " ")
	return v
}

// Hindsight is the hindsight-api-slim version the lock installs.
func Hindsight() string { return Versions()["hindsight-api-slim"] }

// Hash12 is the first 12 hex digits of the lock's sha256: a native
// install's version is Hindsight() + "-" + Hash12().
func Hash12() string {
	sum := sha256.Sum256(Requirements)
	return hex.EncodeToString(sum[:])[:12]
}

// requirementRE is a requirement's name (extras dropped) and version.
var requirementRE = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*)(?:\[[^\]]*\])?==([^ ;\\]+)`)

// Versions maps each locked package's normalised name to its version.
func Versions() map[string]string {
	out := map[string]string{}
	for _, req := range requirementLines(Requirements) {
		if m := requirementRE.FindStringSubmatch(req); m != nil {
			out[Normalize(m[1])] = m[2]
		}
	}
	return out
}

var separators = regexp.MustCompile(`[-_.]+`)

// Normalize is a package name as PEP 503 compares them.
func Normalize(name string) string { return separators.ReplaceAllString(strings.ToLower(name), "-") }

// requirementLines are the lock's requirements, each with its
// continuation lines (hashes) joined, comments and blank lines dropped.
func requirementLines(raw []byte) []string {
	var out []string
	cur := ""
	for _, line := range strings.Split(string(raw), "\n") {
		t := strings.TrimSpace(line)
		if cur == "" && (t == "" || strings.HasPrefix(t, "#")) {
			continue
		}
		if more, ok := strings.CutSuffix(t, "\\"); ok {
			cur += strings.TrimSpace(more) + " "
			continue
		}
		out = append(out, cur+t)
		cur = ""
	}
	if cur != "" {
		out = append(out, strings.TrimSpace(cur))
	}
	return out
}
