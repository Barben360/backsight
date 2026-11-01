package scripts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The steps of the native chain on Windows (Task 14 of M7 block 2), in
// their order: each a function of its own, each failing the run with its
// name.
var ncwSteps = []string{"build", "install", "setup", "dlls", "store", "backup", "restore", "upgrade-rollback",
	"kill-postgres", "kill-supervisor", "refuse-identity", "refuse-missing", "refuse-stray", "refuse-fsync", "uninstall"}

func ncwScript(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("native-chain-windows.ps1")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// ncwFunction is the body of the script's function name, up to the next
// top-level function.
func ncwFunction(t *testing.T, src, name string) string {
	t.Helper()
	start := regexp.MustCompile(`(?m)^function ` + regexp.QuoteMeta(name) + `\b[^\n]*\{\s*$`).FindStringIndex(src)
	if start == nil {
		t.Fatalf("native-chain-windows.ps1 has no function %s", name)
	}
	rest := src[start[1]:]
	if end := regexp.MustCompile(`(?m)^(function |\$|#)`).FindStringIndex(rest); end != nil {
		rest = rest[:end[0]]
	}
	return rest
}

// The CI proof (decision 0080): a job of its own on windows-latest with
// no if: (nor continue-on-error) and the brief's 60 minutes, one step with
// no if: running the script, its project named after the slot, and native's
// model cache keyed on the Windows lock, restored before the chain and
// saved after it whatever happened.
func TestNativeChainWindowsRunsOnEveryPullRequest(t *testing.T) {
	var j *job
	for _, w := range workflows(t) {
		if w.name != "ci.yml" {
			continue
		}
		for i := range w.jobs {
			if w.jobs[i].name == "native-chain-windows" {
				j = &w.jobs[i]
			}
		}
	}
	if j == nil {
		t.Fatal("ci.yml has no native-chain-windows job")
	}
	header, _, _ := strings.Cut(j.raw, "\n    steps:")
	for _, want := range []string{`(?m)^    runs-on: windows-latest\s*$`, `(?m)^    timeout-minutes: 60\s*$`} {
		if !regexp.MustCompile(want).MatchString(header) {
			t.Errorf("the native-chain-windows job lacks %s:\n%s", want, header)
		}
	}
	for _, key := range []string{"if", "continue-on-error"} {
		if regexp.MustCompile(`(?m)^    ` + key + `:`).MatchString(header) {
			t.Errorf("the native-chain-windows job has an %s: (it may not run, or not fail):\n%s", key, header)
		}
	}
	isScript := regexp.MustCompile(`(^|[\s/\\])scripts[/\\]native-chain-windows\.ps1(\s|$)`)
	lockKey := regexp.MustCompile(`(?m)^          key: hf-native-\$\{\{ runner\.os \}\}-\$\{\{ runner\.arch \}\}-\$\{\{ hashFiles\('internal/server/nativelock/requirements-windows-amd64\.txt'\) \}\}\s*$`)
	script, restore, save := -1, -1, -1
	for i, step := range j.steps {
		switch {
		case strings.Contains(step, "uses: actions/cache/restore@") && lockKey.MatchString(step) && strings.Contains(step, "path: ${{ runner.temp }}/hf-native"):
			restore = i
		case strings.Contains(step, "uses: actions/cache/save@") && strings.Contains(step, "path: ${{ runner.temp }}/hf-native"):
			save = i
			if !regexp.MustCompile(`(?m)^        if: always\(\) && `).MatchString(step) {
				t.Errorf("the model cache is not saved after a failed chain (if: always() && …):\n%s", step)
			}
		}
		lines := runScript(step)
		if !slices.ContainsFunc(lines, isScript.MatchString) {
			continue
		}
		if script >= 0 {
			t.Errorf("steps %d and %d both run scripts/native-chain-windows.ps1", script, i)
		}
		script = i
		if regexp.MustCompile(`(?m)^        if:`).MatchString(step) {
			t.Errorf("the step running scripts/native-chain-windows.ps1 has an if::\n%s", step)
		}
		if !slices.ContainsFunc(lines, func(l string) bool {
			return strings.Contains(l, `$env:PROJECT = "bs-native-chain-$env:BACKSIGHT_CI_SLOT"`)
		}) {
			t.Errorf("the chain's project is not bs-native-chain-<slot>:\n%s", step)
		}
		if !strings.Contains(step, `BACKSIGHT_NATIVE_HF: ${{ runner.temp }}\hf-native`) {
			t.Errorf("the chain is not given the model cache (BACKSIGHT_NATIVE_HF):\n%s", step)
		}
	}
	if script < 0 {
		t.Fatal("no step of the native-chain-windows job runs scripts/native-chain-windows.ps1")
	}
	if restore < 0 || restore > script || save < script {
		t.Errorf("the model cache keyed on the Windows lock is not restored before the chain and saved after it (restore %d, chain %d, save %d)", restore, script, save)
	}
}

// Every step the brief names is in the script, in its order, as a
// function of its own that does its step's check (a hollowed step loses
// the text it is checked by); the runner names the step that failed.
func TestNativeChainWindowsHasEveryStep(t *testing.T) {
	src := ncwScript(t)
	m := regexp.MustCompile(`(?s)\$NCSteps = @\((.*?)\)`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("native-chain-windows.ps1 has no $NCSteps list")
	}
	got := regexp.MustCompile(`'([a-z-]+)'`).FindAllStringSubmatch(m[1], -1)
	var steps []string
	for _, g := range got {
		steps = append(steps, g[1])
	}
	if !slices.Equal(steps, ncwSteps) {
		t.Errorf("$NCSteps = %q, want %q", steps, ncwSteps)
	}
	does := map[string][]string{
		"build":            {"'testkey,testhooks'", "nativelock.TestLock", "./scripts/testrelease", "'release'"},
		"install":          {`scripts\install.ps1`, "BACKSIGHT_RELEASE_DIR", "BACKSIGHT_INSTALL_PUBKEY", "test build: trusts the test signing key", "$script:realPath"},
		"setup":            {"'--runtime', 'native'", "'provider=mock'", "Test-NcTask", "Test-NcHealthy", "Get-NcStartPath", "'doctor'"},
		"dlls":             {".Modules", "$NCRuntimeDll", "outside the native root", "$script:pgBin"},
		"store":            {"@('store',", "@('search',"},
		"backup":           {"@('backup', 'create',", "@('backup', 'verify',"},
		"restore":          {"@('store',", "Test-NcDocumentKey 'keys-after-backup'", "@('restore',", "Save-NcKeys 'keys-restore'", "Test-NcDocumentKey 'keys-restore'", "Test-NcSameKeys 'keys-backup' 'keys-restore'", "Wait-NcHealthy"},
		"upgrade-rollback": {"$script:bin2", "@('server', 'upgrade',", "BACKSIGHT_TEST_FAIL_SMOKE", "-ne 30", "Get-NcProcs $script:native 'hindsight-api'", "Test-NcUnder $_.Path $venvDir", "Test-NcSameKeys 'keys-before-upgrade' 'keys-after-upgrade'", "Wait-NcHealthy"},
		"kill-postgres":    {"Get-NcPostmaster", "Stop-Process", "-lt 30", "Test-NcPgReady", "Wait-NcHealthy"},
		"kill-supervisor":  {"Get-NcSupervisors", "Stop-Process", "Wait-NcGone", "Start-NcServer", `a postgres \(pid \d+\) runs on`},
		"refuse-identity":  {"'identity'", "@('server', 'start',", "Start-Sleep -Seconds $NCSettle", "@('server', 'status',", "is not the one backsight recorded", "Get-NcSupervisors"},
		"refuse-missing":   {"Move-NcDir $db $aside", "@('server', 'start',", "Start-Sleep -Seconds $NCSettle", "@('server', 'status',", "is missing: refusing to start", "Get-NcSupervisors", "Move-NcDir $aside $db"},
		"refuse-stray":     {"'pg_ctl.exe'", "Start-NcServer", "-not $started", "stopping it", "Wait-NcGone"},
		"refuse-fsync":     {`'"fsync=off"'`, "'/Run'", "not fsync=on", "Start-Sleep -Seconds $NCSettle", "@('server', 'status',", "Compare-NcFingerprint"},
		"uninstall":        {"'--delete-data'", "Test-NcTask", "Get-NcProcs $script:bh", "Test-Path -LiteralPath $script:native", "$script:userPathKey", `'data\backups'`},
	}
	// Each checked outcome fails the step: its message is a Fail's. (A
	// regular expression cannot tell which condition guards a Fail; the
	// PowerShell tests below run the helpers those conditions call.)
	fails := map[string][]string{
		"build":            {"go build of", "testrelease"},
		"install":          {"install.ps1", "real user PATH"},
		"setup":            {"setup", "not healthy", "task", "doctor"},
		"dlls":             {"outside the native root"},
		"store":            {"was not stored", "never found"},
		"backup":           {"backup create", "backup verify", "restored no rows", "changed while it was backed up"},
		"restore":          {"stored after the backup", "restore failed", "did not pass its checks", "still there after the restore"},
		"upgrade-rollback": {"want 30", "rolled back", "never installed", "after the rollback", "hindsight-api.exe runs from"},
		"kill-postgres":    {"no running postgres", "within 30 s"},
		"kill-supervisor":  {"supervisors run", "left pid", "without a supervisor", "reported a stray"},
		"refuse-identity":  {"accepted a database", "is not the recorded one", "started something", "server status does not give"},
		"refuse-missing":   {"accepted a missing database", "database is missing", "started something", "in place of the missing one", "server status does not give"},
		"refuse-stray":     {"pg_ctl start", "did not stop it", "was not reported", "under one supervisor"},
		"refuse-fsync":     {"never gave the fsync refusal", "name supervise.json", "runs with fsync=off", "changed across the refusals"},
		"uninstall":        {"failed", "still finds the task", "still run from", "still there", "user PATH", "left"},
	}
	failRE := regexp.MustCompile(`Fail ("[^"]*"|'[^']*')`)
	for _, s := range ncwSteps {
		body := ncwFunction(t, src, "Step-"+s)
		for _, want := range does[s] {
			if !strings.Contains(body, want) {
				t.Errorf("step %s does not do its check: its function lacks %q", s, want)
			}
		}
		var msgs []string
		for _, m := range failRE.FindAllStringSubmatch(body, -1) {
			msgs = append(msgs, m[1])
		}
		if len(fails[s]) == 0 {
			t.Errorf("step %s has no failure message to check", s)
		}
		for _, want := range fails[s] {
			if !slices.ContainsFunc(msgs, func(m string) bool { return strings.Contains(m, want) }) {
				t.Errorf("step %s: no Fail says %q (its Fails: %q)", s, want, msgs)
			}
		}
	}
	runner := ncwFunction(t, src, "Invoke-NcSteps")
	for _, want := range []string{`& "Step-$s"`, `"native-chain-windows: step $s failed: `, `"native-chain-windows: step $failed failed"`, "'SKIP'", "return $false"} {
		if !strings.Contains(runner, want) {
			t.Errorf("the runner lacks %q: a failing step would not fail the run with its name", want)
		}
	}
}

// The chain's API and UI ports, for every slot, are none of the bash
// scratch stacks' (scripts/ports.sh's table) and none of the live
// installs'.
func TestNativeChainWindowsPortsAreApart(t *testing.T) {
	m := regexp.MustCompile(`Get-NcSlotPort (\d+) `).FindAllStringSubmatch(ncwScript(t), -1)
	if len(m) != 1 {
		t.Fatalf("want one Get-NcSlotPort call for the default port, found %d", len(m))
	}
	base, _ := strconv.Atoi(m[0][1])
	owner := map[int]string{}
	for _, script := range []string{"sandbox.sh", "fresh-install.sh", "restore-proof.sh", "agent-install.sh", "reattribute-proof.sh", "purge-guard-proof.sh", "m6-chain.sh", "native-chain.sh"} {
		src, err := os.ReadFile(script)
		if err != nil {
			t.Fatal(err)
		}
		b := regexp.MustCompile(`slot_port (\d+) "\$\{1:-\}"`).FindSubmatch(src)
		if b == nil {
			t.Fatalf("%s: no slot_port call", script)
		}
		n, _ := strconv.Atoi(string(b[1]))
		for slot := 0; slot <= 3; slot++ {
			owner[n+slot*10], owner[n+slot*10+1111] = script, script
		}
	}
	ports, err := os.ReadFile("ports.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ports), strconv.Itoa(base)+"  native-chain-windows.ps1") {
		t.Errorf("scripts/ports.sh's table does not list native-chain-windows.ps1's base %d", base)
	}
	for slot := 0; slot <= 3; slot++ {
		for _, p := range []int{base + slot*10, base + slot*10 + 1111} {
			if other, ok := owner[p]; ok {
				t.Errorf("slot %d: port %d is %s's too", slot, p, other)
			}
			if p < 1024 || p >= 32768 || slices.Contains([]int{8888, 9999, 18888, 19999}, p) {
				t.Errorf("slot %d: port %d is outside 1024-32767 or a live install's", slot, p)
			}
		}
	}
}

// ncwPS runs body in shell after dot-sourcing the script with NC_LIB=1:
// its output, and its error.
func ncwPS(t *testing.T, shell, body string, env ...string) (string, error) {
	t.Helper()
	ncwScript(t) // the test cache keys on the script's content
	driver := filepath.Join(t.TempDir(), "driver.ps1")
	src := "$ErrorActionPreference = 'Stop'\n$env:NC_LIB = '1'\n. " + psQuote(mustAbs(t, "native-chain-windows.ps1")) + "\n" + body + "\n"
	if err := os.WriteFile(driver, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(shell, psArgs(driver)...)
	cmd.Env = append(ncwEnv(), env...)
	out, err := cmd.CombinedOutput()
	return strings.ReplaceAll(string(out), "\r\n", "\n"), err
}

// ncwEnv is this process's environment without the chain's settings.
func ncwEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "PROJECT", "NC_DRY", "NC_LIB", "WORK", "KEEP", "CI", "BACKSIGHT_CI_SLOT", "BACKSIGHT_HOME":
			continue
		}
		out = append(out, kv)
	}
	return out
}

