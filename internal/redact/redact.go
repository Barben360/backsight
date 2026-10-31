// Package redact removes secrets from text before it leaves the machine
// (SPEC §13). Credential shapes and secret-named values are caught; plain
// prose mentioning a secret is kept.
package redact

import (
	"encoding/base64"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
)

type rule struct {
	kind string
	re   *regexp.Regexp
	// group is the submatch replaced (0: the whole match).
	group int
	// keep decides, for a matched value, that it is not a secret.
	keep func(h hit) bool
}

// hit is a rule's match: the value, and for the key rules the key, what
// separates them (quotes and ':' or '='), and the text after the value.
type hit struct{ key, value, sep, rest string }

// keyRE is a secret-named key: any identifier containing one of the
// keywords, with prefixes and suffixes (SECRET_KEY, GITHUB_TOKEN_PROD,
// apiToken), and "api key" spelt with a space.
const keyRE = `[a-zA-Z0-9_.-]*(?:secret|token|passw(?:or)?d|passphrase|pwd|api[_ -]?key|private[_-]?key|access[_-]?key|credentials?|auth[_-]?key)[a-zA-Z0-9_.-]*`

// passKeyRE is PASS or PW as a whole segment of a key (DB_PASS, PASS,
// pass-db, db_pw, MYSQL_ROOT_PW), not inside a word (bypass, passport).
const passKeyRE = `(?:[a-zA-Z0-9]+[_-])*(?:pass|pw)(?:[_-][a-zA-Z0-9]+)*`

// flagRE names a command-line option whose value is a secret.
const flagRE = `--(?:password|passwd|passphrase|pass|pw|secret|token)`

// keyRules are the rules for a value assigned to a key matching key:
// double-quoted (spaces allowed), JSON-escaped inside a string
// ({\"api_key\": \"value\"}), single-quoted, then bare.
func keyRules(key string) []rule {
	return []rule{
		{kind: "secret_value", re: regexp.MustCompile(`(?i)["']?\b(` + key + `)\b["']?\s*[:=]\s*"([^"\n]{6,})"`), group: 2, keep: keepValue},
		{kind: "secret_value", re: regexp.MustCompile(`(?i)\\"(` + key + `)\\"\s*:\s*\\"([^"\\\n]{6,})\\"`), group: 2, keep: keepValue},
		{kind: "secret_value", re: regexp.MustCompile(`(?i)["']?\b(` + key + `)\b["']?\s*[:=]\s*'([^'\n]{6,})'`), group: 2, keep: keepValue},
		{kind: "secret_value", re: regexp.MustCompile(`(?i)["']?\b(` + key + `)\b["']?\s*[:=]\s*["']?([^\s"',;}]{6,})`), group: 2, keep: keepValue},
	}
}

// cliRule is a command-line secret: re's first group is the value.
func cliRule(re string) rule {
	return rule{kind: "cli_password", re: regexp.MustCompile(re), group: 1, keep: keepFlag}
}

// segmentKeyRE is a key whose last word names a key, a signature or an
// identity (AccountKey, SharedAccessKey, sig, X-Amz-Signature, TWILIO_AUTH,
// backup_identity, client-key-data): such keys name settings as often as
// secrets, so only a random-looking value counts (keepSegmentValue).
// The word before a space is part of the key ("public key: age1…").
const segmentKeyRE = `(?:[a-zA-Z0-9_.-]+ )?[a-zA-Z0-9_.-]*?(?:key|auth|sig|signature|identity)(?:[_-]data)?`

// segmentValueRE is a value of such a key: base64 (standard or URL,
// percent-escaped too, as in an SAS sig) or hex, padding included.
const segmentValueRE = `([A-Za-z0-9+/_%-]{16,}=*)`

