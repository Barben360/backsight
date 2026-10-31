package dashboard

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/paths"
)

// CodeTTL is how long a login code is accepted after it was minted.
const CodeTTL = 120 * time.Second

// randRead is the code's randomness: tests stub it.
var randRead = rand.Read

// beforeCodeRemove runs between a code's check and its removal: tests
// spend the code there, as a concurrent request would.
var beforeCodeRemove = func() {}

var (
	ErrBadCode     = errors.New("malformed login code")
	ErrNoCode      = errors.New("unknown or already used login code")
	ErrCodeExpired = errors.New("expired login code")
)

// MintCode makes a one-time login code: 128 random bits as 32 lower-case
// hex characters. Only its SHA-256 is kept, as the name of an empty file
// in CodesDir whose mtime is now: the clock AcceptCode compares with.
func MintCode(d paths.Dirs, now time.Time) (string, error) {
	code, err := mintCode(d, now)
	if err != nil {
		return "", startFail(StartLoginCode, err)
	}
	return code, nil
}

func mintCode(d paths.Dirs, now time.Time) (string, error) {
	var b [16]byte
	if _, err := randRead(b[:]); err != nil {
		return "", fmt.Errorf("draw a login code: %w", err)
	}
	code := hex.EncodeToString(b[:])
	dir := CodesDir(d)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create the login codes directory: %w", err)
	}
	path := filepath.Join(dir, codeName(code))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("write a login code: %w", err)
	}
	err = f.Close()
	if err == nil {
		err = os.Chtimes(path, now, now)
	}
	if err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("write a login code: %w", err)
	}
	return code, nil
}

// AcceptCode spends code. Of several requests carrying it only one is
// accepted: the one that creates the code's claim file (an exclusive
// create, which has a single winner on every system) and then removes the
// code file. Removing alone would not do: concurrent removals of one name
// can all succeed on Windows. The claim is held until the removal returns,
// so removals never overlap, and a request that claims after the code is
// gone finds nothing to remove. The code is never compared: its hash names
// the file, so no comparison leaks it through timing. A code minted more
// than CodeTTL from now, before or after (a clock stepped back), is
// refused and removed.
func AcceptCode(d paths.Dirs, code string, now time.Time) error {
	if !lowerHex(code, 32) {
		return ErrBadCode
	}
	path := filepath.Join(CodesDir(d), codeName(code))
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNoCode
	}
	if err != nil {
		return fmt.Errorf("read a login code: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return ErrNoCode
	}
	if expired(fi, now) {
		return spend(path, func(err error) error {
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove an expired login code: %w", err)
			}
			return ErrCodeExpired
		})
	}
	beforeCodeRemove()
	return spend(path, func(err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNoCode // another request spent it first
		}
		if err != nil {
			return fmt.Errorf("spend a login code: %w", err)
		}
		return nil
	})
}

// claimSuffix ends the name of a code's claim file: the code's file name
// followed by it, never mistaken for a code file.
const claimSuffix = ".claim"

// spend removes the code file at path while holding its claim, and returns
// what result makes of the removal's error. A claim another request holds
// means that request is spending the code: ErrNoCode, and nothing is
// removed. A claim left by a crash refuses the code until a sweep.
func spend(path string, result func(error) error) error {
	claim := path + claimSuffix
	f, err := os.OpenFile(claim, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return ErrNoCode
	}
	if err != nil && runtime.GOOS == "windows" && errors.Is(err, fs.ErrPermission) && gone(path) {
		// Windows denies a name another request's claim still holds while
		// it is being removed: that request has spent the code.
		return ErrNoCode
	}
	if err != nil {
		return fmt.Errorf("claim a login code: %w", err)
	}
	_ = f.Close()
	err = result(os.Remove(path))
	_ = os.Remove(claim) // one left keeps the code refused until a sweep
	return err
}

// SweepCodes removes the code files of CodesDir and the claims a crash
// left: every one (all), or the expired ones. Other files are left.
func SweepCodes(d paths.Dirs, now time.Time, all bool) error {
	dir := CodesDir(d)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list the login codes: %w", err)
	}
	var errs []error
	for _, e := range entries {
		if !codeOrClaim(e.Name()) || e.IsDir() {
			continue
		}
		if !all {
			fi, err := e.Info()
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if !expired(fi, now) {
				continue
			}
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("sweep the login codes: %w", err)
	}
	return nil
}

// URL is the link that logs in with code.
func URL(port int, code string) string {
	return "http://127.0.0.1:" + strconv.Itoa(port) + "/?code=" + code
}

// codeName is the file name a code is kept under: its SHA-256, in hex.
func codeName(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// expired: the file's mtime, the minter's clock, is more than CodeTTL
// from now either way.
func expired(fi fs.FileInfo, now time.Time) bool {
	age := now.Sub(fi.ModTime())
	return age > CodeTTL || age < -CodeTTL
}

// gone: nothing is left at path.
func gone(path string) bool {
	_, err := os.Lstat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// codeOrClaim: name is a code file's or a claim's.
func codeOrClaim(name string) bool {
	return lowerHex(strings.TrimSuffix(name, claimSuffix), 64)
}

// lowerHex: s is n lower-case hex characters.
func lowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
