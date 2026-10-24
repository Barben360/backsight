package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Secrets is secrets.toml (SPEC Appendix A.2), the only secret store in v1.
// Its values never print: every fmt verb and JSON show which ones are set.
type Secrets struct {
	APIKey         string `toml:"api_key"`
	UIKey          string `toml:"ui_key"`
	DBPassword     string `toml:"db_password"`
	LLMAPIKey      string `toml:"llm_api_key"`
	BackupIdentity string `toml:"backup_identity"`
}

// Names maps each secret name to whether it is set.
func (s Secrets) Names() map[string]bool {
	return map[string]bool{
		"api_key": s.APIKey != "", "ui_key": s.UIKey != "", "db_password": s.DBPassword != "",
		"llm_api_key": s.LLMAPIKey != "", "backup_identity": s.BackupIdentity != "",
	}
}

func (s Secrets) Format(f fmt.State, _ rune) { _, _ = fmt.Fprintf(f, "Secrets%v", s.Names()) }

func (s Secrets) MarshalJSON() ([]byte, error) { return json.Marshal(s.Names()) }

// LoadSecrets reads path, refusing it when group or others can read it.
func LoadSecrets(path string) (Secrets, error) {
	var s Secrets
	// One handle for the mode check and the read: no window to swap the file.
	f, err := os.Open(path)
	if err != nil {
		return s, err
	}
	defer func() { _ = f.Close() }()
	if runtime.GOOS != "windows" {
		fi, err := f.Stat()
		if err != nil {
			return s, err
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return s, fmt.Errorf("%s is readable by others (mode %o): run chmod 600 %s", path, fi.Mode().Perm(), path)
		}
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return s, err
	}
	dec := toml.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		// Only key paths and positions: decode errors can quote values.
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			var keys []string
			for _, e := range strict.Errors {
				keys = append(keys, strings.Join(e.Key(), "."))
			}
			return Secrets{}, fmt.Errorf("%s: unknown keys: %s", path, strings.Join(keys, ", "))
		}
		var de *toml.DecodeError
		if errors.As(err, &de) {
			row, col := de.Position()
			return Secrets{}, fmt.Errorf("%s:%d:%d: invalid TOML", path, row, col)
		}
		return Secrets{}, fmt.Errorf("%s: invalid TOML", path)
	}
	return s, nil
}
