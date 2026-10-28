// Package token is the connection token: what a client PC needs to join a
// server. It holds the API key, so it never prints it (every fmt verb shows
// the safe summary) and its decode errors never quote the input.
package token

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Prefix starts every encoded token; it is also what redaction looks for.
const Prefix = "bst1."

// LLM is the server's LLM provider, kept for cost estimates on a client.
type LLM struct {
	Provider    string  `json:"provider"`
	Model       string  `json:"model"`
	Currency    string  `json:"currency"`
	PriceInput  float64 `json:"price_input"`
	PriceCached float64 `json:"price_cached"`
	PriceOutput float64 `json:"price_output"`
}

// Token is what a client PC needs to join a server.
type Token struct {
	URL              string `json:"url"`
	Bank             string `json:"bank"`
	APIKey           string `json:"api_key"`
	UIURL            string `json:"ui_url,omitempty"`
	HindsightVersion string `json:"hindsight_version,omitempty"`
	ServerID         string `json:"server_id"`
	LLM              LLM    `json:"llm"`
}

// String names the server, never the key.
func (t Token) String() string {
	return fmt.Sprintf("token for %s bank %s server %s", t.URL, t.Bank, t.ServerID)
}

// Format makes every fmt verb print String.
func (t Token) Format(f fmt.State, _ rune) { _, _ = fmt.Fprint(f, t.String()) }

// MarshalJSON writes the summary, never the key: a Token in any JSON output
// stays safe. Encode and Decode use wire, which keeps the key.
func (t Token) MarshalJSON() ([]byte, error) { return json.Marshal(t.String()) }

// wire is Token's encoded form, without Token's methods.
type wire Token

// Encode renders t as Prefix and base64url JSON.
func Encode(t Token) string {
	raw, _ := json.Marshal(wire(t)) // plain strings and numbers: cannot fail
	return Prefix + base64.RawURLEncoding.EncodeToString(raw)
}

// Decode parses an encoded token, spaces and newlines around it ignored.
// Its errors say what is wrong, never the input or the key.
func Decode(s string) (Token, error) {
	s = strings.TrimSpace(s)
	body, ok := strings.CutPrefix(s, Prefix)
	if !ok {
		return Token{}, errors.New("not a backsight connection token (it starts with " + Prefix + ")")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return Token{}, errors.New("connection token is damaged (invalid base64)")
	}
	var w wire
	if err := json.Unmarshal(raw, &w); err != nil {
		return Token{}, errors.New("connection token is damaged (invalid content)")
	}
	t := Token(w)
	for _, f := range []struct{ name, value string }{{"url", t.URL}, {"bank", t.Bank}, {"api_key", t.APIKey}, {"server_id", t.ServerID}} {
		if f.value == "" {
			return Token{}, fmt.Errorf("connection token has no %s", f.name)
		}
	}
	if u, err := url.Parse(t.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Token{}, errors.New("connection token url is not an http(s) address")
	}
	if u, _ := url.Parse(t.URL); u.User != nil {
		return Token{}, errors.New("connection token url must not carry credentials")
	}
	return t, nil
}