var rules = slices.Concat([]rule{
	// A backsight connection token: it carries the API key. First, and with
	// no \b: glued to a word or after an escaped newline (\n) it must still
	// be masked whole, before a broader rule takes a part of it.
	{kind: "backsight_token", re: regexp.MustCompile(`bst1\.[A-Za-z0-9_-]{40,}`)},
	// PEM and PGP armour: the whole block, to its end or the end of the text.
	{kind: "private_key", re: regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY(?: BLOCK)?-----.*?(?:-----END [A-Z ]*PRIVATE KEY(?: BLOCK)?-----|\z)`)},
	// An age identity: backsight's backup key (bech32, upper-case), whole
	// or cut short.
	{kind: "age_identity", re: regexp.MustCompile(`\bAGE-SECRET-KEY-1[0-9A-Z]{16,}`)},
	{kind: "anthropic_key", re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	{kind: "openai_key", re: regexp.MustCompile(`\bsk-(?:proj-|svcacct-)?[A-Za-z0-9_-]{32,}`)},
	{kind: "stripe_key", re: regexp.MustCompile(`\b[sr]k_(?:live|test)_[A-Za-z0-9]{16,}`)},
	{kind: "github_token", re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{22,})`)},
	{kind: "npm_token", re: regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}`)},
	{kind: "slack_token", re: regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
	{kind: "aws_access_key", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{kind: "google_api_key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`)},
	{kind: "gitlab_token", re: regexp.MustCompile(`\bgl(?:pat|ptt|dt|rt|cbt|imt|oas)-[A-Za-z0-9_-]{20,}`)},
	{kind: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
	{kind: "digitalocean_token", re: regexp.MustCompile(`\bdo[opr]_v1_[a-f0-9]{64}\b`)},
	{kind: "shopify_token", re: regexp.MustCompile(`\bshp(?:at|ca|pa|ss)_[a-fA-F0-9]{32}\b`)},
	// A Twilio API key SID: the key's name, but its only identifier.
	{kind: "twilio_key", re: regexp.MustCompile(`\bSK[0-9a-f]{32}\b`)},
	// Webhook URLs: the path is the credential.
	{kind: "slack_webhook", re: regexp.MustCompile(`\bhttps?://hooks\.slack(?:-gov)?\.com/(?:services|workflows|triggers)/([A-Za-z0-9_/-]{8,})`), group: 1},
	{kind: "discord_webhook", re: regexp.MustCompile(`\bhttps?://(?:(?:ptb|canary)\.)?discord(?:app)?\.com/api/(?:v\d+/)?webhooks/([0-9]+/[A-Za-z0-9_-]+)`), group: 1},
	// A TOTP seed (upper-case base32) in an otpauth URL or after the words
	// naming one.
	{kind: "totp_seed", re: regexp.MustCompile(`(?i:\botpauth://[^\s?]*\?(?:[^\s&#]*&)*secret=)([A-Z2-7]{16,}=*)`), group: 1},
	{kind: "totp_seed", re: regexp.MustCompile(`(?i:\b(?:totp|otp|2fa|mfa|authenticator)[ _-]?(?:secret|seed|key)s?)(?:\s+is)?\s*[:=]?\s*["'\x60]?([A-Z2-7]{16,}=*)\b`), group: 1},
	// A Docker config's "auth": base64 of user:password.
	{kind: "docker_auth", re: regexp.MustCompile(`"auth"\s*:\s*"([A-Za-z0-9+/]{8,}=*)"`), group: 1, keep: keepBasic},
	{kind: "docker_auth", re: regexp.MustCompile(`\\"auth\\"\s*:\s*\\"([A-Za-z0-9+/]{8,}=*)\\"`), group: 1, keep: keepBasic},
	// Any scheme in an Authorization header (Token, ApiKey...); Bearer and
	// Basic have their own kinds below.
	{kind: "auth_header", re: regexp.MustCompile(`(?i)\bauthorization["']?\s*[:=]\s*["']?(?:token|apikey|api-key|key)\s+([A-Za-z0-9._~+/-]{16,}=*)`), group: 1},
	{kind: "bearer", re: regexp.MustCompile(`(?i)\bbearer\s+([A-Za-z0-9._~+/-]{16,}=*)`), group: 1},
	{kind: "basic_auth", re: regexp.MustCompile(`(?i)\bbasic\s+([A-Za-z0-9+/]{4,}=*)`), group: 1, keep: keepBasic},
	// The password of user:pass@host runs to the last @ before whitespace;
	// the user may be empty (redis://:pass@host).
	{kind: "url_password", re: regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@]*:(\S+)@[^\s@]+`), group: 1},
	cliRule(`(?:\s|^)(?:-u|--user)\s+[^\s:-][^\s:]*:(\S+)`),
	// --password and its kin, = or a space, the value quoted or bare; a
	// bare value after a space never starts with '-' (the next option).
	cliRule(flagRE + `(?:=|\s+)"([^"\n]+)"`),
	cliRule(flagRE + `(?:=|\s+)'([^'\n]+)'`),
	cliRule(flagRE + `=([^\s"']\S*)`),
	cliRule(flagRE + `\s+([^\s"'-]\S*)`),
	cliRule(`\b(?:mysql|mysqldump|mariadb|mysqladmin)\b[^\n]*?\s-p(\S{4,})`),
	cliRule(`\bsshpass\b[^\n]*?\s-p\s*["']?([^\s"']+)`),
	cliRule(`\bredis-cli\b[^\n]*?\s-a\s+["']?([^\s"']+)`),
	// Samba's user%password (smbclient, rpcclient, net).
	cliRule(`(?:\s|^)(?:-U|--user(?:name)?)(?:=|\s+)["']?[^\s%"']+%([^\s"']+)`),
	// docker login -p and the other registry clients' (--password is above).
	cliRule(`\b(?:docker|podman|nerdctl|buildah|skopeo|oras|helm\s+registry)\b[^\n]*?\blogin\b[^\n]*?\s-p(?:=|\s+)"([^"\n]+)"`),
	cliRule(`\b(?:docker|podman|nerdctl|buildah|skopeo|oras|helm\s+registry)\b[^\n]*?\blogin\b[^\n]*?\s-p(?:=|\s+)'([^'\n]+)'`),
	cliRule(`\b(?:docker|podman|nerdctl|buildah|skopeo|oras|helm\s+registry)\b[^\n]*?\blogin\b[^\n]*?\s-p(?:=|\s+)([^\s"'-]\S*)`),
	// netrc: a password after a machine with a login, a machine named by a
	// host name, or the default entry; one line or several.
	{kind: "netrc_password", re: regexp.MustCompile(`\b(?:machine\s+\S+\s+|default\s+)login\s+\S+\s+(?:account\s+\S+\s+)?password\s+(\S+)`), group: 1, keep: keepFlag},
	{kind: "netrc_password", re: regexp.MustCompile(`\bmachine\s+[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+\s+password\s+(\S+)`), group: 1, keep: keepFlag},
},
	// A value assigned to a secret-named key, each form for both key
	// shapes before the next form.
	interleave(keyRules(keyRE), keyRules(passKeyRE)),
	[]rule{
		// Long hex after the words naming a secret, in prose ("the token is
		// <hex>"); bare hex is a commit, a digest or a checksum and stays.
		{kind: "hex_secret", re: regexp.MustCompile(`(?i:\b(?:secret|token|passw(?:or)?d|passphrase|api[ _-]?key|private[ _-]key|signing[ _-]key|auth[ _-]token)s?)(?:\s+(?:is|was))?\s*[:=]?\s+["'\x60]?([0-9a-fA-F]{32,})\b`), group: 1},
		// A random value of a key ending in key, auth, sig or identity.
		{kind: "secret_value", re: regexp.MustCompile(`(?i)["']?\b(` + segmentKeyRE + `)["']?\s*[:=]\s*["']?` + segmentValueRE), group: 2, keep: keepSegmentValue},
		// A long mixed-case alphanumeric token anywhere (hex, UUIDs, digests and
		// identifiers made of words are kept).
		{kind: "high_entropy", re: regexp.MustCompile(`\b[A-Za-z0-9_-]{32,}\b`), keep: func(h hit) bool { return !highEntropy(h.value) }},
	})

