// Package target resolves which Hindsight server a command talks to: the
// backsight config and its secrets, or, until the cut-over, the reference
// install backsight replaces.
package target

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/reference"
)

// Target is one server and bank. Token never prints.
type Target struct {
	URL    string           `json:"url"`
	Bank   string           `json:"bank"`
	Source string           `json:"source"` // config | reference
	Token  reference.Secret `json:"-"`
}

// ConfigError is an unusable config.toml or secrets.toml.
type ConfigError struct {
	File string
	Err  error
}

func (e *ConfigError) Error() string { return e.Err.Error() }
func (e *ConfigError) Unwrap() error { return e.Err }

// FromConfig reads backsight's config and secrets. No config.toml returns an
// fs.ErrNotExist error; a missing secrets.toml means no API key.
func FromConfig(d paths.Dirs, home string) (*Target, error) {
	cfg, err := config.Load(d.ConfigFile(), d, home)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err != nil {
		return nil, &ConfigError{File: d.ConfigFile(), Err: err}
	}
	sec, err := config.LoadSecrets(d.SecretsFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, &ConfigError{File: d.SecretsFile(), Err: err}
	}
	return &Target{URL: cfg.Server.URL, Bank: cfg.Server.Bank, Source: "config", Token: reference.Secret(sec.APIKey)}, nil
}

// FromReference uses the reference install's plugin config, if it names a server.
func FromReference(in *reference.Install) *Target {
	if in == nil || in.URL == "" {
		return nil
	}
	return &Target{URL: in.URL, Bank: in.Bank, Source: "reference", Token: in.Token}
}

// Display is the URL with any password masked.
func (t Target) Display() string {
	u, err := url.Parse(t.URL)
	if err != nil {
		return "(invalid URL)"
	}
	return u.Redacted()
}

// Cleartext reports whether the API key would cross a network unencrypted:
// plain http to a host that is not loopback.
func (t Target) Cleartext() bool {
	u, err := url.Parse(t.URL)
	if err != nil || u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

func (t Target) String() string {
	return fmt.Sprintf("%s bank %s (from %s)", t.Display(), t.Bank, t.Source)
}
