package cli

import (
	"fmt"
	"net"
	"slices"
	"strings"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/release"
)

// printedCommand is one step of an install by hand.
type printedCommand struct {
	Where string `json:"where"` // "host" | "this-pc"
	Cmd   string `json:"cmd"`
	Why   string `json:"why"`
}

// printedCommands is server install --print-commands: the steps of
// --ssh for a user who has no ssh access to the host.
type printedCommands struct {
	Version  string           `json:"version"`
	Commands []printedCommand `json:"commands"`
}

const (
	printRepo    = "https://raw.githubusercontent.com/Barben360/backsight/v"
	printRelease = "https://github.com/Barben360/backsight/releases/download/v"
	printToken   = "backsight-token"
)

// printCommands lists the commands that install the memory server by hand
// on another machine and join it from this one. It reads no config and no
// secret, runs no ssh and writes no file; the setup answers come from the
// install flags and --answer, and none is consent, so setup shows its plan
// and asks at the host's terminal. No key is ever in a command: it goes to
// the host's own hidden prompt, and the token travels in a file.
func printCommands(f sshInstallFlags, version string) (printedCommands, error) {
	res := printedCommands{Version: version}
	switch {
	case f.target != "":
		return res, &output.Error{Code: output.Usage, Message: "--ssh and --print-commands exclude each other: --ssh runs the steps, --print-commands lists them for a host you reach yourself"}
	case f.plan:
		return res, &output.Error{Code: output.Usage, Message: "--plan and --print-commands exclude each other: --print-commands changes nothing and lists the steps"}
	case f.source != "", f.keyEnv != "", f.keyFile != "", f.nonInteractive, f.configDir != "":
		return res, &output.Error{Code: output.Usage, Message: "--source, --llm-key-from-env, --llm-key-from-file, --non-interactive and --config-dir go with --ssh: --print-commands lists the steps and reads no key; the host's own prompt asks for it"}
	}
	if slices.ContainsFunc(append([]string{f.private}, f.answers...), looksLikeToken) {
		return res, &output.Error{Code: output.Usage, Message: tokenArgument}
	}
	if v, err := release.Parse(version); err != nil || v.Dev() {
		return res, &output.Error{Code: output.Usage, Message: "no release matches this development build (" + version + "): run a released backsight"}
	}
	if f.private != "" && net.ParseIP(f.private) == nil {
		return res, &output.Error{Code: output.Usage, Message: "--private " + f.private + " is not an IP address: give a LAN or tailnet address of the host"}
	}
	answers, err := checkAnswers(f)
	if err != nil {
		return res, err
	}

	provider := answers["provider"]
	var hostAns, clientAns []string
	for _, id := range hostAnswers {
		v, ok := answers[id]
		for _, p := range installFlagAnswers {
			if fv, set := f.flagValue(p[0]); set && p[1] == id {
				v, ok = fv, true
			}
		}
		if id == "provider" {
			provider = v
		}
		if ok {
			hostAns = append(hostAns, "--answer "+shellWord(id+"="+v))
		}
	}
	if v, ok := answers["ack_subscription"]; ok {
		hostAns = append(hostAns, "--answer "+shellWord("ack_subscription="+v))
	}
	for _, id := range clientAnswers {
		if v, ok := answers[id]; ok {
			clientAns = append(clientAns, "--answer "+shellWord(id+"="+v))
		}
	}
	ip := "<ip>"
	if f.private != "" {
		ip = shellWord(f.private)
	}
	expose := "backsight server expose --private " + ip
	if v, ok := answers["cleartext_lan"]; ok {
		expose += " --answer cleartext_lan=" + shellWord(v)
	}
	join := func(parts ...string) string {
		return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), " ")
	}

	res.Commands = append(res.Commands, printedCommand{"host",
		fmt.Sprintf("curl -fsSL %s%s/scripts/install.sh | BACKSIGHT_RELEASE_URL=%s%s sh", printRepo, version, printRelease, version),
		"installs backsight v" + version + ", the release matching this PC: install.sh checks its signature and hash on the host"})
	if !config.KeylessProviders[provider] {
		res.Commands = append(res.Commands, printedCommand{"host", "backsight secret set llm_api_key",
			"asks for the LLM API key at a hidden prompt: it never goes on a command line"})
	}
	res.Commands = append(res.Commands,
		printedCommand{"host", join("backsight setup --role server", join(hostAns...)),
			"sets the server up; it shows its plan and asks you to approve it at the host's terminal"},
		printedCommand{"host", expose,
			"opens the server to your other PCs on this LAN or tailnet address of the host"},
		printedCommand{"host", "backsight server token --out " + printToken,
			"writes the connection token, which holds the API key, to a file only you can read (mode 0600), never to the screen. Carry " + printToken +
				" to this PC yourself (USB stick, scp), never through a chat or a shared folder"},
		printedCommand{"this-pc", join("backsight connect --token-file "+printToken, join(clientAns...)),
			"joins this PC to the host's memory; it shows its plan and asks. Then delete " + printToken + " on both machines: it holds the API key. To undo on the host: backsight setup --undo"})
	return res, nil
}

// printedCommandsText is printCommands as a script to read: comments and
// the commands to copy.
func printedCommandsText(r printedCommands) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# backsight %s: install the memory server on another machine by hand.\n", r.Version)
	b.WriteString("# On the host (Linux with Docker), as the user who will run the server:\n")
	for _, c := range r.Commands {
		if c.Where == "host" {
			b.WriteString(c.Cmd + "\n")
		}
	}
	b.WriteString("# Carry " + printToken + " to this PC yourself (USB stick, scp), never through a chat or a shared folder.\n")
	b.WriteString("# On this PC:\n")
	for _, c := range r.Commands {
		if c.Where == "this-pc" {
			b.WriteString(c.Cmd + "\n")
		}
	}
	b.WriteString("# Then delete " + printToken + " on both machines: it holds the API key. To undo on the host: backsight setup --undo.\n")
	return b.String()
}