// ncwRun runs the script itself with env.
func ncwRun(t *testing.T, shell string, env ...string) (string, int) {
	t.Helper()
	ncwScript(t)
	cmd := exec.Command(shell, psArgs(mustAbs(t, "native-chain-windows.ps1"))...)
	cmd.Env = append(ncwEnv(), env...)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatalf("%s: %v\n%s", shellName(shell), err, out)
	}
	return strings.ReplaceAll(string(out), "\r\n", "\n"), cmd.ProcessState.ExitCode()
}

func TestNativeChainWindowsListsItsSteps(t *testing.T) {
	for _, shell := range powerShells(t) {
		t.Run(shellName(shell), func(t *testing.T) {
			out, code := ncwRun(t, shell, "NC_DRY=1", "WORK="+t.TempDir())
			if code != 0 || !slices.Equal(strings.Fields(out), ncwSteps) {
				t.Errorf("NC_DRY=1: exit %d, %q, want %q", code, strings.Fields(out), ncwSteps)
			}
		})
	}
}

// The live install's tasks are \backsight\...: the chain refuses that
// project, and any its cleanup could confuse with it, before it does
// anything (its WORK stays empty).
func TestNativeChainWindowsNeverTouchesTheLiveInstall(t *testing.T) {
	for _, shell := range powerShells(t) {
		t.Run(shellName(shell), func(t *testing.T) {
			for _, c := range []struct{ project, want string }{
				{"backsight", "never the live install"}, {"backsight-dev", "never the live install"}, {"backsightx", "never the live install"},
				{"Bs-Upper", "lowercase letters, digits and dashes only"}, {"-bs", "lowercase letters, digits and dashes only"}, {`bs\x`, "lowercase letters, digits and dashes only"},
			} {
				work := t.TempDir()
				out, code := ncwRun(t, shell, "PROJECT="+c.project, "WORK="+work)
				if code != 2 || !strings.Contains(out, c.want) {
					t.Errorf("PROJECT=%s: exit %d, want 2 saying %q:\n%s", c.project, code, c.want, out)
				}
				if ents, _ := os.ReadDir(work); len(ents) > 0 {
					t.Errorf("PROJECT=%s refused after writing to WORK: %v", c.project, ents)
				}
			}
			// Positive control: a bs- project gets past the refusal (NC_DRY
			// stops it before anything runs).
			if out, code := ncwRun(t, shell, "PROJECT=bs-native-chain-1", "NC_DRY=1", "WORK="+t.TempDir()); code != 0 || strings.Contains(out, "never the live install") {
				t.Errorf("PROJECT=bs-native-chain-1: exit %d\n%s", code, out)
			}
		})
	}
}

