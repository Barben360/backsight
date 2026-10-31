package dashboard

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/paths"
)

// codeFiles lists the names in codes/, sorted.
func codeFiles(t *testing.T, d paths.Dirs) []string {
	t.Helper()
	entries, err := os.ReadDir(CodesDir(d))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func mint(t *testing.T, d paths.Dirs, at time.Time) string {
	t.Helper()
	code, err := MintCode(d, at)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func TestMintWritesTheHashOnly(t *testing.T) {
	d, now := testDirs(t), testNow()
	defer func(r func([]byte) (int, error)) { randRead = r }(randRead)
	randRead = func(b []byte) (int, error) {
		for i := range b {
			b[i] = byte(i * 0x11)
		}
		return len(b), nil
	}
	code := mint(t, d, now)
	if code != "00112233445566778899aabbccddeeff" {
		t.Fatalf("code %q", code)
	}
	const name = "5947d7c33d783f94b3b4c1a96ebc8991ed28f1b069b71e03376cba8caa98a720"
	if got := codeFiles(t, d); len(got) != 1 || got[0] != name {
		t.Fatalf("codes/ holds %v, want [%s]", got, name)
	}
	fi, err := os.Stat(filepath.Join(CodesDir(d), name))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 0 {
		t.Errorf("size %d, want 0", fi.Size())
	}
	if !fi.ModTime().Equal(now) {
		t.Errorf("mtime %v, want %v", fi.ModTime(), now)
	}
	if runtime.GOOS != "windows" {
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("code file mode %v, want 0600", fi.Mode().Perm())
		}
		if di, err := os.Stat(CodesDir(d)); err != nil || di.Mode().Perm() != 0o700 {
			t.Errorf("codes dir mode %v (%v), want 0700", di.Mode().Perm(), err)
		}
	}
	err = filepath.WalkDir(filepath.Join(d.State, "dashboard"), func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(e.Name(), code) {
			t.Errorf("%s is named by the code", p)
		}
		if e.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(raw), code) || strings.Contains(string(raw), "\x00\x11\x22\x33") {
			t.Errorf("%s holds the code", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMintCodesDiffer(t *testing.T) {
	d, now := testDirs(t), testNow()
	if a, b := mint(t, d, now), mint(t, d, now); a == b || len(a) != 32 {
		t.Errorf("codes %q and %q", a, b)
	}
	if got := codeFiles(t, d); len(got) != 2 {
		t.Errorf("codes/ holds %v, want 2 files", got)
	}
}

func TestMintFailsWithoutRandomness(t *testing.T) {
	d, now := testDirs(t), testNow()
	defer func(r func([]byte) (int, error)) { randRead = r }(randRead)
	randRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	if code, err := MintCode(d, now); err == nil || code != "" {
		t.Errorf("MintCode: %q, %v; want an error", code, err)
	}
}

func TestAcceptOnce(t *testing.T) {
	d, now := testDirs(t), testNow()
	code := mint(t, d, now)
	if err := AcceptCode(d, code, now); err != nil {
		t.Fatal(err)
	}
	if got := codeFiles(t, d); len(got) != 0 {
		t.Errorf("accepted code left %v", got)
	}
	if err := AcceptCode(d, code, now); !errors.Is(err, ErrNoCode) {
		t.Errorf("second accept: %v, want ErrNoCode", err)
	}
}

func TestAcceptUnknownCode(t *testing.T) {
	d, now := testDirs(t), testNow()
	mint(t, d, now)
	if err := AcceptCode(d, "0123456789abcdef0123456789abcdef", now); !errors.Is(err, ErrNoCode) {
		t.Errorf("unknown code: %v, want ErrNoCode", err)
	}
	if got := codeFiles(t, d); len(got) != 1 {
		t.Errorf("codes/ holds %v, want the minted one", got)
	}
}

func TestAcceptRefusesMalformed(t *testing.T) {
	d, now := testDirs(t), testNow()
	code := mint(t, d, now)
	before := codeFiles(t, d)
	for _, bad := range []string{
		"xyz",
		code[:31],
		code + "0",
		strings.ToUpper(code),
		"../../x",
		"",
	} {
		if err := AcceptCode(d, bad, now); !errors.Is(err, ErrBadCode) {
			t.Errorf("%q: %v, want ErrBadCode", bad, err)
		}
	}
	if got := codeFiles(t, d); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Errorf("codes/ changed: %v, was %v", got, before)
	}
	if err := AcceptCode(d, code, now); err != nil {
		t.Errorf("the minted code after the refusals: %v", err)
	}
}

func TestAcceptRefusesExpired(t *testing.T) {
	d, now := testDirs(t), testNow()
	old := mint(t, d, now.Add(-121*time.Second))
	if err := AcceptCode(d, old, now); !errors.Is(err, ErrCodeExpired) {
		t.Errorf("121 s old: %v, want ErrCodeExpired", err)
	}
	if got := codeFiles(t, d); len(got) != 0 {
		t.Errorf("expired code left %v", got)
	}
	if err := AcceptCode(d, mint(t, d, now.Add(-119*time.Second)), now); err != nil {
		t.Errorf("119 s old: %v, want accepted", err)
	}
}

// The window is closed: exactly CodeTTL away, either way, is accepted.
func TestCodeTTLBoundary(t *testing.T) {
	d, now := testDirs(t), testNow()
	if err := AcceptCode(d, mint(t, d, now.Add(-CodeTTL)), now); err != nil {
		t.Errorf("120 s old: %v, want accepted", err)
	}
	if err := AcceptCode(d, mint(t, d, now.Add(CodeTTL)), now); err != nil {
		t.Errorf("120 s ahead: %v, want accepted", err)
	}
}

// A symlink or a directory named by a valid code's hash is not a minted
// code: refused and left in place.
func TestAcceptRefusesNonRegularFiles(t *testing.T) {
	now := testNow()
	for _, kind := range []string{"symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			d := testDirs(t)
			const code = "0123456789abcdef0123456789abcdef"
			if err := os.MkdirAll(CodesDir(d), 0o700); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(CodesDir(d), codeName(code))
			if kind == "symlink" {
				target := filepath.Join(t.TempDir(), "target")
				if err := os.WriteFile(target, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, p); err != nil {
					t.Skipf("no symlinks here: %v", err)
				}
			} else if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := AcceptCode(d, code, now); !errors.Is(err, ErrNoCode) {
				t.Errorf("%s: %v, want ErrNoCode", kind, err)
			}
			if _, err := os.Lstat(p); err != nil {
				t.Errorf("%s removed: %v", kind, err)
			}
		})
	}
}

// A clock stepped back after the mint: a code from the future is as
// suspect as an old one.
func TestCodeFromTheFuture(t *testing.T) {
	d, now := testDirs(t), testNow()
	if err := AcceptCode(d, mint(t, d, now.Add(121*time.Second)), now); !errors.Is(err, ErrCodeExpired) {
		t.Errorf("121 s ahead: %v, want ErrCodeExpired", err)
	}
	if got := codeFiles(t, d); len(got) != 0 {
		t.Errorf("future code left %v", got)
	}
	if err := AcceptCode(d, mint(t, d, now.Add(60*time.Second)), now); err != nil {
		t.Errorf("60 s ahead: %v, want accepted", err)
	}
}

func TestAcceptRace(t *testing.T) {
	d, now := testDirs(t), testNow()
	code := mint(t, d, now)
	const n = 8
	start := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- AcceptCode(d, code, now)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	accepted, spent := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrNoCode):
			spent++
		default:
			t.Errorf("unexpected: %v", err)
		}
	}
	if accepted != 1 || spent != n-1 {
		t.Errorf("%d accepted, %d ErrNoCode; want 1 and %d", accepted, spent, n-1)
	}
}

