package termstyle

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// fakeConsole replaces the console mode calls; sets records each mode set.
func fakeConsole(t *testing.T, mode uint32, getErr, setErr error) *[]uint32 {
	t.Helper()
	oldGet, oldSet := getConsoleMode, setConsoleMode
	t.Cleanup(func() { getConsoleMode, setConsoleMode = oldGet, oldSet })
	var sets []uint32
	getConsoleMode = func(_ windows.Handle, m *uint32) error {
		*m = mode
		return getErr
	}
	setConsoleMode = func(_ windows.Handle, m uint32) error {
		sets = append(sets, m)
		return setErr
	}
	return &sets
}

func tempFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestEnableVTSetsAndRestores(t *testing.T) {
	sets := fakeConsole(t, 0x3, nil, nil)
	restore, ok := EnableVT(tempFile(t))
	if !ok {
		t.Fatal("not ok")
	}
	if len(*sets) != 1 || (*sets)[0] != 0x3|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING {
		t.Fatalf("sets = %#x", *sets)
	}
	restore()
	restore()
	if len(*sets) != 2 || (*sets)[1] != 0x3 {
		t.Errorf("after restore, sets = %#x", *sets)
	}
}

func TestEnableVTNotAConsole(t *testing.T) {
	sets := fakeConsole(t, 0, errors.New("not a console"), nil)
	if _, ok := EnableVT(tempFile(t)); ok || len(*sets) != 0 {
		t.Errorf("ok %v, sets %#x", ok, *sets)
	}
}

func TestEnableVTSetFails(t *testing.T) {
	fakeConsole(t, 0x3, nil, errors.New("refused"))
	if _, ok := EnableVT(tempFile(t)); ok {
		t.Error("ok despite a failed set")
	}
}

func TestEnableVTNotAFile(t *testing.T) {
	fakeConsole(t, 0x3, nil, nil)
	if _, ok := EnableVT(&bytes.Buffer{}); ok {
		t.Error("ok for a buffer")
	}
}

func TestEnableVTAlreadyOn(t *testing.T) {
	sets := fakeConsole(t, 0x7, nil, nil)
	if _, ok := EnableVT(tempFile(t)); !ok || len(*sets) != 0 {
		t.Errorf("ok %v, sets %#x", ok, *sets)
	}
}
