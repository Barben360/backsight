package redact

import (
	"encoding/base64"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/token"
)

// Secrets are assembled at runtime: the file itself holds no scannable token.
func rep(s string, n int) string { return strings.Repeat(s, n) }

var (
	anthropic = "sk-" + "ant-api03-" + rep("Ab3dE5gH9k", 5)
	openai    = "sk-" + "proj-" + rep("Q1w2E3r4T5", 4)
	github    = "gh" + "p_" + rep("a1B2c3D4e5F6", 3)
	githubPAT = "github_" + "pat_" + rep("11ABCDEFG0", 3)
	slack     = "xo" + "xb-" + "123456789012-" + rep("aB3dE", 4)
	aws       = "AK" + "IA" + "Q3EGRA7HZ6FK4M2X"
	google    = "AI" + "za" + rep("Sy9xK2m4P7", 3) + "abcde"
	jwt       = "ey" + "JhbGciOiJIUzI1NiJ9" + ".ey" + "JzdWIiOiIxMjM0NTY3ODkwIn0" + "." + rep("dBjftJeZ4C", 3)
	bstToken  = "bst" + "1." + rep("eyJ1cmwiOiJo", 5)
	privKey   = "-----BEGIN " + "OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\nQyNTUxOQAAACD\n-----END " + "OPENSSH PRIVATE KEY-----"

	// Security review H1 and M1 shapes.
	ageKey    = "AGE-" + "SECRET-KEY-1" + rep("QZ7X9KMPL2", 5) + "W3RTY4UV"
	ageRecip  = "age1" + rep("qz7x9kmpl2", 5) + "w3rty4uv7a9s"
	hex64     = rep("3f9a1c7e5b2d4f60", 4)
	hex32     = rep("9e8d7c6b5a4f3e2d", 2)
	b64       = "Kq9/z" + "Lm+3XwP7rT0vN8yB2" + "cD/4eF6gH1jK5lM9nQ" + "+sU7wY0aZ3bE6iO/K1SZFPTOtr/KBHBeksoGMGw=="
	sasSig    = "Kq9" + "z%2BLm3XwP7rT0vN8yB2cD%2F4eF6gH1jK5lM9nQ%3D"
	pgpBlock  = "-----BEGIN " + "PGP PRIVATE KEY BLOCK-----\n\nlQOYBGXk8bQBCADn\n/K1SZFPTOtr/KBHBeks\n=Qx7a\n-----END " + "PGP PRIVATE KEY BLOCK-----"
	totpSeed  = "JBSW" + "Y3DPEHPK3PXP" + "JBSWY3DPEHPK3PXP"
	dockerB64 = base64.StdEncoding.EncodeToString([]byte("bob:" + "hunter2hunter2"))
)