// Another request spends the code between this one's check and its
// removal: this one is refused.
func TestAcceptLosesTheRace(t *testing.T) {
	d, now := testDirs(t), testNow()
	code := mint(t, d, now)
	defer func(f func()) { beforeCodeRemove = f }(beforeCodeRemove)
	beforeCodeRemove = func() {
		beforeCodeRemove = func() {}
		if err := AcceptCode(d, code, now); err != nil {
			t.Errorf("the other request: %v", err)
		}
	}
	if err := AcceptCode(d, code, now); !errors.Is(err, ErrNoCode) {
		t.Errorf("the late request: %v, want ErrNoCode", err)
	}
}

func TestSweepCodes(t *testing.T) {
	d, now := testDirs(t), testNow()
	mint(t, d, now.Add(-200*time.Second))
	mint(t, d, now.Add(200*time.Second))
	fresh := mint(t, d, now)
	notes := filepath.Join(CodesDir(d), "notes.txt")
	if err := os.WriteFile(notes, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(notes, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(CodesDir(d), codeName("0123456789abcdef0123456789abcdef"))
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dir, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := SweepCodes(d, now, false); err != nil {
		t.Fatal(err)
	}
	if got := codeFiles(t, d); len(got) != 3 || got[2] != "notes.txt" || !slices.Contains(got, codeName(fresh)) || !slices.Contains(got, filepath.Base(dir)) {
		t.Errorf("after the expired sweep: %v, want the fresh code, the directory and notes.txt", got)
	}
	if err := SweepCodes(d, now, true); err != nil {
		t.Fatal(err)
	}
	if got := codeFiles(t, d); len(got) != 2 || got[1] != "notes.txt" || !slices.Contains(got, filepath.Base(dir)) {
		t.Errorf("after the full sweep: %v, want the directory and notes.txt", got)
	}
	if err := AcceptCode(d, fresh, now); !errors.Is(err, ErrNoCode) {
		t.Errorf("a swept code: %v, want ErrNoCode", err)
	}
}

// A claim another request holds, or a crash left, refuses the code and
// leaves its file; a sweep removes both.
func TestAcceptRefusesAClaimedCode(t *testing.T) {
	d, now := testDirs(t), testNow()
	code := mint(t, d, now)
	if err := os.WriteFile(filepath.Join(CodesDir(d), codeName(code)+claimSuffix), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AcceptCode(d, code, now); !errors.Is(err, ErrNoCode) {
		t.Errorf("a claimed code: %v, want ErrNoCode", err)
	}
	if got := codeFiles(t, d); !slices.Equal(got, []string{codeName(code), codeName(code) + claimSuffix}) {
		t.Errorf("after the refusal: %v, want the code and its claim", got)
	}
	if err := SweepCodes(d, now, true); err != nil {
		t.Fatal(err)
	}
	if got := codeFiles(t, d); len(got) != 0 {
		t.Errorf("after the full sweep: %v, want nothing", got)
	}
}

// Accepting leaves neither the code nor its claim.
func TestAcceptLeavesNoClaim(t *testing.T) {
	d, now := testDirs(t), testNow()
	if err := AcceptCode(d, mint(t, d, now), now); err != nil {
		t.Fatal(err)
	}
	if err := AcceptCode(d, mint(t, d, now.Add(-200*time.Second)), now); !errors.Is(err, ErrCodeExpired) {
		t.Fatalf("an expired code: %v, want ErrCodeExpired", err)
	}
	if got := codeFiles(t, d); len(got) != 0 {
		t.Errorf("codes/ after a login and an expired code: %v, want nothing", got)
	}
}

// Claims a crash left are swept like codes; names that only look like one
// are left.
func TestSweepClaims(t *testing.T) {
	d, now := testDirs(t), testNow()
	if err := os.MkdirAll(CodesDir(d), 0o700); err != nil {
		t.Fatal(err)
	}
	old := codeName("old") + claimSuffix
	fresh := codeName("fresh") + claimSuffix
	keep := []string{strings.Repeat("a", 63) + claimSuffix, codeName("x") + claimSuffix + claimSuffix, "notes" + claimSuffix}
	for _, n := range append([]string{old, fresh}, keep...) {
		p := filepath.Join(CodesDir(d), n)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(filepath.Join(CodesDir(d), fresh), now, now); err != nil {
		t.Fatal(err)
	}
	if err := SweepCodes(d, now, false); err != nil {
		t.Fatal(err)
	}
	want := append([]string{fresh}, keep...)
	sort.Strings(want)
	if got := codeFiles(t, d); !slices.Equal(got, want) {
		t.Errorf("after the expired sweep: %v, want %v", got, want)
	}
	if err := SweepCodes(d, now, true); err != nil {
		t.Fatal(err)
	}
	sort.Strings(keep)
	if got := codeFiles(t, d); !slices.Equal(got, keep) {
		t.Errorf("after the full sweep: %v, want %v", got, keep)
	}
}

func TestSweepWithoutCodesDir(t *testing.T) {
	if err := SweepCodes(testDirs(t), testNow(), true); err != nil {
		t.Errorf("no codes/: %v", err)
	}
}

func TestURL(t *testing.T) {
	if got := URL(4242, "00112233445566778899aabbccddeeff"); got != "http://127.0.0.1:4242/?code=00112233445566778899aabbccddeeff" {
		t.Errorf("URL %q", got)
	}
}
