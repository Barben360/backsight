package redact

import (
	"strings"
	"testing"
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
	privKey   = "-----BEGIN " + "OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\nQyNTUxOQAAACD\n-----END " + "OPENSSH PRIVATE KEY-----"
)

func TestTruePositives(t *testing.T) {
	cases := []struct {
		name, in, kind, gone string
	}{
		{"anthropic", "export ANTHROPIC=" + anthropic + " now", "anthropic_key", anthropic},
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
		"sed 's/password:.*/password: ***/' netplan.yaml",
		"Crée un token : Paramètres → Sécurité",
		"the password: correct",
	} {
		out, kinds := Text(s)
		if out != s || len(kinds) != 0 {
			t.Errorf("look-alike changed: %q -> %q %v", s, out, kinds)
		}
	}
}

func TestKindsAreUniqueAndSorted(t *testing.T) {
	_, kinds := Text(github + " and " + github + " and " + aws)
	if strings.Join(kinds, ",") != "aws_access_key,github_token" {
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