// interleave is a[0], b[0], a[1], b[1]...
func interleave(a, b []rule) []rule {
	var out []rule
	for i := range a {
		out = append(out, a[i], b[i])
	}
	return out
}

// notSecretKeys name a key that is an identifier, a digest or a public
// half, not a secret (cache_key, public_key, primary_key).
var notSecretKeys = regexp.MustCompile(`(?i)public|pub[_-]?key|cache|idempoten|primary|foreign|partition|sort|routing|object|row[_-]?key|dedup|lookup|hash|host[_-]?key|key[_-]?id`)

// keepSegmentValue decides that a value of a key ending in key, auth, sig
// or identity is a setting (auth: true, sig=v1, cache_key=<sha256>,
// identity_file = ~/.ssh/id_ed25519), not a secret: only a random-looking
// value is one.
func keepSegmentValue(h hit) bool {
	return notSecretKeys.MatchString(h.key) || placeholder(h.value) || strings.HasPrefix(h.value, "age1") || !randomValue(h.value)
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// randomValue: 32 hex characters or more, or base64 (percent-escaped too)
// of 20 characters or more holding digits and letters with frequent
// changes of character class, as random bytes do. Identifiers made of
// words (cleColonneEnTeteJour, claude-cli/claude-sonnet-4-6) change class
// rarely or hold no digit; a UUID is an identifier.
func randomValue(v string) bool {
	v = strings.NewReplacer("%2B", "+", "%2b", "+", "%2F", "/", "%2f", "/", "%3D", "=", "%3d", "=").Replace(v)
	core := strings.TrimRight(v, "=")
	if uuidRE.MatchString(core) {
		return false
	}
	hex := true
	var alpha, digits, changes int
	prev := 0
	for _, r := range core {
		c := 0
		switch {
		case r >= '0' && r <= '9':
			digits, c = digits+1, 1
		case r >= 'a' && r <= 'z':
			alpha, c = alpha+1, 2
		case r >= 'A' && r <= 'Z':
			alpha, c = alpha+1, 3
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			hex = false
		}
		if c == 0 {
			continue // separators split words, they are not randomness
		}
		if prev != 0 && c != prev {
			changes++
		}
		prev = c
	}
	if hex {
		return len(core) >= 32
	}
	n := alpha + digits
	return len(core) >= 20 && digits >= 2 && alpha >= 2 && float64(changes)/float64(n) >= 0.3
}

// keepBasic decides that the word after "Basic" is not credentials: a
// long one always is (base64 of user:password), a short one only when it
// decodes to printable text holding the ':' (Basic settings, basic flow).
func keepBasic(h hit) bool {
	v := h.value
	if len(v) >= 12 {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		if raw, err = base64.RawStdEncoding.DecodeString(v); err != nil {
			return true
		}
	}
	for _, c := range raw {
		if c < 0x20 || c > 0x7e {
			return true
		}
	}
	return !strings.Contains(string(raw), ":")
}

// placeholder: a value standing for a secret, not one ($VAR, <your key>,
// {{ .Token }}, an earlier redaction, a mask, an ellipsis).
func placeholder(v string) bool {
	return strings.HasPrefix(v, "$") || strings.HasPrefix(v, "…") || strings.HasPrefix(v, "...") || strings.HasPrefix(v, "<") || strings.HasPrefix(v, "{{") || strings.HasPrefix(v, "[redacted") ||
		strings.HasPrefix(v, "***") || strings.HasPrefix(v, "%")
}

var passwordKeys = regexp.MustCompile(`(?i)passw(?:or)?d|passphrase|pwd|secret|(?:^|[_-])(?:pass|pw)(?:$|[_-])`)

// strongKeys name a secret on their own: a credential-like or long value
// is one.
var strongKeys = regexp.MustCompile(`(?i)passw(?:or)?d|passphrase|pwd|secret|token|private[_-]?key|credentials?|(?:^|[_-])(?:pass|pw)(?:$|[_-])`)

// secretEnd: the key ends with the word naming the secret (DB_PASSWORD,
// client_secret, apiToken, MYSQL_ROOT_PW), so its value is the secret,
// not a setting about it (token_type, password_auth); group 1 is a
// password's word.
var secretEnd = regexp.MustCompile(`(?i)(passw(?:or)?d|passphrase|pwd|secret|(?:^|[_.-])(?:pass|pw))$|token$`)

// keepFlag decides that an option's value is not a secret: a placeholder,
// a usage metavariable (--token TOKEN) or a prose word after a bare
// option (--token values).
func keepFlag(h hit) bool {
	v := strings.Trim(h.value, "`") // markdown code spans in prose
	if placeholder(v) {
		return true
	}
	if !letters(v) {
		return false
	}
	return strings.ToUpper(v) == v || notSecretWords[strings.ToLower(v)]
}

// notSecretWords name a type, the secret itself or a state, or are prose
// function words: as the short plain value of a secret-named key or
// option, they are not the secret (token: string, password=password,
// --token values).
var notSecretWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`string number boolean object secret secrets password passwd passphrase token tokens
		private public required optional hidden masked redacted missing example sample value values
		and or the to is as in of for with from flag flags option here there when if then only also`) {
		notSecretWords[w] = true
	}
}

