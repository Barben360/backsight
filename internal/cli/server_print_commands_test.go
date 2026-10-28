package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/output"
)

const printedBlock = `# backsight 9.9.9: install the memory server on another machine by hand.
# On the host (Linux with Docker), as the user who will run the server:
curl -fsSL https://raw.githubusercontent.com/Barben360/backsight/v9.9.9/scripts/install.sh | BACKSIGHT_RELEASE_URL=https://github.com/Barben360/backsight/releases/download/v9.9.9 sh
backsight secret set llm_api_key
backsight setup --role server --answer provider=anthropic --answer model=m
backsight server expose --private 100.101.1.2
backsight server token --out backsight-token
# Carry backsight-token to this PC yourself (USB stick, scp), never through a chat or a shared folder.
# On this PC:
backsight connect --token-file backsight-token
# Then delete backsight-token on both machines: it holds the API key. To undo on the host: backsight setup --undo.
`

var printArgs = []string{"server", "install", "--print-commands", "--provider", "anthropic", "--model", "m", "--private", "100.101.1.2"}

func TestPrintCommandsText(t *testing.T) {
	setBuild(t, "9.9.9", "abc1234")
	code, out, errOut := run(t, printArgs...)
	if code != output.OK || out != printedBlock {
		t.Fatalf("exit %d, stderr %q, stdout:\n%s\nwant:\n%s", code, errOut, out, printedBlock)
	}
}

type printedEnvelope struct {
	OK    bool            `json:"ok"`
	Data  printedCommands `json:"data"`
	Error *output.Error   `json:"error"`
}

func TestPrintCommandsJSON(t *testing.T) {
	setBuild(t, "9.9.9", "abc1234")
	code, out, errOut := run(t, append(printArgs, "--json")...)
	var env printedEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil || code != output.OK {
		t.Fatalf("exit %d: %v\n%s%s", code, err, out, errOut)
	}
	cs := env.Data.Commands
	if env.Data.Version != "9.9.9" || len(cs) != 6 {
		t.Fatalf("version %q, %d commands, want 9.9.9 and 6: %+v", env.Data.Version, len(cs), cs)
	}
	for i, c := range cs {
		want := "host"
		if i == 5 {
			want = "this-pc"
		}
		if c.Where != want || c.Cmd == "" || c.Why == "" {
			t.Errorf("command %d = %+v, want where %s, a cmd and a why", i, c, want)
		}
	}
	if strings.Contains(out, "consent") {
		t.Errorf("the output holds a consent answer: setup must ask at the host\n%s", out)
	}
}

// Nothing is read or run: ssh refuses, no key flag is taken, no secret is
// printed and this PC's tree is unchanged.
func TestPrintCommandsCarryNoSecretAndRunNothing(t *testing.T) {
	home, _ := setupMachine(t)
	setBuild(t, "9.9.9", "abc1234")
	t.Setenv(llmKeyEnv, llmKey)
	before := treeOf(t, home)
	code, out, errOut := run(t, append(printArgs, "--llm-key-from-env", llmKeyEnv)...)
	if code != output.Usage || !strings.Contains(errOut, "go with --ssh") || strings.Contains(out+errOut, llmKey) {
		t.Errorf("a key flag: exit %d, want 2 saying the key flags go with --ssh\n%s%s", code, out, errOut)
	}
	code, out, errOut = run(t, printArgs...)
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	for _, bad := range []string{llmKey, "--token ", "--token-file backsight-token --token"} {
		if strings.Contains(out+errOut, bad) {
			t.Errorf("the output holds %q:\n%s", bad, out)
		}
	}
	sameTree(t, before, treeOf(t, home))
}

func TestPrintCommandsKeylessProvider(t *testing.T) {
	setBuild(t, "9.9.9", "abc1234")
	code, out, _ := run(t, "server", "install", "--print-commands", "--provider", "ollama")
	if code != output.OK || strings.Contains(out, "secret set") || !strings.Contains(out, "--answer provider=ollama") {
		t.Errorf("exit %d, want no secret set line and the provider answered\n%s", code, out)
	}
	code, out, _ = run(t, "server", "install", "--print-commands")
	if code != output.OK || !strings.Contains(out, "secret set llm_api_key") || !strings.Contains(out, "expose --private <ip>") {
		t.Errorf("default provider: exit %d, want a secret set line and an <ip> placeholder\n%s", code, out)
	}
}

