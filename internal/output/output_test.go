package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// Agents branch on these numbers (SPEC §11.2); changing one breaks them.
func TestExitCodesAreTheContractValues(t *testing.T) {
	want := map[string]int{"OK": 0, "Usage": 2, "NeedsUser": 10, "Blocked": 20, "RolledBack": 30, "Partial": 40}
	got := map[string]int{"OK": OK, "Usage": Usage, "NeedsUser": NeedsUser, "Blocked": Blocked, "RolledBack": RolledBack, "Partial": Partial}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d", k, got[k], v)
		}
	}
}

func TestSuccessEnvelope(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, "version", map[string]string{"version": "1.2.3"}, nil); err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, buf.String())
	}
	if env["schema_version"] != float64(SchemaVersion) || env["command"] != "version" || env["ok"] != true {
		t.Errorf("envelope header wrong: %v", env)
	}
	if env["data"].(map[string]any)["version"] != "1.2.3" {
		t.Errorf("data not carried: %v", env["data"])
	}
	if _, ok := env["error"]; ok {
		t.Errorf("success must not carry an error field: %v", env)
	}
}

func TestErrorEnvelopeCarriesCodeAndMessage(t *testing.T) {
	var buf bytes.Buffer
	cmdErr := &Error{Code: Blocked, Message: "docker is not running"}
	if err := WriteJSON(&buf, "doctor", nil, cmdErr); err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	e := env["error"].(map[string]any)
	if env["ok"] != false || e["code"] != float64(Blocked) || e["message"] != "docker is not running" {
		t.Errorf("error envelope wrong: %v", env)
	}
}

func TestCodeOf(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, OK},
		{&Error{Code: NeedsUser, Message: "x"}, NeedsUser},
		{errors.Join(errors.New("ctx"), &Error{Code: Partial, Message: "y"}), Partial},
		{errors.New("plain failure"), Blocked},
	}
	for _, c := range cases {
		if got := CodeOf(c.err); got != c.want {
			t.Errorf("CodeOf(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}

func TestErrorEnvelopeFromPlainAndWrappedErrors(t *testing.T) {
	for name, c := range map[string]struct {
		err  error
		code int
		msg  string
	}{
		"plain":   {errors.New("disk full"), Blocked, "disk full"},
		"wrapped": {fmt.Errorf("setup: %w", &Error{Code: NeedsUser, Message: "type the key"}), NeedsUser, "type the key"},
	} {
		var buf bytes.Buffer
		if err := WriteJSON(&buf, "x", map[string]int{"n": 1}, c.err); err != nil {
			t.Fatal(err)
		}
		var env struct {
			OK    bool           `json:"ok"`
			Data  map[string]int `json:"data"`
			Error *Error         `json:"error"`
		}
		if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if env.OK || env.Error == nil || env.Error.Code != c.code || env.Error.Message != c.msg || env.Data["n"] != 1 {
			t.Errorf("%s: %s", name, buf.String())
		}
	}
}
