package token

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The key is assembled at runtime so the file holds no scannable secret.
var key = "k3y-" + strings.Repeat("Zx9Qw", 8)

func full() Token {
	return Token{
		URL: "http://100.64.0.7:8888", Bank: "work", APIKey: key,
		UIURL: "http://100.64.0.7:9999", HindsightVersion: "0.4.2",
		ServerID: "0b6f1c1e-5b0e-4f4e-9d0c-2b8f3c5a7e11",
		LLM: LLM{Provider: "openai", Model: "gpt-x", Currency: "USD",
			PriceInput: 1.5, PriceCached: 0.25, PriceOutput: 6},
	}
}

func enc(json string) string { return Prefix + base64.RawURLEncoding.EncodeToString([]byte(json)) }

func TestRoundTrip(t *testing.T) {
	want := full()
	s := Encode(want)
	if !strings.HasPrefix(s, "bst1.") {
		t.Fatalf("no prefix: %q", s)
	}
	got, err := Decode(s)
	if err != nil || got != want {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
	if got, err = Decode("\n  " + s + " \n"); err != nil || got != want {
		t.Fatalf("surrounding whitespace: %+v, %v", got, err)
	}
}

func TestDecodeRefusals(t *testing.T) {
	ok := `{"url":"http://h:1","bank":"b","api_key":"` + key + `","server_id":"id"}`
	cases := map[string]string{
		"wrong prefix":     "bst2." + base64.RawURLEncoding.EncodeToString([]byte(ok)),
		"bad base64":       Prefix + "!!!not-base64!!!" + key,
		"not json":         enc("hello " + key),
		"missing url":      enc(`{"bank":"b","api_key":"` + key + `","server_id":"id"}`),
		"missing bank":     enc(`{"url":"http://h:1","api_key":"` + key + `","server_id":"id"}`),
		"missing api_key":  enc(`{"url":"http://h:1","bank":"b","server_id":"id"}`),
		"missing id":       enc(`{"url":"http://h:1","bank":"b","api_key":"` + key + `"}`),
		"ftp url":          enc(`{"url":"ftp://h:1","bank":"b","api_key":"` + key + `","server_id":"id"}`),
		"url without host": enc(`{"url":"http://","bank":"b","api_key":"` + key + `","server_id":"id"}`),
		"empty":            "",
	}
	if _, err := Decode(enc(ok[:0] + ok)); err != nil {
		t.Fatalf("control token refused: %v", err)
	}
	for name, in := range cases {
		_, err := Decode(in)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		msg := err.Error()
		if strings.Contains(msg, key) || (in != "" && strings.Contains(msg, strings.TrimPrefix(in, Prefix))) || (in != "" && strings.Contains(msg, in)) {
			t.Errorf("%s: error quotes the input or key: %q", name, msg)
		}
	}
}

func TestNeverPrintsTheKey(t *testing.T) {
	tok := full()
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d"} {
		for _, v := range []any{tok, &tok} {
			if out := fmt.Sprintf(verb, v); strings.Contains(out, key) {
				t.Errorf("%s leaks the key: %q", verb, out)
			}
		}
	}
	if got, want := tok.String(), "token for http://100.64.0.7:8888 bank work server "+tok.ServerID; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if out := fmt.Sprint(tok); out != tok.String() {
		t.Errorf("Sprint = %q", out)
	}
}

func TestJSONNeverCarriesTheKey(t *testing.T) {
	tok := full()
	holder := struct{ Conn Token }{tok}
	for name, v := range map[string]any{"token": tok, "pointer": &tok, "holder": holder} {
		raw, err := json.Marshal(v)
		if err != nil || strings.Contains(string(raw), key) {
			t.Errorf("%s: %s, %v", name, raw, err)
		}
	}
	if got, err := Decode(Encode(tok)); err != nil || got != tok {
		t.Fatalf("round trip lost the key: %+v, %v", got, err)
	}
}