func TestPrintCommandsRefusals(t *testing.T) {
	setBuild(t, "9.9.9", "abc1234")
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"ssh":                        {[]string{"--ssh", "u@h"}, "exclude each other"},
		"plan":                       {[]string{"--plan"}, "exclude each other"},
		"source":                     {[]string{"--source", "/tmp/x"}, "go with --ssh"},
		"key":                        {[]string{"--llm-key-from-file", "/tmp/k"}, "go with --ssh"},
		"answer":                     {[]string{"--answer", "nope=1"}, "unknown answer"},
		"ip":                         {[]string{"--private", "host.example"}, "not an IP address"},
		"public":                     {[]string{"--private", "8.8.8.8"}, "not a LAN or tailnet address"},
		"consent":                    {[]string{"--answer", "consent=yes"}, "at their own terminal"},
		"token in an answer's value": {[]string{"--answer", "provider=bst1.xxx"}, "token"},
		"token as an answer":         {[]string{"--answer", "bst1.xxx"}, "token"},
		"token in --private":         {[]string{"--private", "bst1.xxx"}, "token"},
	} {
		t.Run(name, func(t *testing.T) {
			code, out, errOut := run(t, append([]string{"server", "install", "--print-commands", "--json"}, c.args...)...)
			if code != output.Usage || !strings.Contains(out+errOut, c.want) || strings.Contains(out+errOut, "bst1.xxx\\n") || strings.Contains(out, "backsight setup") {
				t.Errorf("exit %d, want 2 naming %q\n%s%s", code, c.want, out, errOut)
			}
		})
	}
}

func TestPrintCommandsRefusesADevBuild(t *testing.T) {
	for _, v := range []string{"dev", "0.1.7-3-gabc1234", "0.1.7-dirty"} {
		setBuild(t, v, "abc1234")
		code, out, errOut := run(t, "server", "install", "--print-commands")
		if code != output.Usage || !strings.Contains(errOut, "no release matches this development build ("+v+")") || out != "" {
			t.Errorf("%s: exit %d, want 2 naming the build\n%s%s", v, code, out, errOut)
		}
	}
}

// Every printed command is one this binary has, with flags it takes.
func TestPrintedCommandsExist(t *testing.T) {
	root := docsRoot()
	res, err := printCommands(sshInstallFlags{private: "100.101.1.2", answers: []string{"cleartext_lan=yes", "retention=keep"},
		install: installFlags{provider: "anthropic", model: "m"}, changed: func(string) bool { return true }}, "9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range res.Commands {
		rest, ok := strings.CutPrefix(c.Cmd, "backsight ")
		if !ok {
			continue
		}
		n++
		if err := checkMention(root, strings.Fields(rest), nil); err != nil {
			t.Errorf("`%s`: %v", c.Cmd, err)
		}
	}
	// The undo the closing comment names.
	if err := checkMention(root, strings.Fields("setup --undo"), nil); err != nil {
		t.Error(err)
	}
	if n != 5 {
		t.Errorf("%d backsight commands checked, want 5", n)
	}
}

// Each answer lands on the command that takes it.
func TestPrintCommandsPlaceEachAnswer(t *testing.T) {
	setBuild(t, "9.9.9", "abc1234")
	code, out, errOut := run(t, "server", "install", "--print-commands", "--provider", "claude-code", "--private", "192.168.1.5",
		"--answer", "cleartext_lan=yes", "--answer", "retention=keep", "--answer", "backfill=no", "--answer", "ack_subscription=yes")
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	for _, want := range []string{
		"backsight setup --role server --answer provider=claude-code --answer ack_subscription=yes\n",
		"backsight server expose --private 192.168.1.5 --answer cleartext_lan=yes\n",
		"backsight connect --token-file backsight-token --answer retention=keep --answer backfill=no\n",
	} {
		if !strings.Contains(out, "\n"+want) {
			t.Errorf("output lacks the line %q\n%s", want, out)
		}
	}
}
