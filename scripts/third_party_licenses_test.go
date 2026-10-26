package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// thirdPartyLicenses runs scripts/third-party-licenses.sh in dir (its
// content, so Go's test cache keys on it) with args and extra env.
func thirdPartyLicenses(t *testing.T, dir string, env []string, args ...string) (string, error) {
	t.Helper()
	src, err := os.ReadFile("third-party-licenses.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{"-c", string(src), "third-party-licenses.sh"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), append([]string{"GOWORK=off", "GOFLAGS=-mod=mod"}, env...)...)
	b, err := cmd.CombinedOutput()
	return string(b), err
}

// licenseModule makes a main module in a temporary directory whose
// cmd/app imports one local dependency, example.com/dep, holding files.
// Its go.mod has the repository's toolchain line, so the script uses the
// release toolchain (and its standard library license).
func licenseModule(t *testing.T, files map[string]string) string {
	t.Helper()
	gomod, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	toolchain := regexp.MustCompile(`(?m)^toolchain go\S+$`).Find(gomod)
	if toolchain == nil {
		t.Fatal("no toolchain line in go.mod")
	}
	d := t.TempDir()
	all := map[string]string{
		"go.mod":          "module example.com/m\n\ngo 1.26.0\n\n" + string(toolchain) + "\n\nrequire example.com/dep v0.0.0\n\nreplace example.com/dep => ./dep\n",
		"cmd/app/main.go": "package main\n\nimport \"example.com/dep\"\n\nfunc main() { dep.F() }\n",
		"dep/go.mod":      "module example.com/dep\n\ngo 1.26.0\n",
		"dep/dep.go":      "package dep\n\nfunc F() {}\n",
	}
	for k, v := range files {
		all[k] = v
	}
	for name, content := range all {
		p := filepath.Join(d, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

var licenseEnv = []string{"PKG=./cmd/app", "TARGETS=linux/amd64 windows/amd64"}

// A dependency without a license file cannot ship: the script names it and
// writes nothing.
func TestThirdPartyLicensesRefuseAModuleWithoutItsText(t *testing.T) {
	d := licenseModule(t, map[string]string{"dep/README.md": "no license here\n"})
	out, err := thirdPartyLicenses(t, d, licenseEnv, "OUT")
	if err == nil || !strings.Contains(out, "example.com/dep") || !strings.Contains(out, "no license file") {
		t.Errorf("a module without a license must fail and be named: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(filepath.Join(d, "OUT")); statErr == nil {
		t.Error("OUT written although a module has no license")
	}
	// --check fails too, even when an old file is there.
	if err := os.WriteFile(filepath.Join(d, "OUT"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := thirdPartyLicenses(t, d, licenseEnv, "--check", "OUT"); err == nil || !strings.Contains(out, "example.com/dep") {
		t.Errorf("--check with a module without a license: %v\n%s", err, out)
	}
}

// Every license, notice and patent file at a module's root is copied
// whole (both texts of a dual-licensed module), the Go standard library's
// license is included, and source files are not.
func TestThirdPartyLicensesCopyEveryTextWhole(t *testing.T) {
	dual := "Part of this code is MIT, part Apache-2.0.\n\nMIT TEXT\n\n---\n\nAPACHE TEXT" // no final newline
	d := licenseModule(t, map[string]string{
		"dep/LICENSE":         dual,
		"dep/NOTICE":          "Copyright notice of dep\n",
		"dep/license_test.go": "package dep\n",
		"dep/sub/LICENSE":     "NOT AT THE ROOT\n",
	})
	if out, err := thirdPartyLicenses(t, d, licenseEnv, "OUT"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(d, "OUT"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{
		"- Go standard library",
		"- example.com/dep\n",
		"\nexample.com/dep\n",
		"--- LICENSE ---\n\n" + dual + "\n",
		"--- NOTICE ---\n\nCopyright notice of dep\n",
		"Copyright 2009 The Go Authors", // the standard library's LICENSE
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, bad := range []string{"license_test.go", "NOT AT THE ROOT", d} {
		if strings.Contains(got, bad) {
			t.Errorf("%q should not be in the file:\n%s", bad, got)
		}
	}

	// --check accepts the current file and refuses an edited or missing one.
	if out, err := thirdPartyLicenses(t, d, licenseEnv, "--check", "OUT"); err != nil {
		t.Errorf("--check on a fresh file: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(d, "OUT"), []byte(strings.Replace(got, "APACHE TEXT", "", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := thirdPartyLicenses(t, d, licenseEnv, "--check", "OUT"); err == nil || !strings.Contains(out, "make licenses") {
		t.Errorf("--check on a stale file: %v\n%s", err, out)
	}
	if out, err := thirdPartyLicenses(t, d, licenseEnv, "--check", "NONE"); err == nil {
		t.Errorf("--check on a missing file: %s", out)
	}
}

// A module only one release target links (as cobra's Windows helper) is
// listed: the file covers every archive.
func TestThirdPartyLicensesCoverEveryTarget(t *testing.T) {
	d := licenseModule(t, map[string]string{
		"dep/LICENSE":          "dep license\n",
		"cmd/app/main.go":      "package main\n\nfunc main() { f() }\n",
		"cmd/app/f_other.go":   "//go:build !windows\n\npackage main\n\nfunc f() {}\n",
		"cmd/app/f_windows.go": "//go:build windows\n\npackage main\n\nimport \"example.com/dep\"\n\nfunc f() { dep.F() }\n",
	})
	if out, err := thirdPartyLicenses(t, d, []string{"PKG=./cmd/app", "TARGETS=linux/amd64"}, "OUT"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(d, "OUT")); strings.Contains(string(b), "example.com/dep") {
		t.Errorf("a Linux-only file lists the Windows-only module:\n%s", b)
	}
	if out, err := thirdPartyLicenses(t, d, licenseEnv, "OUT"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(d, "OUT")); !strings.Contains(string(b), "dep license") {
		t.Errorf("the Windows-only module is missing:\n%s", b)
	}
}

// The committed file is what go.mod gives today: a new dependency without
// its text fails the tests (and CI's check) before it ships.
func TestThirdPartyLicensesAreCurrent(t *testing.T) {
	if out, err := thirdPartyLicenses(t, "..", nil, "--check"); err != nil {
		t.Errorf("%v\n%s", err, out)
	}
}
