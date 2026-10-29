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
	note  string // a comment line printed before Cmd, also ending Why
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
	if err := refuseTokenArgs(f); err != nil {
		return res, err
	}
	if v, err := release.Parse(version); err != nil || v.Dev() {
		return res, &output.Error{Code: output.Usage, Message: "no release matches this development build (" + version + "): run a released backsight"}
	}
	if f.private != "" {
		ip := net.ParseIP(f.private)
		switch {
		case ip == nil:
			return res, &output.Error{Code: output.Usage, Message: "--private " + f.private + " is not an IP address: give a LAN or tailnet address of the host"}
		case !config.IsTailnet(ip) && !ip.IsPrivate():
			return res, &output.Error{Code: output.Usage, Message: "--private " + f.private + " is not a LAN or tailnet address: give one of the host's (tailscale ip -4 or ip addr there)"}
		}
	}
	answers, err := checkAnswers(f)
	if err != nil {
		return res, err
	}
	if _, ok := answers["consent"]; ok {
		return res, &output.Error{Code: output.Usage, Message: "--answer consent goes with --ssh: with --print-commands, setup and connect ask for consent at their own terminal, so there is none to give"}
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
	exposeNote := ""
	if v, ok := answers["cleartext_lan"]; ok {
		expose += " --answer cleartext_lan=" + shellWord(v)
	} else if a := net.ParseIP(f.private); a != nil && !config.IsTailnet(a) {
		exposeNote = f.private + " is not a tailnet address: the API key would travel unencrypted on this network, so server expose asks you to answer cleartext_lan=yes"
	}
	join := func(parts ...string) string {
		return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), " ")
	}

	res.Commands = append(res.Commands, printedCommand{Where: "host",
		Cmd: fmt.Sprintf("curl -fsSL %s%s/scripts/install.sh | BACKSIGHT_RELEASE_URL=%s%s sh", printRepo, version, printRelease, version),
		Why: "installs backsight v" + version + ", the release matching this PC: install.sh checks its signature and hash on the host"})
	if !config.KeylessProviders[provider] {
		res.Commands = append(res.Commands, printedCommand{Where: "host", Cmd: "backsight secret set llm_api_key",
			Why: "asks for the LLM API key at a hidden prompt: it never goes on a command line"})
	}
	res.Commands = append(res.Commands,
		printedCommand{Where: "host", Cmd: join("backsight setup --role server", join(hostAns...)),
			Why: "sets the server up; it shows its plan and asks you to approve it at the host's terminal"},
		printedCommand{Where: "host", Cmd: expose,
			Why: "opens the server to your other PCs on this LAN or tailnet address of the host", note: exposeNote},
		printedCommand{Where: "host", Cmd: "backsight server token --out " + printToken,
			Why: "writes the connection token, which holds the API key, to a file only you can read (mode 0600), never to the screen. Carry " + printToken +
				" to this PC yourself (USB stick, scp), never through a chat or a shared folder"},
		printedCommand{Where: "this-pc", Cmd: join("backsight connect --token-file "+printToken, join(clientAns...)),
			Why: "joins this PC to the host's memory; it shows its plan and asks. Then delete " + printToken + " on both machines: it holds the API key. To undo on the host: backsight setup --undo"})
	for i, c := range res.Commands {
		if c.note != "" {
			res.Commands[i].Why += ". " + strings.ToUpper(c.note[:1]) + c.note[1:]
		}
	}
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
			if c.note != "" {
				b.WriteString("# " + c.note + "\n")
			}
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
