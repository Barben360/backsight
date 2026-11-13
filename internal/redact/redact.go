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
	// find, when set in place of re, gives the spans to replace.
	find func(s string) [][]int
	// lit: the text holds one of these strings at least when the rule can
	// match, so a text holding none skips the rule (most texts hold no
	// vendor prefix).
	lit []string
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

// pemBegin and pemEnd are the armour lines of a PEM or PGP private key.
const (
	pemBegin = `-----BEGIN [A-Z ]*PRIVATE KEY(?: BLOCK)?-----`
	pemEnd   = `-----END [A-Z ]*PRIVATE KEY(?: BLOCK)?-----`
)

// vendorRules are credentials known by a prefix of their own, from
// gitleaks' default rules and the AI providers' formats (decision 0088):
// each prefix is distinctive enough that a match is a credential.
var vendorRules = []rule{
	{kind: "sendgrid_key", re: regexp.MustCompile(`\bSG\.[A-Za-z0-9_-]{16,32}\.[A-Za-z0-9_-]{16,64}`), lit: []string{"SG."}},
	{kind: "tailscale_key", re: regexp.MustCompile(`\btskey-(?:[a-z]+-)?[A-Za-z0-9]+-[A-Za-z0-9]{16,}`), lit: []string{"tskey-"}},
	{kind: "huggingface_token", re: regexp.MustCompile(`\b(?:hf|api_org)_[A-Za-z0-9]{30,}`), lit: []string{"hf_", "api_org_"}},
	{kind: "groq_key", re: regexp.MustCompile(`\bgsk_[A-Za-z0-9]{40,}`), lit: []string{"gsk_"}},
	{kind: "xai_key", re: regexp.MustCompile(`\bxai-[A-Za-z0-9]{60,}`), lit: []string{"xai-"}},
	{kind: "replicate_token", re: regexp.MustCompile(`\br8_[A-Za-z0-9]{30,}`), lit: []string{"r8_"}},
	{kind: "perplexity_key", re: regexp.MustCompile(`\bpplx-[A-Za-z0-9]{40,}`), lit: []string{"pplx-"}},
	{kind: "pypi_token", re: regexp.MustCompile(`\bpypi-AgE[A-Za-z0-9_-]{50,}`), lit: []string{"pypi-AgE"}},
	{kind: "vault_token", re: regexp.MustCompile(`\bhv[sbr]\.[A-Za-z0-9_-]{24,}`), lit: []string{"hvs.", "hvb.", "hvr."}},
	// Not after a word or '-' (cache-key-<md5>, npm-key-<hash>).
	{kind: "mailgun_key", find: bounded(regexp.MustCompile(`key-[0-9a-f]{32}`), inClass("_-"), nil), lit: []string{"key-"}},
	{kind: "doppler_token", re: regexp.MustCompile(`\bdp\.(?:pt|st|ct|sa|scim|audit)\.[A-Za-z0-9]{40,}`), lit: []string{"dp."}},
	{kind: "dynatrace_token", re: regexp.MustCompile(`\bdt0c01\.[A-Za-z0-9]{24}\.[A-Za-z0-9]{64}`), lit: []string{"dt0c01."}},
	{kind: "linear_key", re: regexp.MustCompile(`\blin_(?:api|oauth)_[A-Za-z0-9]{40}`), lit: []string{"lin_"}},
	{kind: "notion_token", re: regexp.MustCompile(`\bntn_[0-9]{11}[A-Za-z0-9]{35}`), lit: []string{"ntn_"}},
	{kind: "databricks_token", re: regexp.MustCompile(`\bdapi[a-f0-9]{32}(?:-\d)?\b`), lit: []string{"dapi"}},
	{kind: "pulumi_token", re: regexp.MustCompile(`\bpul-[a-f0-9]{40}\b`), lit: []string{"pul-"}},
	{kind: "postman_key", re: regexp.MustCompile(`\bPMAK-[a-f0-9]{24}-[a-f0-9]{34}\b`), lit: []string{"PMAK-"}},
	{kind: "rubygems_key", re: regexp.MustCompile(`\brubygems_[a-f0-9]{48}\b`), lit: []string{"rubygems_"}},
	{kind: "sentry_token", re: regexp.MustCompile(`\bsntry[us]_[A-Za-z0-9+/=_]{40,}`), lit: []string{"sntry"}},
	{kind: "planetscale_token", re: regexp.MustCompile(`\bpscale_(?:tkn|oauth|pw)_[A-Za-z0-9_.=-]{32,64}`), lit: []string{"pscale_"}},
	{kind: "prefect_key", re: regexp.MustCompile(`\bpnu_[A-Za-z0-9]{36}`), lit: []string{"pnu_"}},
	{kind: "grafana_token", re: regexp.MustCompile(`\b(?:glc_[A-Za-z0-9+/]{32,}={0,3}|glsa_[A-Za-z0-9]{32}_[A-Fa-f0-9]{8})`), lit: []string{"glc_", "glsa_"}},
	{kind: "heroku_key", re: regexp.MustCompile(`\bHRKU-[A-Za-z0-9_-]{30,}`), lit: []string{"HRKU-"}},
	{kind: "brevo_key", re: regexp.MustCompile(`\bx(?:keysib|smtpsib)-[a-f0-9]{64}-[A-Za-z0-9]{16}`), lit: []string{"sib-"}},
	{kind: "square_token", re: regexp.MustCompile(`\bsq0(?:atp|csp)-[A-Za-z0-9_-]{22,}`), lit: []string{"sq0"}},
	{kind: "artifactory_key", re: regexp.MustCompile(`\bAKCp[A-Za-z0-9]{69}\b`), lit: []string{"AKCp"}},
	{kind: "flyio_token", re: regexp.MustCompile(`\b(?:fo1_[A-Za-z0-9_-]{43}|fm1[ar]_[A-Za-z0-9+/]{100,}={0,3}|fm2_[A-Za-z0-9+/]{100,}={0,3})`), lit: []string{"fo1_", "fm1", "fm2_"}},
	{kind: "openshift_token", re: regexp.MustCompile(`\bsha256~[A-Za-z0-9_-]{43}`), lit: []string{"sha256~"}},
	{kind: "onepassword_token", re: regexp.MustCompile(`\b(?:ops_eyJ[A-Za-z0-9+/]{250,}={0,3}|A3-[A-Z0-9]{6}-(?:[A-Z0-9]{11}|[A-Z0-9]{6}-[A-Z0-9]{5})-[A-Z0-9]{5}-[A-Z0-9]{5}-[A-Z0-9]{5}\b)`), lit: []string{"ops_eyJ", "A3-"}},
	{kind: "newrelic_key", re: regexp.MustCompile(`\b(?:NRAK-[A-Z0-9]{27}|NRII-[A-Za-z0-9-]{32})\b`), lit: []string{"NRAK-", "NRII-"}},
	{kind: "alibaba_key", re: regexp.MustCompile(`\bLTAI[A-Za-z0-9]{20}\b`), lit: []string{"LTAI"}},
	{kind: "atlassian_token", re: regexp.MustCompile(`\bATATT3[A-Za-z0-9_=-]{100,}`), lit: []string{"ATATT3"}},
	{kind: "azure_client_secret", find: bounded(regexp.MustCompile(`[A-Za-z0-9_~.]{3}\dQ~[A-Za-z0-9_~.-]{31,34}`), inClass("_~.-"), nil), lit: []string{"Q~"}},
	{kind: "bedrock_key", re: regexp.MustCompile(`\bABSK[A-Za-z0-9+/]{100,}={0,2}`), lit: []string{"ABSK"}},
	{kind: "supabase_key", re: regexp.MustCompile(`\b(?:sbp_[a-f0-9]{40}\b|sb_secret_[A-Za-z0-9_-]{20,})`), lit: []string{"sbp_", "sb_secret_"}},
	{kind: "sourcegraph_token", re: regexp.MustCompile(`\bsgp_(?:(?:[a-fA-F0-9]{16}|local)_)?[a-fA-F0-9]{40}\b`), lit: []string{"sgp_"}},
	{kind: "cloudflare_key", re: regexp.MustCompile(`\bv1\.0-[a-f0-9]{24}-[a-f0-9]{146}`), lit: []string{"v1.0-"}},
	{kind: "typeform_token", re: regexp.MustCompile(`\btfp_[A-Za-z0-9_.=-]{59}`), lit: []string{"tfp_"}},
	{kind: "easypost_key", re: regexp.MustCompile(`\bEZ[AT]K[A-Za-z0-9]{54}\b`), lit: []string{"EZAK", "EZTK"}},
	{kind: "shippo_token", re: regexp.MustCompile(`\bshippo_(?:live|test)_[a-fA-F0-9]{40}\b`), lit: []string{"shippo_"}},
	{kind: "clojars_token", re: regexp.MustCompile(`\bCLOJARS_[A-Za-z0-9]{60}`), lit: []string{"CLOJARS_"}},
	{kind: "facebook_token", re: regexp.MustCompile(`\bEAA[MC][A-Za-z0-9]{100,}`), lit: []string{"EAAM", "EAAC"}},
}

