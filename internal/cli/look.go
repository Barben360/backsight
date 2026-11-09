package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

// lookClass is how a command looks at a terminal (decision 0084 D2):
// plain commands are run by programs or print a value meant to be
// captured, so they never get color or a splash; no-splash commands get
// color only; full commands get both.
type lookClass string

const (
	lookPlain    lookClass = "plain"
	lookNoSplash lookClass = "no-splash"
	lookFull     lookClass = "full"
)

// lookAnnotation is the cobra annotation holding a command's class.
const lookAnnotation = "backsight.look"

// setLook gives cmd and, by inheritance, its subcommands class c.
func setLook(cmd *cobra.Command, c lookClass) *cobra.Command {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[lookAnnotation] = string(c)
	return cmd
}

// classOf is the class of cmd's nearest annotated ancestor (itself
// first). Cobra's completion command and its subcommands, which cobra
// adds itself, are plain: a shell reads their output. Otherwise full.
func classOf(cmd *cobra.Command) lookClass {
	for c := cmd; c != nil; c = c.Parent() {
		if v, ok := c.Annotations[lookAnnotation]; ok {
			return lookClass(v)
		}
		if c.Name() == "completion" && c.HasParent() && !c.Parent().HasParent() {
			return lookPlain
		}
	}
	return lookFull
}

// classForArgs is the class of the command args will run, before cobra
// parses them. Cobra creates __complete inside ExecuteC, so it is
// recognised by name; an unknown command is full (its error goes to
// stderr as today).
func classForArgs(root *cobra.Command, args []string) lookClass {
	if len(args) > 0 && strings.HasPrefix(args[0], cobra.ShellCompRequestCmd) {
		return lookPlain
	}
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd(args...)
	cmd, _, err := root.Find(args)
	if err != nil || cmd == nil {
		return lookFull
	}
	return classOf(cmd)
}
