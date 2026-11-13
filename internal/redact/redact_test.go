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

// Decision 0088: the prefixed formats of gitleaks' default rules and of
// the AI providers, the bare AWS secret key and the whole Telegram token.
var (
	sendgrid  = "SG" + "." + rep("aB3dE5gH9k", 2) + "Zx" + "." + rep("q8Zr4Lm2Vn7T", 3) + "Kp9wX2y"
	tailscale = "tskey" + "-auth-" + "kXGGu7Ra7v11CNTRL-" + rep("Zx9kLmQ2wP", 3) + "ab"
	awsSecret = "wJalrXUtnFEMI" + "/K7MDENG/bPxRfiCYEXAMPLEKEY"
	telegram  = "1234567" + "89:AA" + "HdqTcvCH1vGWJxfSeofSAs0K5PALDsawQ"
	hfToken   = "hf" + "_" + rep("QwErTyUiOpAsDfGhJk", 2)
	groq      = "gs" + "k_" + rep("a1B2c3D4e5F6g7H8i9J0", 3)
	pypi      = "pypi" + "-AgEIcHlwaS5vcmc" + rep("Zx9-kLmQ2_wP", 6)
	vaultTok  = "hv" + "s." + rep("CAESIJ9x2kLmQ2wPzX", 5)
	mailgun   = "key" + "-" + rep("3f9a1c7e", 4)
)