// A failing step fails the run with its name, and the steps after it do
// not run; every step passing passes it.
func TestNativeChainWindowsFailsWithTheStepsName(t *testing.T) {
	body := `
$script:ran = @()
function Step-a { $script:ran += 'a'; return 'a done' }
function Step-b { $script:ran += 'b'; Fail 'boom in b' }
function Step-c { $script:ran += 'c'; return 'c done' }
$ok = Invoke-NcSteps @('a', 'b', 'c')
[Console]::Out.WriteLine("ok=$ok ran=$($script:ran -join ',')")
$script:ran = @()
$ok = Invoke-NcSteps @('a', 'c')
[Console]::Out.WriteLine("ok=$ok ran=$($script:ran -join ',')")
`
	for _, shell := range powerShells(t) {
		t.Run(shellName(shell), func(t *testing.T) {
			out, err := ncwPS(t, shell, body)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			for _, want := range []string{
				"native-chain-windows: step b failed: native-chain-windows: boom in b\n",
				"native-chain-windows: step b failed\n",
				"ok=False ran=a,b\n",
				"ok=True ran=a,c\n",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			for _, row := range []string{`(?m)^a\s+PASS\s+\d+s\s+a done$`, `(?m)^b\s+FAIL\s+\d+s\s+see above$`, `(?m)^c\s+SKIP\s+-\s+after b failed$`} {
				if !regexp.MustCompile(row).MatchString(out) {
					t.Errorf("results lack %s:\n%s", row, out)
				}
			}
		})
	}
}

// Get-NcSlotPort is scripts/ports.sh's slot_port: base + slot x 10, the
// port given, refusals of an unset slot on CI, an unknown slot and the
// live installs' ports.
func TestNativeChainWindowsSlotPort(t *testing.T) {
	cases := []struct {
		env        []string
		port, want string // want "" for a refusal
	}{
		{nil, "", "28750"},
		{[]string{"BACKSIGHT_CI_SLOT=2"}, "", "28770"},
		{[]string{"BACKSIGHT_CI_SLOT=2", "CI=true"}, "", "28770"},
		{[]string{"CI=true"}, "", ""},
		{[]string{"BACKSIGHT_CI_SLOT=4"}, "", ""},
		{nil, "31000", "31000"},
		{nil, "8888", ""},
		{nil, "7777", ""}, // its UI would be 8888
		{nil, "abc", ""},
	}
	for _, shell := range powerShells(t) {
		t.Run(shellName(shell), func(t *testing.T) {
			for _, c := range cases {
				out, err := ncwPS(t, shell, "try { [Console]::Out.WriteLine('port=' + (Get-NcSlotPort 28750 '"+c.port+"')) } catch { [Console]::Out.WriteLine('refused: ' + $_.Exception.Message) }", c.env...)
				if err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
				switch {
				case c.want != "" && strings.TrimSpace(out) != "port="+c.want:
					t.Errorf("%v port %q: %q, want %s", c.env, c.port, out, c.want)
				case c.want == "" && !strings.HasPrefix(strings.TrimSpace(out), "refused: "):
					t.Errorf("%v port %q: %q, want a refusal", c.env, c.port, out)
				}
			}
		})
	}
}

// What goes to a CI log is masked: every secret of 8 characters or more
// in secrets.toml.
func TestNativeChainWindowsMasksTheSecrets(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config", "secrets.toml"), []byte("db_password = \"pw-S3CR3T-long\"\napi_key = \"key-0123456789\"\nshort = \"abc\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, shell := range powerShells(t) {
		t.Run(shellName(shell), func(t *testing.T) {
			out, err := ncwPS(t, shell, `[Console]::Out.WriteLine((Protect-NcText 'url postgres://hindsight:pw-S3CR3T-long@127.0.0.1 key key-0123456789 abc'))`, "BACKSIGHT_HOME="+home)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if got := strings.TrimSpace(out); got != "url postgres://hindsight:***@127.0.0.1 key *** abc" {
				t.Errorf("masked: %q", got)
			}
		})
	}
}

