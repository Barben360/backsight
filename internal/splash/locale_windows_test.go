//go:build windows

package splash

import (
	"errors"
	"testing"
)

func TestUTF8ConsoleCodePage(t *testing.T) {
	saved := consoleOutputCP
	t.Cleanup(func() { consoleOutputCP = saved })
	env := func(string) string { return "en_US.UTF-8" }
	for _, c := range []struct {
		cp   uint32
		err  error
		want bool
	}{{65001, nil, true}, {437, nil, false}, {65001, errors.New("no console"), false}} {
		consoleOutputCP = func() (uint32, error) { return c.cp, c.err }
		if got := UTF8(env); got != c.want {
			t.Errorf("code page %d (err %v): UTF8 = %v, want %v", c.cp, c.err, got, c.want)
		}
	}
}