func TestPrefixedFormatsAndWholeTokens(t *testing.T) {
	cases := []struct {
		name, in, kind string
		gone           []string
	}{
		{"sendgrid", "SENDGRID " + sendgrid + " in CI", "sendgrid_key", []string{sendgrid[:10], sendgrid[26:]}},
		{"tailscale auth key", "tailscale up --authkey " + tailscale, "tailscale_key", []string{"kXGGu7Ra7v11CNTRL", "Zx9kLmQ2wP"}},
		{"tailscale api key", "key tskey" + "-api-" + "k123456CNTRL-" + rep("aB3dE5gH9k", 3), "tailscale_key", []string{"k123456CNTRL", "aB3dE5gH9k"}},
		{"aws secret bare", "aws configure set aws_secret " + awsSecret + " done", "aws_secret_key", []string{awsSecret[:12], awsSecret[20:]}},
		{"aws secret in a csv", "AKIA-less row," + awsSecret + "\n", "aws_secret_key", []string{"bPxRfiCY"}},
		{"aws secret quoted", `secret '` + awsSecret + `'`, "aws_secret_key", []string{"bPxRfiCY"}},
		{"aws secret at the end of a sentence", "the secret is " + awsSecret + ".", "aws_secret_key", []string{"bPxRfiCY"}},
		{"telegram", "bot token " + telegram + " ok", "telegram_token", []string{"123456789", "HdqTcvCH1v"}},
		{"telegram in a url", "https://api.telegram.org/bot" + telegram + "/sendMessage", "telegram_token", []string{"123456789", "HdqTcvCH1v"}},
		{"huggingface", "HF " + hfToken, "huggingface_token", []string{"QwErTyUiOp"}},
		{"groq", "export X=1 " + groq, "groq_key", []string{"a1B2c3D4e5"}},
		{"pypi", "twine upload -p " + pypi, "pypi_token", []string{"AgEIcHlwaS5vcmc", "kLmQ2"}},
		{"vault service", "VAULT " + vaultTok, "vault_token", []string{"CAESIJ9x2k"}},
		{"vault batch", "hv" + "b." + rep("AAAAAQKq9zLm3XwP7rT0", 8), "vault_token", []string{"AAAAAQKq9z"}},
		{"mailgun", "mg " + mailgun + " sent", "mailgun_key", []string{"3f9a1c7e"}},
		{"slack app token", "xa" + "pp-1-A0123BCDEF-1234567890123-" + rep("0a1b2c3d4e", 6), "slack_token", []string{"0a1b2c3d4e"}},
		{"slack refresh token", "xo" + "xe-1-" + rep("ABCDEF0123", 15), "slack_token", []string{"ABCDEF0123"}},
		{"stripe prod", "sk" + "_prod_" + rep("4eC39HqLyj", 2), "stripe_key", []string{"4eC39HqLyj"}},
		{"stripe webhook", "wh" + "sec_" + rep("4eC39HqLyjWDarjt", 2), "stripe_key", []string{"4eC39HqLyjWDarjt"}},
		{"aws temporary id", "A3T" + "X" + "Q3EGRA7HZ6FK4M2X", "aws_access_key", []string{"Q3EGRA7HZ6FK4M2X"}},
		{"gitlab feed token", "gl" + "ft-" + rep("xY7kQ2mZ9p", 2), "gitlab_token", []string{"xY7kQ2mZ9p"}},
		{"gitlab runner registration", "GR13" + "48941" + rep("xY7kQ2mZ9p", 2), "gitlab_token", []string{"xY7kQ2mZ9p"}},
		{"xai", "xa" + "i-" + rep("a1B2c3D4e5", 8), "xai_key", []string{"a1B2c3D4e5"}},
		{"replicate", "r8" + "_" + rep("a1B2c3D4e5", 4), "replicate_token", []string{"a1B2c3D4e5"}},
		{"doppler", "dp" + ".pt." + rep("a1B2c3D4e5", 4) + "abc", "doppler_token", []string{"a1B2c3D4e5"}},
		{"linear", "lin" + "_api_" + rep("a1B2c3D4e5", 4), "linear_key", []string{"a1B2c3D4e5"}},
		{"notion", "nt" + "n_" + "12345678901" + rep("a1B2c3D4e5", 3) + "abcde", "notion_token", []string{"a1B2c3D4e5"}},
		{"perplexity", "pp" + "lx-" + rep("a1B2c3D4e5F6", 4), "perplexity_key", []string{"a1B2c3D4e5"}},
		{"databricks", "da" + "pi" + hex32, "databricks_token", []string{hex32}},
		{"pulumi", "pu" + "l-" + hex64[:40], "pulumi_token", []string{hex64[:40]}},
		{"postman", "PM" + "AK-" + hex64[:24] + "-" + hex64[:34], "postman_key", []string{hex64[:24]}},
		{"rubygems", "ruby" + "gems_" + hex64[:48], "rubygems_key", []string{hex64[:48]}},
		{"sentry user", "sntr" + "yu_" + hex64, "sentry_token", []string{hex64}},
		{"planetscale", "pscale" + "_tkn_" + rep("a1B2c3D4e5", 4), "planetscale_token", []string{"a1B2c3D4e5"}},
		{"prefect", "pn" + "u_" + rep("a1B2c3D4e5F6", 3), "prefect_key", []string{"a1B2c3D4e5"}},
		{"grafana cloud", "gl" + "c_" + rep("a1B2c3D4e5F6", 4), "grafana_token", []string{"a1B2c3D4e5"}},
		{"grafana service account", "gl" + "sa_" + rep("a1B2c3D4", 4) + "_" + hex32[:8], "grafana_token", []string{"a1B2c3D4"}},
		{"heroku", "HR" + "KU-AA" + rep("a1B2c3D4e5", 6), "heroku_key", []string{"a1B2c3D4e5"}},
		{"brevo", "xkey" + "sib-" + hex64 + "-" + rep("a1B2c3D4", 2), "brevo_key", []string{hex64}},
		{"square", "sq0" + "atp-" + rep("a1B2c3D4e5F", 2), "square_token", []string{"a1B2c3D4e5F"}},
		{"artifactory", "AK" + "Cp" + rep("a1B2c3D4e5", 7)[:69], "artifactory_key", []string{"a1B2c3D4e5"}},
		{"fly.io", "fo" + "1_" + rep("a1B2c3D4e5", 5)[:43], "flyio_token", []string{"a1B2c3D4e5"}},
		{"openshift", "sha256" + "~" + rep("a1B2c3D4e5", 5)[:43], "openshift_token", []string{"a1B2c3D4e5"}},
		{"1password service account", "op" + "s_eyJ" + rep("a1B2c3D4e5", 26), "onepassword_token", []string{"a1B2c3D4e5"}},
		{"1password secret key", "A3" + "-ABCDEF-ABCDEFGHIJK-ABCDE-ABCDE-ABCDE", "onepassword_token", []string{"ABCDEFGHIJK"}},
		{"newrelic user key", "NR" + "AK-" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ0", "newrelic_key", []string{"ABCDEFGHIJKLMNOP"}},
		{"alibaba", "LT" + "AI" + rep("a1B2c3D4e5", 2), "alibaba_key", []string{"a1B2c3D4e5"}},
		{"atlassian", "ATAT" + "T3" + rep("xFfGF0a1B2c3D4e5", 12), "atlassian_token", []string{"xFfGF0a1B2"}},
		{"azure ad client secret", "secret ab" + "c8Q~" + rep("a1B2c3D4e5", 4)[:34] + " end", "azure_client_secret", []string{"a1B2c3D4e5"}},
		{"bedrock", "AB" + "SK" + rep("QmVkcm9ja0FQSUtleS", 7), "bedrock_key", []string{"QmVkcm9ja0FQSUtleS"}},
		{"supabase", "sb" + "p_" + hex64[:40], "supabase_key", []string{hex64[:40]}},
		{"supabase secret", "sb" + "_secret_" + rep("a1B2c3D4e5", 3), "supabase_key", []string{"a1B2c3D4e5"}},
		{"sourcegraph", "sg" + "p_" + hex64[:16] + "_" + hex64[:40], "sourcegraph_token", []string{hex64[:40]}},
		{"cloudflare origin ca", "v1" + ".0-" + hex64[:24] + "-" + rep(hex64, 3)[:146], "cloudflare_key", []string{hex64[:24]}},
		{"typeform", "tf" + "p_" + rep("a1B2c3D4e5", 6)[:59], "typeform_token", []string{"a1B2c3D4e5"}},
		{"easypost", "EZ" + "AK" + rep("a1B2c3D4e5", 6)[:54], "easypost_key", []string{"a1B2c3D4e5"}},
		{"shippo", "shippo" + "_live_" + hex64[:40], "shippo_token", []string{hex64[:40]}},
		{"clojars", "CLOJARS" + "_" + rep("a1b2c3d4e5", 6), "clojars_token", []string{"a1b2c3d4e5"}},
		{"facebook page token", "EA" + "AC" + rep("a1B2c3D4e5", 11), "facebook_token", []string{"a1B2c3D4e5"}},
		// Passphrases made of words: still high-entropy tokens.
		{"word passphrase", "MyPasswordIsSecretHorse1CorrectBattery2Staple3", "high_entropy", []string{"Horse1Correct"}},
		{"word passphrase 2", "Tom2Jerry3Cat4Dog5Fish6Bird7Bear8Lion9Wolf0Fox", "high_entropy", []string{"Cat4Dog5"}},
		{"nuget password", `<add key="ClearTextPassword" value="hunter2hunter2" />`, "nuget_password", []string{"hunter2hunter2"}},
		{"teams webhook", "https://acme.webhook.office.com/webhookb2/" + "88cf850f-c82e-4b79-b1d7-ecc3af0e50a9@88cf850f-c82e-4b79-b1d7-ecc3af0e50a9/IncomingWebhook/" + hex32 + "/88cf850f-c82e-4b79-b1d7-ecc3af0e50a9", "teams_webhook", []string{hex32}},
	}
	for _, c := range cases {
		out, kinds := Text(c.in)
		for _, g := range c.gone {
			if strings.Contains(out, g) {
				t.Errorf("%s: %q survived: %q", c.name, g, out)
			}
		}
		if !strings.Contains(out, "[redacted:"+c.kind+"]") || !contains(kinds, c.kind) {
			t.Errorf("%s: want kind %s, got %q %v", c.name, c.kind, out, kinds)
		}
	}
}

// The new rules leave what only looks like them alone: a bare 40-character
// value is an AWS secret only with '/' or '+', random, and between
// separators; identifiers made of words are no high-entropy token.
func TestPrefixedLookAlikesSurvive(t *testing.T) {
	for _, s := range []string{
		"commit " + hex64[:40] + " fixed it",
		"commit " + strings.ToUpper(hex64[:40]) + " fixed it",
		"see internal/cli/hook_prompt.go and internal/redact/redact.go",
		"github.com/Barben360/backsight/internal/hindsight/client",
		"open https://github.com/Barben360/backsight/blob/main/internal/cli/x.go",
		"the path src/components/Header/HeaderMenuItem2/index is long",
		"cd /home/annuser/Workspace/github.com/Barben360/backsight",
		"/usr/lib/x86_64-linux-gnu/libQt5WebEngineCore.so.5",
		"func TestServerInstallComposeFailureRecovery2(t *testing.T)",
		"type HTTPServerConfigurationForTheBillingService2 struct{}",
		"hf_hub_download and hf_transfer are installed",
		"use key-value pairs and key-rotation",
		"cache-key-" + hex32 + " restored",
		"restore-keys: npm-key-" + hex32,
		"pubkey-" + hex32 + " is the public one",
		"the hvs. prefix marks a Vault service token",
		"SG.1 and SG.2 are sections",
		"tskey-auth- is the prefix of Tailscale auth keys",
		"gsk_ is Groq's prefix, pypi- PyPI's",
		"port 123456789:AAA is not a token",
		"log 12345:AssertionErrorInTheBillingServiceModule",
		"timestamps 1731400000:42 and 12:30",
		"-----BEGIN PGP PUBLIC KEY BLOCK-----\nmQENBGXk8bQBCADn\n-----END PGP PUBLIC KEY BLOCK-----",
	} {
		out, kinds := Text(s)
		if out != s || len(kinds) != 0 {
			t.Errorf("look-alike changed: %q -> %q %v", s, out, kinds)
		}
	}
}

// A private key header without its END line swallows only the key
// material after it (base64 lines, armour headers), not the rest of the
// text; a key pasted without its END line is still masked whole.
func TestUnterminatedPrivateKeyHeader(t *testing.T) {
	hdr := "-----BEGIN " + "RSA PRIVATE KEY-----"
	body := "MIIEowIBAAKCAQEAx7Lm3XwP7rT0vN8yB2cD4eF6gH1jK5lM9nQ0sU7wY0aZ3bE6\niO/K1SZFPTOtr/KBHBeksoGMGwKq9zLm3XwP7rT0vN8yB2cD4eF6gH1jK5lM9nQ+\nsU7wY0aZ3bE6iO=="
	keep := []string{
		"grep -rn '" + hdr + "' ~/.ssh\nthen we fixed the config and pushed",
		"files starting with " + hdr + " are private keys; never commit them, the CI refuses them",
		`if strings.HasPrefix(s, "` + hdr + `\n") { return errKey } // then the rest of the code`,
		hdr + "\nNote: this is only a header, the key was removed",
	}
	for _, s := range keep {
		out, _ := Text(s)
		if out != s {
			t.Errorf("text after a bare header was eaten:\n%q\n-> %q", s, out)
		}
	}
	l := strings.Split(body, "\n")
	prefixed := func(p string) string { return p + strings.Join(l, "\n"+p) }
	for _, c := range []struct{ name, in, after string }{
		{"lines", "my key:\n" + hdr + "\n" + body + "\nthen I deleted it", "\nthen I deleted it"},
		{"lines to the end", hdr + "\n" + body, ""},
		{"word lines after: still the key's", hdr + "\n" + body + "\nThanks\nbye", ""},
		{"armour headers", hdr + "\nProc-Type: 4,ENCRYPTED\nDEK-Info: AES-128-CBC,3F17F5316E2BAC89\n\n" + body + "\nend of paste", "\nend of paste"},
		{"escaped in json", `{"private_key": "` + hdr + `\n` + strings.ReplaceAll(body, "\n", `\n`) + `", "x": 1}`, `", "x": 1}`},
		{"double-escaped in json", `{"k": "` + hdr + `\\n` + strings.ReplaceAll(body, "\n", `\\n`) + `\\n", "x": 1}`, `\\n", "x": 1}`},
		{"flattened", "KEY=" + hdr + " " + strings.ReplaceAll(body, "\n", " ") + "\nthe next line here", "\nthe next line here"},
		{"glued word after", hdr + "\n" + body[:64] + "\nabcdefghijklmnopq(x) said\nand then some prose", "\nand then some prose"},
		{"crlf", hdr + "\r\n" + strings.ReplaceAll(body, "\n", "\r\n") + "\r\nbye for now", "\r\nbye for now"},
		{"quoted", "> " + hdr + "\n" + prefixed("> ") + "\n\nthat was the key", "\n\nthat was the key"},
		{"commented", "// " + hdr + "\n" + prefixed("// ") + "\nfunc main() { run(the, key, now) }", "\nfunc main() { run(the, key, now) }"},
		{"hash comments", "# " + hdr + "\n" + prefixed("#   ") + "\n# end of the key", "\n# end of the key"},
		{"backslash continuations", `ENV K="` + hdr + `\n\` + "\n" + strings.Join(l, `\n\`+"\n") + `"` + "\nRUN make build and test", `"` + "\nRUN make build and test"},
		{"cat -A", hdr + "$\n" + strings.Join(l, "$\n") + "$\nthe end of the file", "\nthe end of the file"},
		// Review probes: a quote prefix on every line, and a short first
		// chunk on the header's line with the material on the next.
		{"quoted, word line after", "> " + hdr + "\n> MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7\n> abcdEFGH/ijklMNOP+qrstUVWX0123456789x7Lm3XwP\nthanks", ""},
		{"short first chunk", hdr + " MIIEvQ IBADANBgkqhkiG9w0BAQEFAASCBKcw\nabcdEFGH/ijklMNOP+qrstUVWX0123456789x7Lm3XwP\nand that was all", "\nand that was all"},
		{"blank line inside", hdr + "\n" + l[0] + "\n\n" + l[1] + "\n" + l[2] + "\nok then we are done", "\nok then we are done"},
	} {
		out, kinds := Text(c.in)
		for _, frag := range []string{"MIIEow", "MIIEvQ", "K1SZFPTOtr", "KBHBeks", "sU7wY0aZ3bE6iO", "x7Lm3XwP", "abcdEFGH"} {
			if strings.Contains(out, frag) {
				t.Errorf("%s: key material %q survived: %q", c.name, frag, out)
			}
		}
		if !contains(kinds, "private_key") {
			t.Errorf("%s: kinds %v", c.name, kinds)
		}
		if !strings.HasSuffix(out, "[redacted:private_key]"+c.after) {
			t.Errorf("%s: the text after the key changed: %q", c.name, out)
		}
	}
	// Many headers or a long line after one: each header reads only up to
	// the next one, so the work stays linear (no wall clock here: the
	// outcome only).
	many := strings.Repeat(hdr+"\n", 2000) + body
	if out, _ := Text(many); strings.Contains(out, "K1SZFPTOtr") || strings.Count(out, hdr) != 1999 {
		t.Errorf("many headers: %d headers left, key %v", strings.Count(out, hdr), strings.Contains(out, "K1SZFPTOtr"))
	}
	// A header quoted before a real key: the real key alone is masked.
	out, _ := Text("grep '" + hdr + "' found:\n" + privKey + "\nok")
	if !strings.HasPrefix(out, "grep '"+hdr+"' found:\n[redacted:private_key]\nok") {
		t.Errorf("%q", out)
	}
}

// Two credentials one separator apart are both masked: no rule consumes
// the character that bounds its match.
func TestAdjacentMatchesAreAllFound(t *testing.T) {
	aws2 := "Zq8/xLm+3XwP7rT0vN8yB2cD/4eF6gH1jK5lM9nQ"
	tg2 := "987654" + "321:AA" + "FfGgHhJjKk1LlMmNnOoPpQqRrSsTtUuVvWw"
	az1, az2 := "ab"+"c8Q~"+rep("a1B2c3D4e5", 4)[:34], "xy"+"z9Q~"+rep("Zx9kLmQ2wP", 4)[:34]
	for _, c := range []struct{ name, in, kind string }{
		{"aws space", awsSecret + " " + aws2, "aws_secret_key"},
		{"aws comma", awsSecret + "," + aws2, "aws_secret_key"},
		{"telegram space", telegram + " " + tg2, "telegram_token"},
		{"telegram comma", telegram + "," + tg2, "telegram_token"},
		{"azure space", az1 + " " + az2, "azure_client_secret"},
	} {
		out, _ := Text(c.in)
		if want := "[redacted:" + c.kind + "]"; strings.Count(out, want) != 2 {
			t.Errorf("%s: %q", c.name, out)
		}
	}
}