func TestTruePositives(t *testing.T) {
	cases := []struct {
		name, in, kind, gone string
	}{
		{"anthropic", "export ANTHROPIC=" + anthropic + " now", "anthropic_key", anthropic},
		{"backsight token", "here: " + bstToken, "backsight_token", bstToken[5:]},
		{"openai", "the key is " + openai, "openai_key", openai},
		{"github", "token " + github + " expired", "github_token", github},
		{"github pat", githubPAT, "github_token", githubPAT},
		{"slack", "bot " + slack, "slack_token", slack},
		{"aws", "id " + aws + " in the logs", "aws_access_key", aws},
		{"google", "maps " + google, "google_api_key", google},
		{"jwt", "cookie=" + jwt, "jwt", jwt},
		{"private key", "here:\n" + privKey + "\nend", "private_key", "b3BlbnNzaC1rZXktdjEAAAAA"},
		{"bearer", "curl -H 'Authorization: Bearer abcDEF123456ghiJKL789' http://x", "bearer", "abcDEF123456ghiJKL789"},
		{"url password", "postgresql://hindsight:s3cretPassw0rd@db:5432/hindsight", "url_password", "s3cretPassw0rd"},
		{"env assignment", "HINDSIGHT_API_LLM_API_KEY=fakevalueXYZ123456", "secret_value", "fakevalueXYZ123456"},
		{"json token", `{"apiUrl":"http://x","apiToken":"q8Zr4Lm2Vn7Tw1Ks"}`, "secret_value", "q8Zr4Lm2Vn7Tw1Ks"},
		{"yaml password", "db:\n  password: hunter2hunter2\n", "secret_value", "hunter2hunter2"},
		{"long letters-only secret", "API_KEY=abcdefghijklmnopqrstuvwxyzABCD", "secret_value", "abcdefghijklmnopqrstuvwxyzABCD"},
		{"suffixed key", "export SECRET_KEY=django-insecure-q8zr4lm2", "secret_value", "django-insecure-q8zr4lm2"},
		{"key with suffix", "GITHUB_TOKEN_PROD=abcdefgh1234", "secret_value", "abcdefgh1234"},
		{"letters-only password", "password=correcthorsebattery", "secret_value", "correcthorsebattery"},
		{"short password", "DB_PASSWORD=hunter2", "secret_value", "hunter2"},
		{"quoted with spaces", `{"password": "my long pass phrase"}`, "secret_value", "my long pass phrase"},
		{"basic auth", "Authorization: Basic " + "dXNlcjpwYXNzd29yZDEyMw==", "basic_auth", "dXNlcjpwYXNzd29yZDEyMw=="},
		{"curl -u", "curl -u admin:s3cretpass https://x", "cli_password", "s3cretpass"},
		{"--password", "mytool --password Sup3rS3cret login", "cli_password", "Sup3rS3cret"},
		{"mysql -p", "mysql -u root -pSuperSecret1 billing", "cli_password", "SuperSecret1"},
		{"stripe", "sk_" + "live_" + rep("4eC39HqLyjWDarjtT1zdp7dc", 1), "stripe_key", "4eC39HqLyjWDarjtT1zdp7dc"},
		{"npm", "npm_" + rep("a1B2c3D4e5F6", 3), "npm_token", "a1B2c3D4e5F6a1B2"},
		{"url password with slash", "postgres://u:pa/ss@h/db", "url_password", "pa/ss"},
		{"high entropy", "token is " + rep("aB3dE5gH9kZx", 3) + " ok", "high_entropy", "aB3dE5gH9kZx"},
		// M1 review probes
		{"bare PASS key", "export DB_PASS=Zx9kLmQ2wP", "secret_value", "Zx9kLmQ2wP"},
		{"hyphenated api key header", `curl -H "X-API-Key: 7fA9qL2mZx8Pw3Rt" https://x`, "secret_value", "7fA9qL2mZx8Pw3Rt"},
		{"escaped json", `{\"api_key\": \"q8Fh2LmZp0Xr7Tn\"}`, "secret_value", "q8Fh2LmZp0Xr7Tn"},
		{"authorization token", "Authorization: Token " + rep("0123456789abcdef", 2), "auth_header", "0123456789abcdef0123"},
		{"gitlab", "remote https://oauth2:x@h " + "gl" + "pat-" + rep("xY7kQ2mZ9p", 2), "gitlab_token", "xY7kQ2mZ9pxY7kQ2mZ9p"},
		// M5 review rules (§13 redaction)
		{"pw key", "pw=hunter2xyz", "secret_value", "hunter2xyz"},
		{"pw key with colon", "PW: hunter2xyz", "secret_value", "hunter2xyz"},
		{"db_pw key", "export db_pw=Zx9kLmQ2", "secret_value", "Zx9kLmQ2"},
		{"pw suffix segment", "MYSQL_ROOT_PW: hunter2xyz", "secret_value", "hunter2xyz"},
		{"docker -e PW=", "docker run -e PW=hunter2xyz app", "secret_value", "hunter2xyz"},
		{"quoted pw with spaces", `PW="hunter two xyz"`, "secret_value", "hunter two xyz"},
		{"url password empty user", "redis://:hunter2xyz@10.0.0.7:6379/0", "url_password", "hunter2xyz"},
		{"http url password empty user", "http://:s3cr3tpw@10.1.2.3:8888", "url_password", "s3cr3tpw"},
		{"--passwd=", "tool --passwd=hunter2xyz", "cli_password", "hunter2xyz"},
		{"--passwd space", "tool --passwd hunter2xyz", "cli_password", "hunter2xyz"},
		{"--pass=", "tool --pass=hunter2xyz", "cli_password", "hunter2xyz"},
		{"--pass space", "tool --pass hunter2xyz -v", "cli_password", "hunter2xyz"},
		{"--passphrase quoted", `gpg --batch --passphrase "correct horse" -d x.gpg`, "cli_password", "correct horse"},
		{"--passphrase=", "gpg --passphrase=correct-horse -d x.gpg", "cli_password", "correct-horse"},
		{"--pw space", "app --pw hunter2xyz", "cli_password", "hunter2xyz"},
		{"--pw=", "app --pw=hunter2xyz", "cli_password", "hunter2xyz"},
		{"--secret space", "vault --secret s3cr3tvalue", "cli_password", "s3cr3tvalue"},
		{"--secret=", "vault --secret=s3cr3tvalue", "cli_password", "s3cr3tvalue"},
		{"--token space", "gh auth --token abcdefgh", "cli_password", "abcdefgh"},
		{"--token=", "cli --token=abcdefgh", "cli_password", "abcdefgh"},
		{"sshpass -p", "sshpass -p hunter2xyz ssh bob@nas", "cli_password", "hunter2xyz"},
		{"sshpass -p glued", "sshpass -phunter2xyz ssh bob@nas", "cli_password", "hunter2xyz"},
		{"redis-cli -a", "redis-cli -h 10.0.0.7 -a hunter2xyz ping", "cli_password", "hunter2xyz"},
		{"smbclient -U user%pass", "smbclient //nas/share -U bob%hunter2xyz", "cli_password", "hunter2xyz"},
		{"--user=user%pass", "rpcclient --user=bob%hunter2xyz nas", "cli_password", "hunter2xyz"},
		{"passphrase key", "passphrase=correct-horse-battery", "secret_value", "correct-horse-battery"},
		{"passphrase short letters", "PASSPHRASE: hunterxyz", "secret_value", "hunterxyz"},
		{"api key with a space", "API key: q8Zr4Lm2Vn7T", "secret_value", "q8Zr4Lm2Vn7T"},
		{"api key with spaced equals", "api key = q8Zr4Lm2Vn7T", "secret_value", "q8Zr4Lm2Vn7T"},
		{"short letters after token", "token: abcdefgh", "secret_value", "abcdefgh"},
		{"short letters after password", "password: abcdefgh", "secret_value", "abcdefgh"},
		{"short letters after pass", "pass=hunter", "secret_value", "hunter"},
		{"short letters after secret", "secret: abcdefgh", "secret_value", "abcdefgh"},
		{"capitalised word after password", "PASSWORD=Summer", "secret_value", "Summer"},
		{"short letters after client_secret", "client_secret=hunterxyz", "secret_value", "hunterxyz"},
		{"short letters in yaml", "db:\n  password: hunterxyz\n  user: app\n", "secret_value", "hunterxyz"},
		{"short letters quoted in json", `{"password": "hunterxyz", "user": "app"}`, "secret_value", "hunterxyz"},
		{"short letters after --password", "mytool --password hunterxyz login", "cli_password", "hunterxyz"},
		{"short basic auth", "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("bob:pw")), "basic_auth", base64.StdEncoding.EncodeToString([]byte("bob:pw"))},
		{"short padded basic auth", "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("a:bc")), "basic_auth", base64.StdEncoding.EncodeToString([]byte("a:bc"))},
		// Security review H1: backsight's own backup key (an age identity).
		{"age identity bare", "here is my key " + ageKey + " from the vault", "age_identity", ageKey},
		{"age identity file", "# created: 2026-10-25T10:00:00Z\n# public key: " + ageRecip + "\n" + ageKey + "\n", "age_identity", ageKey},
		{"age identity in secrets.toml", `backup_identity = "` + ageKey + `"`, "age_identity", ageKey},
		{"age identity piped", "echo " + ageKey + " | backsight backup identity import", "age_identity", ageKey},
		{"age identity cut short", "AGE-" + "SECRET-KEY-1" + rep("QZ7X9KMPL2", 3), "age_identity", "QZ7X9KMPL2QZ7X9KMPL2"},
		{"identity key random value", "SIGNING_IDENTITY=" + rep("q8Zr4Lm2Vn7T", 3), "secret_value", "q8Zr4Lm2Vn7T"},
		// Security review M1.
		{"netrc one line", "machine github.com login bob password s3cr3tP4ssw0rd", "netrc_password", "s3cr3tP4ssw0rd"},
		{"netrc lines", "machine api.heroku.com\n  login bob@example.com\n  password hunter2xyz\n", "netrc_password", "hunter2xyz"},
		{"netrc no login", "machine nas.example.org password hunter2xyz", "netrc_password", "hunter2xyz"},
		{"netrc default", "default login anonymous password hunter2xyz", "netrc_password", "hunter2xyz"},
		{"docker config auth", `{"auths":{"ghcr.io":{"auth":"` + dockerB64 + `"}}}`, "docker_auth", dockerB64},
		{"docker config auth spaced", `"auth": "` + dockerB64 + `"`, "docker_auth", dockerB64},
		{"docker login -p", "docker login -u bob -p hunter2hunter2 ghcr.io", "cli_password", "hunter2hunter2"},
		{"docker login -p=", "docker login -u bob -p=hunter2hunter2", "cli_password", "hunter2hunter2"},
		{"podman login -p", "podman login quay.io -u bob -p 'hunter2 two'", "cli_password", "hunter2 two"},
		{"docker login --password", "docker login --username bob --password hunter2hunter2", "cli_password", "hunter2hunter2"},
		{"slack webhook", "curl -X POST https://hooks.slack.com/services/" + "T024BE7LD/B01ABCDEF/" + rep("aB3dE5gH9k", 2) + "xyzw -d @x", "slack_webhook", rep("aB3dE5gH9k", 2)},
		{"discord webhook", "https://discord.com/api/webhooks/" + "112233445566778899/" + rep("Zx9-kLmQ2_wP", 5), "discord_webhook", "Zx9-kLmQ2_wP"},
		{"discordapp webhook", "https://discordapp.com/api/webhooks/" + "112233445566778899/" + rep("Zx9kLmQ2wP", 6), "discord_webhook", "Zx9kLmQ2wP"},
		{"digitalocean", "DO token dop" + "_v1_" + hex64, "digitalocean_token", hex64},
		{"shopify access", "shp" + "at_" + hex32, "shopify_token", hex32},
		{"shopify shared secret", "shp" + "ss_" + hex32, "shopify_token", hex32},
		{"twilio api key sid", "key SK" + hex32 + " for the app", "twilio_key", hex32},
		{"twilio auth", "TWILIO_AUTH=" + hex32, "secret_value", hex32},
		{"twilio auth token", "TWILIO_AUTH_TOKEN=" + hex32, "secret_value", hex32},
		{"hex secret key", "ENCRYPTION_KEY=" + hex64, "secret_value", hex64},
		{"hex secret in yaml", "master_key: " + hex64 + "\n", "secret_value", hex64},
		{"hex after token word", "the token is " + hex64, "hex_secret", hex64},
		{"hex after api key words", "my API key: " + hex32, "secret_value", hex32},
		{"hex after secret word", "client secret " + hex64 + " rotated", "hex_secret", hex64},
		{"totp otpauth", "otpauth://totp/ACME:bob?secret=" + totpSeed + "&issuer=ACME", "totp_seed", totpSeed},
		{"totp otpauth secret not first", "otpauth://totp/ACME:bob?issuer=ACME&secret=" + totpSeed, "totp_seed", totpSeed},
		{"totp prose", "TOTP secret " + totpSeed, "totp_seed", totpSeed},
		{"2fa seed", "2FA seed: " + totpSeed, "totp_seed", totpSeed},
		{"secret= base32", "secret=" + totpSeed, "secret_value", totpSeed},
		{"pgp private key", "gpg --export-secret-keys -a:\n" + pgpBlock + "\n", "private_key", "/K1SZFPTOtr/KBHBeks"},
		{"pgp private key cut", "-----BEGIN " + "PGP PRIVATE KEY BLOCK-----\nlQOYBGXk8bQBCADn\n/K1SZFPTOtr/KBHBeks", "private_key", "/K1SZFPTOtr/KBHBeks"},
		{"azure account key", "DefaultEndpointsProtocol=https;AccountName=acme;AccountKey=" + b64 + ";EndpointSuffix=core.windows.net", "secret_value", "/K1SZFPTOtr/KBHBeksoGMGw=="},
		{"azure shared access key", "Endpoint=sb://acme.servicebus.windows.net/;SharedAccessKeyName=root;SharedAccessKey=" + b64, "secret_value", "/K1SZFPTOtr/KBHBeksoGMGw=="},
		{"azure sas sig", "https://acme.blob.core.windows.net/c/x?sv=2022-11-02&sp=r&se=2026-11-01&sig=" + sasSig + "&spr=https", "secret_value", "eF6gH1jK5lM9nQ%3D"},
		{"kubeconfig client key", "    client-key-data: " + "LS0tLS1CRUdJTiBSU0Eg" + b64, "secret_value", "/K1SZFPTOtr/KBHBeksoGMGw=="},
		{"aws signature", "X-Amz-Signature=" + hex64, "secret_value", hex64},
	}
	for _, c := range cases {
		out, kinds := Text(c.in)
		if strings.Contains(out, c.gone) {
			t.Errorf("%s: secret survived: %q", c.name, out)
		}
		if !strings.Contains(out, "[redacted:"+c.kind+"]") || !contains(kinds, c.kind) {
			t.Errorf("%s: want kind %s, got %q %v", c.name, c.kind, out, kinds)
		}
	}
}

func TestContextAroundSecretsSurvives(t *testing.T) {
	out, _ := Text("curl -H 'Authorization: Bearer abcDEF123456ghiJKL789' http://api.example/v1")
	if !strings.Contains(out, "Authorization: Bearer [redacted:bearer]") || !strings.Contains(out, "http://api.example/v1") {
		t.Errorf("%q", out)
	}
	out, _ = Text("postgresql://hindsight:s3cretPassw0rd@db:5432/hindsight")
	if out != "postgresql://hindsight:[redacted:url_password]@db:5432/hindsight" {
		t.Errorf("%q", out)
	}
	out, _ = Text("HINDSIGHT_API_LLM_API_KEY=fakevalueXYZ123456 # deepseek")
	if out != "HINDSIGHT_API_LLM_API_KEY=[redacted:secret_value] # deepseek" {
		t.Errorf("%q", out)
	}
}

// Look-alikes that must not be touched: redaction must not eat normal text.
func TestLookAlikesSurvive(t *testing.T) {
	for _, s := range []string{
		"We use sk-learn and scikit-learn for the model.",
		"task-ant-colony optimisation",
		"The token bucket refills every second.",
		"max_tokens: 50000",
		"input_tokens=3101 output_tokens=354",
		"TOKEN=${GITHUB_TOKEN}",
		"api_key = <your key here>",
		"password: \"$DB_PASSWORD\"",
		"Set the password reset flow behind a feature flag.",
		"commit 5d1b0c7e9f2a4b6c8d0e1f2a3b4c5d6e7f8a9b0c fixed it",
		"https://github.com/Barben360/backsight/pull/54",
		"postgresql://db:5432/hindsight",
		"AKIA is the prefix of AWS access keys",
		"eyJhbGciOi is how a JWT header starts",
		"Bearer tokens go in the Authorization header.",
		"secrets.toml must be 0600",
		"apiToken: [redacted:secret_value]",
		"docker run -p 8080:80 nginx",
		"sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11",
		"session 88cf850f-c82e-4b79-b1d7-ecc3af0e50a9 resumed",
		"see TestServerInstallComposeFailure and TestBankApplyRecreatesAWipedBank",
		"github.com/Barben360/backsight/internal/hindsight",
		"flatpak override --user --filesystem=xdg-config/gtk-4.0:ro",
		"const expediteurPubliqueFingerprintDeclare = 1",
		"/tmp/claude-1000/-home-annuser-Workspace-github-com-Barben360-atlas-spec/x",
		`"predicted_per_token_ms":41.23456789`,
		"The signing keys have no password: `pass` and GitHub secrets encrypt them.",
		"Crée un token : Profil → Sécurité",
		"Crée un token : `https://ha.example.org/profile/security` → « Jetons »",
		"Downloads/Contrat_CDI_-_Jean_DUPONT_au_01.07.2026.pdf",
		"sed 's/password:.*/password: ***/' netplan.yaml",
		"Crée un token : Paramètres → Sécurité",
		"bypass_mode=enabled and compass=north",
		"passport_number = 12",
		"use the --pass-through flag",
		"Authorization: header missing",
		"PASS=1",
		// M5 review rules: prose around the new forms
		"the pw field is required",
		"PW=1",
		"pw: n/a",
		"api key rotation is planned",
		"the api key: see below",
		"API key: <your key here>",
		"--pass-through and --passive stay",
		"gh auth --token-file ~/.config/gh/x and --secret-name prod",
		"vault --secret $VAULT_SECRET",
		"redis-cli -h 10.0.0.7 ping",
		"smbclient -U bob //nas/share",
		"sshpass -f ~/.sshpw ssh bob@nas",
		"Basic settings live in config.toml",
		"basic flow and basic auth",
		"token_type: bearer",
		"password_auth: enabled",
		"passphrase-protected keys are fine",
		// Short plain words that are prose, code or usage, not a secret
		// (seen in real transcripts).
		"secrets.toml has no db_password: restore it from the backup",
		"interface Login { token: string; password: string }",
		"--- PASS: TestFoo (0.00s)",
		"in.Token = cfgTok",
		"Second pass: thoughts on the design",
		"A secret: printed once, then gone.",
		"connect(user=user, password=password)",
		`{"password": "string", "token": "secret"}`,
		"mytool --password PASSWORD --token TOKEN",
		"set it with --token values from the vault",
		"DEBUG=pw:browser npx playwright test",
		// Security review H1/M1 rules: what they must leave alone.
		"commit " + hex64[:40] + " fixed it",
		"git checkout " + hex64[:40],
		"image ghcr.io/vectorize-io/hindsight@sha256:" + hex64,
		"sha256 " + hex64 + "  backsight_linux_amd64.tar.gz",
		hex64 + "  checksums.txt",
		"digest: sha256:" + hex64,
		`"sha256": "` + hex64 + `"`,
		"checksum=" + hex64,
		"cache_key=" + hex64,
		"session 88cf850f-c82e-4b79-b1d7-ecc3af0e50a9 resumed",
		"request_key=88cf850f-c82e-4b79-b1d7-ecc3af0e50a9",
		"md5 " + hex32 + " of the file",
		"# public key: " + ageRecip,
		"recipient = \"" + ageRecip + "\"",
		"AGE-SECRET-KEY-1 is how an age identity starts",
		"identity_file = ~/.ssh/id_ed25519",
		"IdentityFile ~/.ssh/id_ed25519",
		"identity_provider=okta-prod",
		"auth: true",
		"auth=oauth2",
		`"auth": "basic"`,
		"primary_key=user_id",
		"public_key = " + ageRecip,
		"monkey=banana",
		"sig=v1",
		"the machine learning password field",
		"machine github.com login bob password $GH_PASSWORD",
		"docker login -u bob --password-stdin ghcr.io",
		"docker run -p 8080:80 nginx and docker login ghcr.io",
		"https://hooks.slack.com/services/",
		"https://discord.com/api/webhooks",
		"the totp secret is in 1Password",
		"TOTP secret: see the vault",
		"-----BEGIN PGP PUBLIC KEY BLOCK-----\nmQENBGXk8bQBCADn\n-----END PGP PUBLIC KEY BLOCK-----",
		"-----BEGIN PGP SIGNATURE-----",
		"AccountName=acme;EndpointSuffix=core.windows.net",
		"shpat_ is the prefix of Shopify admin tokens",
		"SKU 12345 and SKIP_TESTS=1",
		"the token is valid for one hour",
		// Seen in real transcripts: identifiers after a key word.
		"key: cleColonneEnTeteJour(i),",
		"key: cleInterrupteurConsentementSentry,",
		"Current model identity: claude-cli/claude-sonnet-4-6.",
		"netrc `machine … login … password X` and `docker login -p X`",
		"netrc `machine … login … password …`",
	} {
		out, kinds := Text(s)
		if out != s || len(kinds) != 0 {
			t.Errorf("look-alike changed: %q -> %q %v", s, out, kinds)
		}
	}
}

func TestKindsAreUniqueAndSorted(t *testing.T) {
	_, kinds := Text(github + " and " + github + " and " + aws + " " + bstToken)
	if strings.Join(kinds, ",") != "aws_access_key,backsight_token,github_token" {
		t.Errorf("%v", kinds)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// The exact values of the secret stores go whatever their shape, in their
// URL-escaped forms too, read once.
func TestLocalSecrets(t *testing.T) {
	plain := "correct" + "horsebatterystaple" // no rule knows this shape
	odd := "pa:ss/" + "word with spaces"      // escaped differently in URLs and paths
	short := "abc" + "1234"                   // under MinSecret: ordinary text
	calls := 0
	SetLocalSecrets(func() []string { calls++; return []string{plain, odd, short, ""} })
	t.Cleanup(func() { SetLocalSecrets(nil) })
	for _, in := range []string{
		"the llm key is " + plain + " now",
		"see https://x/?k=" + url.QueryEscape(odd) + "&a=1",
		"GET /banks/" + url.PathEscape(odd) + "/memories",
		"GET /banks/" + strings.ReplaceAll(url.PathEscape(odd), ":", "%3A") + "/memories",
		"quoted: '" + odd + "'",
	} {
		out, kinds := Text(in)
		if strings.Contains(out, plain) || strings.Contains(out, "word") || !strings.Contains(out, "[redacted:local_secret]") || !contains(kinds, LocalKind) {
			t.Errorf("%q -> %q %v", in, out, kinds)
		}
	}
	if out, kinds := Text("build abc1234 passed"); out != "build abc1234 passed" || len(kinds) != 0 {
		t.Errorf("a value under MinSecret was redacted: %q %v", out, kinds)
	}
	if calls != 1 {
		t.Errorf("the secret stores were read %d times, want once", calls)
	}
	SetLocalSecrets(nil)
	if out, _ := Text("the llm key is " + plain); !strings.Contains(out, plain) {
		t.Errorf("a cleared source still redacts: %q", out)
	}
}

// A connection token carries the API key: it is masked wherever it sits,
// glued to a word or after an escaped newline in a JSON string included.
func TestRedactMasksAnEncodedToken(t *testing.T) {
	sec := config.Secrets{}
	config.GenerateMissing(&sec)
	tok := token.Encode(token.Token{URL: "http://100.101.1.2:8888", Bank: "backsight", APIKey: sec.APIKey, ServerID: "0b6f1c1e-5b0e-4f4e-9d0c-2b8f3c5a7e11"})
	for _, in := range []string{"here: " + tok, "x" + tok, `\n` + tok, `{"t":"` + tok + `"}`} {
		out, kinds := Text(in)
		if strings.Contains(out, tok[len(token.Prefix):]) || strings.Contains(out, sec.APIKey) {
			t.Errorf("%q left the token: %q", in, out)
		}
		if !slices.Contains(kinds, "backsight_token") {
			t.Errorf("%q: kinds %v", in, kinds)
		}
	}
}

// Values replaces each known value, escaped as a URL carries it too, and
// leaves a value shorter than minValue and everything else alone.
func TestValuesReplacesKnownSecretsAndTheirURLForms(t *testing.T) {
	pw := "p@ss/w:rd 9x"
	text := "pw " + pw + " url postgresql://hindsight:" + url.UserPassword("hindsight", pw).String()[len("hindsight:"):] + "@127.0.0.1:5432/hindsight key sk-long-key short abc"
	got := Values(text, []string{pw, "sk-long-key", "abc", ""})
	for _, leak := range []string{pw, url.QueryEscape(pw), url.PathEscape(pw), "sk-long-key"} {
		if strings.Contains(got, leak) {
			t.Errorf("%q left in %q", leak, got)
		}
	}
	if want := "pw [redacted] url postgresql://hindsight:[redacted]@127.0.0.1:5432/hindsight key [redacted] short abc"; got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}