var rules = slices.Concat([]rule{
	// A backsight connection token: it carries the API key. First, and with
	// no \b: glued to a word or after an escaped newline (\n) it must still
	// be masked whole, before a broader rule takes a part of it.
	{kind: "backsight_token", re: regexp.MustCompile(`bst1\.[A-Za-z0-9_-]{40,}`)},
	// PEM and PGP armour: the whole block to its END line; without it (cut
	// short), the key material after the header, fail-closed (pemBlocks).
	{kind: "private_key", find: pemBlocks},
	// An age identity: backsight's backup key (bech32, upper-case), whole
	// or cut short.
	{kind: "age_identity", re: regexp.MustCompile(`\bAGE-SECRET-KEY-1[0-9A-Z]{16,}`)},
	{kind: "anthropic_key", re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	{kind: "openai_key", re: regexp.MustCompile(`\bsk-(?:proj-|svcacct-)?[A-Za-z0-9_-]{32,}`)},
	{kind: "stripe_key", re: regexp.MustCompile(`\b(?:[sr]k_(?:live|test|prod)_[A-Za-z0-9]{10,}|whsec_[A-Za-z0-9]{32,})`)},
	{kind: "github_token", re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{22,})`)},
	{kind: "npm_token", re: regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}`)},
	{kind: "slack_token", re: regexp.MustCompile(`\b(?:xox[abeoprs]-[A-Za-z0-9-]{10,}|xapp-\d-[A-Za-z0-9-]{20,})`)},
	{kind: "aws_access_key", re: regexp.MustCompile(`\b(?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[0-9A-Z]{16}\b`)},
	{kind: "google_api_key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`)},
	{kind: "gitlab_token", re: regexp.MustCompile(`\b(?:gl(?:pat|ptt|dt|rt|cbt|imt|oas|ft|soat|ffct|agent)-[A-Za-z0-9_-]{20,}|GR1348941[A-Za-z0-9_-]{20,})`)},
	{kind: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
	{kind: "digitalocean_token", re: regexp.MustCompile(`\bdo[opr]_v1_[a-f0-9]{64}\b`)},
	{kind: "shopify_token", re: regexp.MustCompile(`\bshp(?:at|ca|pa|ss)_[a-fA-F0-9]{32}\b`)},
	// A Twilio API key SID: the key's name, but its only identifier.
	{kind: "twilio_key", re: regexp.MustCompile(`\bSK[0-9a-f]{32}\b`)},
	// A Telegram bot token, its bot id included (also after "bot" in an
	// API URL).
	{kind: "telegram_token", find: bounded(regexp.MustCompile(`[0-9]{5,16}:A[A-Za-z0-9_-]{32,36}`), isDigit, func(s string, i int) bool { return i == len(s) || !inClass("_-")(s[i]) }), keep: func(h hit) bool { _, v, _ := strings.Cut(h.value, ":"); return wordy(v) }},
	{kind: "teams_webhook", re: regexp.MustCompile(`\bhttps://[a-z0-9-]+\.webhook\.office\.com/webhookb2/([A-Za-z0-9@/_-]{40,})`), group: 1},
	{kind: "nuget_password", re: regexp.MustCompile(`(?i)<add\s+key="(?:ClearText)?Password"\s+value="([^"]{6,})"`), group: 1},
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
	vendorRules,
	// A value assigned to a secret-named key, each form for both key
	// shapes before the next form.
	interleave(keyRules(keyRE), keyRules(passKeyRE)),
	[]rule{
		// Long hex after the words naming a secret, in prose ("the token is
		// <hex>"); bare hex is a commit, a digest or a checksum and stays.
		{kind: "hex_secret", re: regexp.MustCompile(`(?i:\b(?:secret|token|passw(?:or)?d|passphrase|api[ _-]?key|private[ _-]key|signing[ _-]key|auth[ _-]token)s?)(?:\s+(?:is|was))?\s*[:=]?\s+["'\x60]?([0-9a-fA-F]{32,})\b`), group: 1},
		// A random value of a key ending in key, auth, sig or identity.
		{kind: "secret_value", re: regexp.MustCompile(`(?i)["']?\b(` + segmentKeyRE + `)["']?\s*[:=]\s*["']?` + segmentValueRE), group: 2, keep: keepSegmentValue},
		// A bare AWS secret access key: 40 random base64 characters holding
		// '/' or '+' (40 without them are a high-entropy token below),
		// between separators: not inside a path, a URL, a file name or an
		// identifier (decision 0088).
		{kind: "aws_secret_key", find: bounded(regexp.MustCompile(`[A-Za-z0-9/+]{40}`), inClass("/+=_.-"), awsEnd), keep: keepAWSSecret},
		// A long mixed-case alphanumeric token anywhere (hex, UUIDs, digests and
		// identifiers that rarely change character class are kept).
		{kind: "high_entropy", re: regexp.MustCompile(`\b[A-Za-z0-9_-]{32,}\b`), keep: func(h hit) bool { return !highEntropy(h.value) }},
	})

var (
	pemBeginRE = regexp.MustCompile(pemBegin)
	pemEndRE   = regexp.MustCompile(pemEnd)
	armourRE   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*:[ \t]`)
)

// pemBlocks is the spans of the private keys (decision 0088). A header
// reads up to the next header only, so the work stays linear however many
// there are: to its END line when one comes before the next header, the
// whole block; else to the end of the key material after it
// (pemMaterialEnd), and nothing when there is none (a header quoted in a
// command or named in prose stays, with the text after it).
func pemBlocks(s string) [][]int {
	begins, ends := pemBeginRE.FindAllStringIndex(s, -1), pemEndRE.FindAllStringIndex(s, -1)
	var out [][]int
	e := 0
	for k, b := range begins {
		limit := len(s)
		if k+1 < len(begins) {
			limit = begins[k+1][0]
		}
		for e < len(ends) && ends[e][0] < b[1] {
			e++
		}
		if e < len(ends) && ends[e][0] < limit {
			out = append(out, []int{b[0], ends[e][1]})
			continue
		}
		if end := pemMaterialEnd(s[:limit], b[1]); end > b[1] {
			out = append(out, []int{b[0], end})
		}
	}
	return out
}

// OpenKeyBefore is where the last private key header before i starts
// when no END line follows it before i (i falls inside its block), else
// -1: a text cut at i would leave that key's body without its header.
func OpenKeyBefore(s string, i int) int {
	locs := pemBeginRE.FindAllStringIndex(s[:min(len(s), i+len("-----BEGIN ")+64)], -1)
	for k := len(locs) - 1; k >= 0; k-- {
		if b := locs[k][0]; b < i {
			if pemEndRE.MatchString(s[b:i]) {
				return -1
			}
			return b
		}
	}
	return -1
}

// KeyEnd is where s's first private key END line ends, or -1.
func KeyEnd(s string) int {
	if loc := pemEndRE.FindStringIndex(s); loc != nil {
		return loc[1]
	}
	return -1
}

// pemMaterialEnd is where a key cut before its END line ends, read from i
// (the end of its header): fail-closed, every line up to the first line of
// prose (three words or more, each under 16 characters) or the end of s,
// once a line held key material (a base64 run of 16 characters or more);
// i when none did. Lines break at a newline, real or escaped (\n, \\n,
// \r\n); a quote, comment or diff prefix (>, #, //, *, |) and a trailing
// backslash or $ (cat -A) are no part of a line's words. Before the
// material, armour headers (Proc-Type:, Version:, Comment: …) are read
// as part of the block. A string closed right after base64 ("…==", in
// JSON or code) ends the key there. Blank lines are bridged.
func pemMaterialEnd(s string, i int) int {
	start, end, material := i, i, false
	for {
		j, next := pemLineEnd(s, i)
		if off, core := pemCore(s[i:j]); core != "" {
			b, p := base64Run(core)
			switch {
			case b > 0 && p < len(core) && strings.IndexByte("\"'`,", core[p]) >= 0 && (material || b >= 16):
				return i + off + p
			case !material && armourRE.MatchString(core):
				end = j
			case pemProse(core):
				if !material {
					return start
				}
				return end
			default:
				material = material || longRun(core, 16)
				end = j
			}
		}
		if next < 0 {
			if !material {
				return start
			}
			return end
		}
		i = next
	}
}