// letters: ASCII letters only.
func letters(v string) bool {
	for _, r := range v {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return v != ""
}

// assigned: the value is written as one, not as prose or code after the
// key: quoted, after a tight '=' (pass=hunter, -e PW=hunter), or after
// ": " ending its line (YAML). "db_password: restore it", "token: string;"
// and "in.Token = cfgTok" are not.
func assigned(h hit) bool {
	if q := strings.TrimLeft(h.sep, " \t:="); q != "" && strings.HasPrefix(h.rest, q[len(q)-1:]) {
		return true // "value" or 'value'
	}
	core := strings.Trim(h.sep, `"'`)
	if core == "=" {
		return true
	}
	end := h.rest == "" || h.rest[0] == '\n' || h.rest[0] == '\r'
	return strings.HasPrefix(core, ":") && strings.TrimSpace(core) == ":" && len(core) > 1 && end
}

// keepValue decides that a secret-named key's value is not a secret.
func keepValue(h hit) bool {
	key, v := h.key, h.value
	v = strings.Trim(v, "`") // markdown code spans in prose
	if len(v) < 6 {
		return true
	}
	if strings.Contains(v, "://") {
		return true // a URL named after a token page, not a token
	}
	if placeholder(v) || strings.ContainsAny(v, `*\^`) {
		return true
	}
	if numeric(v) {
		// Counters and timings such as max_tokens: 50000 or per_token_ms: 41.2;
		// a numeric password still goes.
		return !passwordKeys.MatchString(key)
	}
	lower := strings.ToLower(key)
	if strings.HasSuffix(lower, "tokens") || strings.Contains(lower, "token_count") {
		return true // token counts, not tokens
	}
	for _, r := range v {
		if r > 127 {
			return true // accented words are prose ("token : Paramètres")
		}
	}
	if letters(strings.TrimRight(v, ")]")) && namesNoSecret(key, v) {
		return true
	}
	if strongKeys.MatchString(key) && (credentialLike(v) || len(v) >= 12) {
		return false
	}
	// A short plain word as the secret itself (pass=hunter, token: abcdefgh,
	// PASSWORD=Summer), when written as a value and not a word naming the
	// secret or a type; a capitalised word after a token is prose
	// ("token : Profil").
	if m := secretEnd.FindStringSubmatch(key); m != nil && (m[1] != "" || !capitalised(v)) && assigned(h) {
		return false
	}
	return !credentialLike(v)
}

// namesNoSecret: v names the key or a type (password=password),
// whatever closing bracket follows it (password=password)).
func namesNoSecret(key, v string) bool {
	w := strings.ToLower(strings.TrimRight(v, ")]"))
	return notSecretWords[w] || strings.Contains(strings.ToLower(key), w)
}

// capitalised: one upper-case letter then lower-case letters (Profil).
func capitalised(v string) bool {
	for i, r := range v {
		if i == 0 && (r < 'A' || r > 'Z') || i > 0 && (r < 'a' || r > 'z') {
			return false
		}
	}
	return len(v) > 1
}

func numeric(v string) bool {
	digit := false
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case r == '.' || r == '-' || r == '+' || r == 'e' || r == 'E':
		default:
			return false
		}
	}
	return digit
}

