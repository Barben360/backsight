package doctor

import (
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/state"
)

func withDate(t *testing.T, date string) {
	old := buildinfo.Date
	buildinfo.Date = date
	t.Cleanup(func() { buildinfo.Date = old })
}

func TestBinaryCheck(t *testing.T) {
	f := newFixture(t)
	rec := state.SelfCheck{Exe: "/d/bin/0.5.0/backsight", Version: "0.5.0"}
	f.env.SelfCheck = func() state.SelfCheck { return rec }

	rec.Status, rec.KeyID = "verified", "15AF315BAB4E6A02"
	expect(t, check(t, f.run(), "binary"), OK, "15AF315BAB4E6A02")

	withDate(t, "unknown")
	rec.Status, rec.Detail = "unsigned", "no checksums.txt next to /d/bin/0.5.0/backsight"
	c := check(t, f.run(), "binary")
	expect(t, c, Info, "unsigned dev build")
	if c.Fix != "" {
		t.Errorf("a dev build needs no fix: %q", c.Fix)
	}

	// A release binary installed without its checksums cannot check
	// itself: info, with the reinstall that brings them.
	withDate(t, "2026-10-25T10:00:00Z")
	c = check(t, f.run(), "binary")
	expect(t, c, Info, "not self-verified")
	if !strings.Contains(c.Fix, "install.sh") {
		t.Errorf("fix %q", c.Fix)
	}

	rec.Status, rec.Detail = "failed", "checksum mismatch: backsight_0.5.0_linux_amd64 has sha256 aa, checksums.txt says bb"
	r := f.run()
	c = check(t, r, "binary")
	expect(t, c, Error, "does not match its signed release")
	if c.Detail != rec.Detail || !strings.Contains(c.Fix, "install.sh") || r.Errors == 0 {
		t.Errorf("%+v (errors %d)", c, r.Errors)
	}
}

// Without the probe (tests, or a doctor that cannot know its binary) there
// is no binary check.
func TestBinaryCheckNeedsTheProbe(t *testing.T) {
	f := newFixture(t)
	for _, c := range f.run().Checks {
		if c.ID == "binary" {
			t.Errorf("unexpected %+v", c)
		}
	}
}

// The shim's earlier copies a swap left (Windows): none is ok, some is
// information with each named, never a fault; no list asked, no check.
func TestShimCopiesCheck(t *testing.T) {
	f := newFixture(t)
	for _, c := range f.run().Checks {
		if c.ID == "shim-copies" {
			t.Errorf("a check without ShimCopies: %+v", c)
		}
	}
	var copies []string
	f.env.ShimCopies = func() []string { return copies }
	expect(t, check(t, f.run(), "shim-copies"), OK, "no earlier copy")
	copies = []string{`C:\d\bin\current.exe.old-1`, `C:\d\bin\current.exe.old-3`}
	c := check(t, f.run(), "shim-copies")
	expect(t, c, Info, "2 earlier copies of the shim still run")
	if !strings.Contains(c.Detail, copies[0]) || !strings.Contains(c.Detail, copies[1]) {
		t.Errorf("detail %q", c.Detail)
	}
	copies = copies[:1]
	expect(t, check(t, f.run(), "shim-copies"), Info, "1 earlier copy of the shim still runs")
}