// pemCore is text without its blanks, its prefix tokens (quote, comment
// or diff marks, opening quotes) and a trailing backslash or $, and where
// that starts in text.
func pemCore(text string) (int, string) {
	off := 0
	for off < len(text) && strings.IndexByte(" \t>#/*|\"'`", text[off]) >= 0 {
		off++
	}
	core := strings.TrimRight(text[off:], " \t\r\\$")
	return off, core
}

// pemProse: three words or more, each under 16 characters (key material
// is one run per line, or chunks of 64 when flattened).
func pemProse(core string) bool {
	f := strings.Fields(core)
	if len(f) < 3 {
		return false
	}
	for _, w := range f {
		if len(w) >= 16 {
			return false
		}
	}
	return true
}

// base64Run is the length of the base64 run starting core, and where its
// padding ends.
func base64Run(core string) (int, int) {
	b := 0
	for b < len(core) && isBase64(core[b]) {
		b++
	}
	p := b
	for p < len(core) && core[p] == '=' {
		p++
	}
	return b, p
}

// longRun: core holds a base64 run of n characters or more.
func longRun(core string, n int) bool {
	run := 0
	for k := 0; k < len(core); k++ {
		if !isBase64(core[k]) {
			run = 0
			continue
		}
		if run++; run >= n {
			return true
		}
	}
	return false
}

