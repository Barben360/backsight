package server

import (
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/server/nativelock"
)

// The native layout follows Native.GOOS, whatever the host: a Linux
// install keeps its paths, a Windows one has Scripts\*.exe, Lib\ and the
// copied Postgres under pg\, every path with \ on any host (Linux's take
// the host's separator: the systemd host's tests run on Windows too).
func TestLayoutByOS(t *testing.T) {
	pg, uv := nativelock.PGInstallation, nativelock.UVVersion
	for _, tc := range []struct {
		goos, compose                    string
		venvBin, sitePackages, pgBin, uv string
		envFile, dataDir                 string
	}{
		{"linux", "/srv/bs",
			"/srv/bs/native/venv/1.1.0-x/bin/hindsight-api",
			"/srv/bs/native/venv/1.1.0-x/lib/python3.11/site-packages",
			"/srv/bs/native/home/.pg0/installation/" + pg + "/bin/postgres",
			"/srv/bs/native/uv/" + uv + "/uv",
			"/srv/bs/native/server.env",
			"/srv/bs/native/home/.pg0/instances/hindsight/data"},
		{"windows", `C:\bs`,
			`C:\bs\native\venv\1.1.0-x\Scripts\hindsight-api.exe`,
			`C:\bs\native\venv\1.1.0-x\Lib\site-packages`,
			`C:\bs\native\pg\` + pg + `\bin\postgres.exe`,
			`C:\bs\native\uv\` + uv + `\uv.exe`,
			`C:\bs\native\server.env`,
			`C:\bs\native\home\.pg0\instances\hindsight\data`}, // Task 10 moves it to db\
	} {
		t.Run(tc.goos, func(t *testing.T) {
			n := Native{GOOS: tc.goos, Cfg: config.Config{Server: config.Server{ComposeDir: tc.compose}}}
			for _, c := range []struct{ what, got, want string }{
				{`venvBin("1.1.0-x", "hindsight-api")`, n.venvBin("1.1.0-x", "hindsight-api"), tc.venvBin},
				{`sitePackages("1.1.0-x")`, n.sitePackages("1.1.0-x"), tc.sitePackages},
				{`pgBin("postgres")`, n.pgBin("postgres"), tc.pgBin},
				{"UV()", n.UV(), tc.uv},
				{"EnvFile()", n.EnvFile(), tc.envFile},
				{"DataDir()", n.DataDir(), tc.dataDir},
			} {
				if tc.goos == "linux" {
					c.want = filepath.FromSlash(c.want)
				}
				if c.got != c.want {
					t.Errorf("%s = %q, want %q", c.what, c.got, c.want)
				}
			}
		})
	}
}

// A tool's environment on Linux is toolEnv's; on Windows it is the fixed
// list (0080), its PATH the tool's directory then System32, TEMP and TMP
// under the root, a proxy setting passed once whatever its case.
func TestToolEnvForByOS(t *testing.T) {
	for _, k := range passedThrough {
		t.Setenv(k, "")
	}
	t.Setenv("HTTPS_PROXY", "http://proxy:3128")
	for k, v := range map[string]string{"SYSTEMROOT": `C:\Windows`, "SystemDrive": "C:", "USERPROFILE": `C:\Users\u`,
		"LOCALAPPDATA": `C:\Users\u\AppData\Local`, "APPDATA": `C:\Users\u\AppData\Roaming`, "COMSPEC": `C:\Windows\system32\cmd.exe`} {
		t.Setenv(k, v)
	}
	linux := Native{GOOS: "linux", Cfg: config.Config{Server: config.Server{ComposeDir: "/srv/bs"}}}
	if got := linux.toolEnvFor("/srv/bs/native/uv/x"); !reflect.DeepEqual(got, toolEnv()) {
		t.Errorf("Linux: %q, want toolEnv()'s %q", got, toolEnv())
	}
	win := Native{GOOS: "windows", Cfg: config.Config{Server: config.Server{ComposeDir: `C:\bs`}}}
	want := []string{`SYSTEMROOT=C:\Windows`, "SystemDrive=C:", `TEMP=C:\bs\native\tmp`, `TMP=C:\bs\native\tmp`,
		`USERPROFILE=C:\Users\u`, `LOCALAPPDATA=C:\Users\u\AppData\Local`, `APPDATA=C:\Users\u\AppData\Roaming`,
		`COMSPEC=C:\Windows\system32\cmd.exe`, `PATH=C:\bs\native\uv\0.13.1;C:\Windows\System32`, "HTTPS_PROXY=http://proxy:3128",
		"UV_NO_CONFIG=1", "UV_PYTHON_PREFERENCE=only-managed"}
	if got := win.toolEnvFor(`C:\bs\native\uv\0.13.1`); !reflect.DeepEqual(got, want) {
		t.Errorf("Windows:\n got %q\nwant %q", got, want)
	}
	// No SYSTEMROOT: System32 is still absolute, never relative to the
	// tool's working directory.
	t.Setenv("SYSTEMROOT", "")
	if got := win.toolEnvFor(`C:\bs\native\uv\0.13.1`); !slices.Contains(got, `PATH=C:\bs\native\uv\0.13.1;C:\Windows\System32`) {
		t.Errorf("Windows without SYSTEMROOT: %q", got)
	}
}
