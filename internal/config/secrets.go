package config

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/Barben360/backsight/internal/fsutil"
)

// Secrets is secrets.toml (SPEC Appendix A.2), the only secret store in v1.
// Its values never print: every fmt verb and JSON show which ones are set.
type Secrets struct {
	APIKey         string `toml:"api_key,omitempty"`
	UIKey          string `toml:"ui_key,omitempty"`
	DBPassword     string `toml:"db_password,omitempty"`
	LLMAPIKey      string `toml:"llm_api_key,omitempty"`
	BackupIdentity string `toml:"backup_identity,omitempty"`
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

// field returns the secret called name.
func (s *Secrets) field(name string) (*string, error) {
	switch name {
	case "api_key":
		return &s.APIKey, nil
	case "ui_key":
		return &s.UIKey, nil
	case "db_password":
		return &s.DBPassword, nil
	case "llm_api_key":
		return &s.LLMAPIKey, nil
	case "backup_identity":
		return &s.BackupIdentity, nil
	}
	return nil, fmt.Errorf("unknown secret %q (one of api_key, ui_key, db_password, llm_api_key, backup_identity)", name)
}

var dbPasswordRE = regexp.MustCompile(`^[A-Za-z0-9_-]{16,}$`)

// Set stores value under name; empty values are refused.
func (s *Secrets) Set(name, value string) error {
	f, err := s.field(name)
	if err != nil {
		return err
	}
	if value == "" {
		return fmt.Errorf("%s: empty value", name)
	}
	if name == "db_password" && !dbPasswordRE.MatchString(value) {
		return fmt.Errorf("db_password: use at least 16 characters from A-Z, a-z, 0-9, - and _ (it goes into a database URL)")
	}
	*f = value
	return nil
}

// GenerateMissing fills the server-side secrets that are unset with 32
// random bytes (base64url) and returns their names.
func GenerateMissing(s *Secrets) []string {
	var made []string
	for _, name := range []string{"api_key", "db_password", "ui_key"} {
		f, _ := s.field(name)
		if *f != "" {
			continue
		}
		b := make([]byte, 32)
		_, _ = rand.Read(b) // never fails (crypto/rand, Go 1.24+)
		*f = base64.RawURLEncoding.EncodeToString(b)
		made = append(made, name)
	}
	sort.Strings(made)
	return made
}

// UpdateSecrets loads path (missing: empty), applies fn and writes it back
// with mode 0600, unless the file changed meanwhile. fn's error writes nothing.
func UpdateSecrets(path string, fn func(*Secrets) error) error {
	hash, err := fsutil.HashFile(path)
	if err != nil {
		return err
	}
	var s Secrets
	if hash != fsutil.Absent {
		if s, err = LoadSecrets(path); err != nil {
			return err
		}
	}
	if err := fn(&s); err != nil {
		return err
	}
	raw, err := toml.Marshal(s)
	if err != nil {
		return err
	}
	return fsutil.ReplaceIfUnchanged(path, raw, 0o600, hash)
}

// KeylessProviders run without an LLM API key.
var KeylessProviders = map[string]bool{"ollama": true}

// RequiredSecrets says which secrets cfg needs: the server keys for a local
// server, the API key always, the LLM key unless the provider needs none.
func RequiredSecrets(cfg Config) map[string]bool {
	local := cfg.Server.Mode == "local"
	return map[string]bool{
		"api_key":         true,
		"ui_key":          local,
		"db_password":     local,
		"llm_api_key":     local && !KeylessProviders[cfg.LLM.Provider],
		"backup_identity": false, // backups arrive in M5
	}
}