// pemLineEnd is the end of the line starting at i, before its break, and
// where the next one starts (-1 at the end of s). A break is a newline,
// or n or r-then-n after one backslash or more (JSON-escaped once or
// twice).
func pemLineEnd(s string, i int) (int, int) {
	for k := i; k < len(s); k++ {
		if s[k] == '\n' {
			if k > i && s[k-1] == '\r' {
				return k - 1, k + 1
			}
			return k, k + 1
		}
		if s[k] != '\\' {
			continue
		}
		m := k
		for m < len(s) && s[m] == '\\' {
			m++
		}
		switch {
		case m < len(s) && s[m] == 'n':
			return k, m + 1
		case m < len(s) && s[m] == 'r':
			n := m + 1
			for n < len(s) && s[n] == '\\' {
				n++
			}
			if n > m+1 && n < len(s) && s[n] == 'n' {
				return k, n + 1
			}
		}
		k = m - 1
	}
	return len(s), -1
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isBase64(c byte) bool { return isAlnum(c) || c == '+' || c == '/' }

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// bounded is re's matches with no byte of class just before them nor
// just after (after, when set, decides the byte after instead). Checking
// a neighbour consumes nothing, so two matches one separator apart are
// both found.
func bounded(re *regexp.Regexp, class func(byte) bool, after func(s string, i int) bool) func(string) [][]int {
	if after == nil {
		after = func(s string, i int) bool { return i == len(s) || !class(s[i]) }
	}
	return func(s string) [][]int {
		var out [][]int
		for _, m := range re.FindAllStringIndex(s, -1) {
			if (m[0] == 0 || !class(s[m[0]-1])) && after(s, m[1]) {
				out = append(out, m)
			}
		}
		return out
	}
}

// inClass is a byte test for the characters of chars plus ASCII letters
// and digits.
func inClass(chars string) func(byte) bool {
	return func(c byte) bool { return isAlnum(c) || strings.IndexByte(chars, c) >= 0 }
}

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

// keepAWSSecret decides that a bare 40-character base64 value is not an
// AWS secret key: it holds no '/' or '+' (a high-entropy token then), it
// is hex (a commit), or it is not random: upper and lower case, a digit
// and frequent changes of character class, its '/'-separated parts not
// words (a path).
func keepAWSSecret(h hit) bool {
	v := h.value
	if !strings.ContainsAny(v, "/+") || wordy(v) {
		return true
	}
	var upper, lower, digit, changes int
	prev := 0
	for _, r := range v {
		c := 0
		switch {
		case r >= 'A' && r <= 'Z':
			upper, c = upper+1, 1
		case r >= 'a' && r <= 'z':
			lower, c = lower+1, 2
		case r >= '0' && r <= '9':
			digit, c = digit+1, 3
		}
		if c == 0 {
			continue // separators split words, they are not randomness
		}
		if prev != 0 && c != prev {
			changes++
		}
		prev = c
	}
	return upper < 2 || lower < 2 || digit < 1 || float64(changes)/float64(len(v)) < 0.3
}

// awsEnd: no base64, '=', '_', '-' or '.' right after a bare AWS secret
// key, but a '.' ending a sentence (before a blank or the end).
func awsEnd(s string, i int) bool {
	if i == len(s) {
		return true
	}
	if s[i] == '.' {
		return i+1 == len(s) || s[i+1] == ' ' || s[i+1] == '\t' || s[i+1] == '\n' || s[i+1] == '\r'
	}
	return !inClass("/+=_.-")(s[i])
}

// wordy: every '/'-separated part of v is made of words, as identifiers
// and paths are (CamelCaseIdentifier2Version3, newClientWithRetry2,
// src/Header/MenuItem2): lower-case and capitalised words holding a
// vowel, and runs of one or two digits. A random token almost never is:
// each capital is followed by a lower-case letter, each run of lower-case
// letters holds a vowel, and no three digits follow each other. Only the
// new bounded rules use it (aws_secret_key, telegram_token): high_entropy
// does not, as a passphrase made of words would then leak (decision 0088
// D5).
func wordy(v string) bool {
	for _, part := range strings.Split(v, "/") {
		if !wordRunRE.MatchString(part) {
			return false
		}
		for _, w := range wordRE.FindAllString(part, -1) {
			if !strings.ContainsAny(strings.ToLower(w), "aeiouy") {
				return false
			}
		}
	}
	return true
}

var (
	wordRunRE = regexp.MustCompile(`^(?:[A-Z]?[a-z]+|[0-9]{1,2})+$`)
	wordRE    = regexp.MustCompile(`[A-Z]?[a-z]+`)
)

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

// minValue is the shortest value Values replaces.
const minValue = 6

// EnvSecretKey names the environment-file entries that hold a secret even
// unquoted (the server's .env and server.env).
var EnvSecretKey = regexp.MustCompile(`(?i)(key|pass|secret|token)`)

// Forms are what Values replaces for values: each one of minValue bytes or
// more and its URL-escaped forms (Encodings), longest first.
func Forms(values []string) []string {
	var forms []string
	for _, v := range values {
		if len(v) < minValue {
			continue
		}
		for _, f := range Encodings(v) {
			if !slices.Contains(forms, f) {
				forms = append(forms, f)
			}
		}
	}
	sort.SliceStable(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
	return forms
}

// Values is text with every one of Forms(values) replaced by [redacted]:
// the runtime's redaction of the known secrets, for error messages quoting
// a tool and for the native server's logs (a database URL carries the
// password escaped). Unlike Text, it applies no rule: a value is replaced
// wherever it appears, and nothing else is.
func Values(text string, values []string) string {
	for _, f := range Forms(values) {
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
		if r.lit != nil && !slices.ContainsFunc(r.lit, func(l string) bool { return strings.Contains(s, l) }) {
			continue
		}
		var b strings.Builder
		last, changed := 0, false
		var subs [][]int
		if r.find != nil {
			subs = r.find(s)
		} else {
			subs = r.re.FindAllStringSubmatchIndex(s, -1)
		}
		for _, sub := range subs {
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