// Each argument reaches the program as one, whatever it holds (Windows
// has no argv: CommandLineToArgvW's rules).
func TestNativeChainWindowsQuotesArguments(t *testing.T) {
	cases := map[string]string{
		`plain`:                     `plain`,
		`C:\dir\x.exe`:              `C:\dir\x.exe`,
		`with space`:                `"with space"`,
		`C:\dir with space\`:        `"C:\dir with space\\"`,
		`say "hi"`:                  `"say \"hi\""`,
		`back\"slash`:               `"back\\\"slash"`,
		``:                          `""`,
		`-p 28760 -c x=127.0.0.1`:   `"-p 28760 -c x=127.0.0.1"`,
		"tab\there":                 "\"tab\there\"",
		`end\\`:                     `end\\`,
		`two words\\`:               `"two words\\\\"`,
		`Native chain note 1: the.`: `"Native chain note 1: the."`,
	}
	for _, shell := range powerShells(t) {
		t.Run(shellName(shell), func(t *testing.T) {
			for in, want := range cases {
				out, err := ncwPS(t, shell, "[Console]::Out.Write((ConvertTo-NcArg "+psQuote(in)+"))")
				if err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
				if out != want {
					t.Errorf("ConvertTo-NcArg %q = %q, want %q", in, out, want)
				}
			}
		})
	}
}

// Two fingerprints compare as the gate does: nothing for equal data
// (Hindsight's history tables left out), one line per difference
// otherwise.
func TestNativeChainWindowsComparesFingerprints(t *testing.T) {
	body := `
$a = '{"tables":{"documents":{"rows":3,"keys_md5":"d1"},"memory_units":{"rows":9,"keys_md5":"m1"},"llm_requests":{"rows":5,"keys_md5":"l1"}},"banks":["b"],"control_documents":{"backsight:epoch":"e1"}}' | ConvertFrom-Json
$b = '{"tables":{"documents":{"rows":3,"keys_md5":"d1"},"memory_units":{"rows":9,"keys_md5":"m1"},"llm_requests":{"rows":8,"keys_md5":"l2"}},"banks":["b"],"control_documents":{"backsight:epoch":"e1"}}' | ConvertFrom-Json
$c = '{"tables":{"documents":{"rows":4,"keys_md5":"d2"},"memory_units":{"rows":9,"keys_md5":"m2"}},"banks":["b"],"control_documents":{"backsight:epoch":"e2"}}' | ConvertFrom-Json
[Console]::Out.WriteLine("equal=" + @(Compare-NcFingerprint $a $b).Count)
[Console]::Out.WriteLine("diff=" + (@(Compare-NcFingerprint $a $c) -join '|'))
`
	for _, shell := range powerShells(t) {
		t.Run(shellName(shell), func(t *testing.T) {
			out, err := ncwPS(t, shell, body)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			for _, want := range []string{"equal=0\n", "diff=table documents: rows 3 -> 4|table memory_units: rows 9, keys differ|control document backsight:epoch\n"} {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
		})
	}
}

// The data is the same when no key is gone or added but the control
// bank's: the restore epoch's document, its memory unit (named by the
// control bank's links) and its links, which a restore or a rollback
// writes again; a key of the user's bank gone or added is a difference.
func TestNativeChainWindowsComparesKeys(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	link := func(a, b, bank string) string { return `"(` + a + `,` + b + `,semantic,,1,\"t\",` + bank + `)"` }
	keys := func(docs, chunks, units, links string) string {
		return `{"documents":[` + docs + `],"chunks":[` + chunks + `],"memory_units":[` + units + `],"memory_links":[` + links + `],"llm_requests":["(r1)"]}`
	}
	docs := `"(d1,bk)","(backsight:epoch,bk-control)","(backsight:server,bk-control)"`
	before := write("before.keys", keys(docs, `"(bk-control_backsight:epoch_c1)"`, `"(u1)","(u2)","(e1)","(s1)"`,
		link("u1", "u2", "bk")+","+link("e1", "s1", "bk-control")))
	// The epoch document rewritten: its chunk, its unit and the link to it
	// replaced, the history table grown.
	same := write("same.keys", strings.Replace(keys(docs, `"(bk-control_backsight:epoch_c2)"`, `"(u1)","(u2)","(e2)","(s1)"`,
		link("u1", "u2", "bk")+","+link("e2", "s1", "bk-control")), `"(r1)"`, `"(r1)","(r2)"`, 1))
	// The user's rows lost or new.
	lost := write("lost.keys", keys(`"(backsight:epoch,bk-control)","(backsight:server,bk-control)"`, `"(bk-control_backsight:epoch_c1)"`,
		`"(u1)","(e1)","(s1)","(u9)"`, link("e1", "s1", "bk-control")))
	// The server document lost, with its unit and link: no unit added.
	lostCtl := write("lostctl.keys", keys(`"(d1,bk)","(backsight:epoch,bk-control)"`, `"(bk-control_backsight:epoch_c1)"`,
		`"(u1)","(u2)","(e1)"`, link("u1", "u2", "bk")))
	// Two control units replaced: more than the epoch document's one.
	twice := write("twice.keys", keys(docs, `"(bk-control_backsight:epoch_c2)"`, `"(u1)","(u2)","(e2)","(s2)"`,
		link("u1", "u2", "bk")+","+link("e2", "s2", "bk-control")))
	// The first restore: the epoch document new, with its unit and links.
	first := write("first.keys", keys(docs, `"(bk-control_backsight:epoch_c1)"`, `"(u1)","(u2)","(e1)","(s1)"`,
		link("u1", "u2", "bk")+","+link("e1", "s1", "bk-control")))
	firstBefore := write("firstbefore.keys", keys(`"(d1,bk)","(backsight:server,bk-control)"`, ``, `"(u1)","(u2)","(s1)"`, link("u1", "u2", "bk")))
	// A control unit and its link added, the epoch document not new.
	unitOnly := write("unitonly.keys", keys(`"(d1,bk)","(backsight:server,bk-control)"`, ``, `"(u1)","(u2)","(s1)","(e1)"`,
		link("u1", "u2", "bk")+","+link("e1", "s1", "bk-control")))
	cmp := func(label, after string) string {
		return "[Console]::Out.WriteLine('" + label + "=' + (@(Compare-NcKeys " + psQuote(before) + " " + psQuote(after) + " 'bk-control') -join '|') + ' control=' + $script:controlChanged)\n"
	}
	body := cmp("same", same) + cmp("lost", lost) + cmp("lostctl", lostCtl) + cmp("twice", twice) +
		"[Console]::Out.WriteLine('first=' + (@(Compare-NcKeys " + psQuote(firstBefore) + " " + psQuote(first) + " 'bk-control') -join '|') + ' control=' + $script:controlChanged)\n" +
		"[Console]::Out.WriteLine('unitonly=' + (@(Compare-NcKeys " + psQuote(firstBefore) + " " + psQuote(unitOnly) + " 'bk-control') -join '|'))\n" +
		"$a = [pscustomobject]@{ control_documents = [pscustomobject]@{ 'backsight:epoch' = 'e1'; 'backsight:server' = 's1' } }\n" +
		"$b = [pscustomobject]@{ control_documents = [pscustomobject]@{ 'backsight:epoch' = 'e2'; 'backsight:server' = 's1' } }\n" +
		"$c = [pscustomobject]@{ control_documents = [pscustomobject]@{ 'backsight:epoch' = 'e1' } }\n" +
		"[Console]::Out.WriteLine('docs-same=' + (@(Compare-NcControlDocs $a $b) -join '|') + ' docs-lost=' + (@(Compare-NcControlDocs $a $c) -join '|'))\n"
	for _, shell := range powerShells(t) {
		t.Run(shellName(shell), func(t *testing.T) {
			out, err := ncwPS(t, shell, body)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			for _, want := range []string{
				"same= control=6\n",
				"lost=gone: documents (d1,bk)|gone: memory_links (u1,u2,semantic,,1,\"t\",bk)|gone: memory_units (u2)|added: memory_units (u9) control=0\n",
				"lostctl=gone: documents (backsight:server,bk-control)|gone: memory_units (s1) (control units gone 1, added 0: the epoch document's rewrite replaces one)|gone: memory_links (e1,s1,semantic,,1,\"t\",bk-control) (a control link joining no unit of the epoch document) control=0\n",
				"twice=gone: memory_units (e1) (control units gone 2, added 2",
				"added: memory_units (s2) (control units gone 2, added 2",
				"docs-same= docs-lost=control document backsight:server\n",
				"first= control=4\n",
				"unitonly=added: memory_units (e1) (control units gone 0, added 1: the epoch document's rewrite replaces one)|added: memory_links (e1,s1,semantic,,1,\"t\",bk-control) (a control link joining no unit of the epoch document)\n",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
		})
	}
}
