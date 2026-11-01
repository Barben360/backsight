package server

import (
	"testing"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/testtool"
)

// The native goldens were recorded from 884c942, before the Windows
// native runtime (M7 block 2): a Linux native install renders these files
// byte for byte, whatever the Windows host adds. The project is "bs", so
// the unit names follow it.
func TestNativeRenderIsUnchanged(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: the Linux units' goldens, recorded with / paths (Windows: TestTaskXMLGolden, TestTaskXMLOddPathGolden)")
	withFreePort(t, 8889)
	cfg := config.Default(paths.Dirs{Data: "/srv/bs/data"}, "/home/u")
	cfg.Server.Runtime, cfg.Server.Project = "native", "bs"
	n := Native{Cfg: cfg, Dirs: paths.Dirs{Data: "/srv/bs/data"}, UnitDir: "/srv/units",
		Env: map[string]string{"BACKSIGHT_HOME": "/srv/bs"}, UserHome: "/home/u"}.WithVersion("1.1.0-0123456789ab").(Native)
	f, err := n.Render(secrets, "box")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Units) != 2 {
		t.Errorf("units %d, want the db and the server", len(f.Units))
	}
	golden(t, "native-golden/server.env", f.Env)
	golden(t, "native-golden/bs-db.service", f.Units["bs-db.service"])
	golden(t, "native-golden/bs-server.service", f.Units["bs-server.service"])
}