// credentialLike: letters with digits or symbols, or a long token; plain
// words are prose.
func credentialLike(v string) bool {
	var letter, digit, symbol bool
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			letter = true
		case r > 127:
			return false // accented words are prose
		default:
			symbol = true
		}
	}
	return letter && (digit || symbol || len(v) >= 24)
}

// highEntropy: upper, lower and digits mixed with frequent changes of
// character class, as in random tokens; identifiers made of words
// (expediteurFingerprint, Barben360-atlas-spec) change class rarely.
func highEntropy(v string) bool {
	class := func(r rune) int {
		switch {
		case r >= 'A' && r <= 'Z':
			return 1
		case r >= 'a' && r <= 'z':
			return 2
		case r >= '0' && r <= '9':
			return 3
		}
		return 0
	}
	var counts [4]int
	changes, prev := 0, -1
	for _, r := range v {
		c := class(r)
		counts[c]++
		if c == 0 {
			continue // separators split words, they are not randomness
		}
		if prev >= 0 && c != prev {
			changes++
		}
		prev = c
	}
	if counts[1] < 2 || counts[2] < 2 || counts[3] < 2 {
		return false
	}
	return float64(changes)/float64(len(v)) >= 0.3
}

// MinSecret is the shortest exact secret value replaced: a shorter one
// would eat ordinary words, and backsight's own secrets are 32 random
// bytes.
const MinSecret = 8

// LocalKind is the kind of an exact value given by SetLocalSecrets.
const LocalKind = "local_secret"

var local struct {
	mu     sync.Mutex
	load   func() []string
	loaded bool
	values []string // with their encodings, longest first
}

// SetLocalSecrets makes Text replace the exact values load returns (those
// of secrets.toml and the server's .env), whatever their shape, before any
// rule. load runs once, at the first Text after this call, so a command
// that redacts nothing reads no secret; a later call replaces it.
func SetLocalSecrets(load func() []string) {
	local.mu.Lock()
	defer local.mu.Unlock()
	local.load, local.loaded, local.values = load, false, nil
}

func localSecrets() []string {
	local.mu.Lock()
	defer local.mu.Unlock()
	if !local.loaded && local.load != nil {
		local.loaded = true
		var out []string
		for _, v := range local.load() {
			if len(v) < MinSecret {
				continue
			}
			for _, f := range Encodings(v) {
				if !slices.Contains(out, f) {
					out = append(out, f)
				}
			}
		}
		sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
		local.values = out
	}
	return local.values
}

// Encodings are v and its URL-escaped forms: query, path, the path form
// of hindsight's bankPath, which escapes ':' too, and the password of a
// URL's userinfo (a database URL's).
func Encodings(v string) []string {
	out := []string{v}
	userinfo := strings.TrimPrefix(url.UserPassword("", v).String(), ":")
	for _, f := range []string{url.QueryEscape(v), url.PathEscape(v), strings.ReplaceAll(url.PathEscape(v), ":", "%3A"), userinfo} {
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// MinValue is the shortest value Values replaces.
const MinValue = 6

// Values is text with every one of values (MinValue bytes or more) and its
// URL-escaped forms replaced by [redacted]: the runtime's redaction of the
// known secrets, for error messages quoting a tool and for the native
// server's logs (a database URL carries the password escaped). Unlike
// Text, it applies no rule: a value is replaced wherever it appears, and
// nothing else is.
func Values(text string, values []string) string {
	var forms []string
	for _, v := range values {
		if len(v) < MinValue {
			continue
		}
		for _, f := range Encodings(v) {
			if !slices.Contains(forms, f) {
				forms = append(forms, f)
			}
		}
	}
	sort.SliceStable(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
	for _, f := range forms {
		text = strings.ReplaceAll(text, f, "[redacted]")
	}
	return text
}

// Text returns s with secrets replaced by [redacted:<kind>] and the sorted
// kinds found.
func Text(s string) (string, []string) {
	found := map[string]bool{}
	for _, v := range localSecrets() {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, "[redacted:"+LocalKind+"]")
			found[LocalKind] = true
		}
	}
	for _, r := range rules {
		var b strings.Builder
		last, changed := 0, false
		for _, sub := range r.re.FindAllStringSubmatchIndex(s, -1) {
			start, end := sub[2*r.group], sub[2*r.group+1]
			if start < 0 {
				continue
			}
			if r.keep != nil {
				h := hit{value: s[start:end], rest: s[end:]}
				if r.group == 2 && sub[2] >= 0 {
					h.key, h.sep = s[sub[2]:sub[3]], s[sub[3]:start]
				}
				if r.keep(h) {
					continue
				}
			}
			found[r.kind], changed = true, true
			b.WriteString(s[last:start] + "[redacted:" + r.kind + "]")
			last = end
		}
		if changed {
			b.WriteString(s[last:])
			s = b.String()
		}
	}
	kinds := make([]string, 0, len(found))
	for k := range found {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return s, kinds
}
