package commands

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/alecthomas/kong"
	"golang.org/x/term"
)

//go:embed banner.txt
var bannerArt string

// Banner writes the darkplum wordmark and tagline to w, colored in brand Grape
// when w is a terminal and NO_COLOR is unset.
func Banner(w io.Writer) {
	art := strings.TrimRight(bannerArt, "\n")
	tagline := "    private ai. yours to run."
	rule := "    " + strings.Repeat("─", 41)

	if useColor(w) {
		art = ansiBrand + art + ansiReset
		tagline = ansiSage + tagline + ansiReset
		rule = ansiMoss + rule + ansiReset
	}

	fmt.Fprintf(w, "\n%s\n%s\n%s\n\n", art, tagline, rule)
}

// HelpPrinter renders the banner plus brand-styled help for whichever node is
// selected — root, command, or subcommand — so every help screen is consistent.
// It fully replaces kong's default printer; the layout is derived from the
// parsed model so it can't drift from the real command tree.
func HelpPrinter(_ kong.HelpOptions, ctx *kong.Context) error {
	Banner(ctx.Stdout)
	node := ctx.Selected()
	if node == nil {
		node = ctx.Model.Node
	}
	renderHelp(ctx.Stdout, node)
	return nil
}

func renderHelp(w io.Writer, node *kong.Node) {
	row := func(name, help string) {
		fmt.Fprintf(w, "  %s  %s\n", sCmd(fmt.Sprintf("%-16s", name)), sDim(help))
	}

	path := cmdPath(node)
	usage := path
	if len(visibleChildren(node)) > 0 {
		usage += " <command>"
	}
	for _, p := range node.Positional {
		usage += " <" + p.Name + ">"
	}
	usage += " [flags]"
	fmt.Fprintf(w, "%s\n  %s\n", sHeader("Usage"), usage)
	if node.Parent != nil && node.Help != "" {
		fmt.Fprintf(w, "  %s\n", sDim(node.Help))
	}
	fmt.Fprintln(w)

	if cmds := visibleChildren(node); len(cmds) > 0 {
		fmt.Fprintf(w, "%s\n", sHeader("Commands"))
		for _, c := range cmds {
			row(c.Name, c.Help)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "%s\n", sHeader("Options"))
	for _, f := range gatherFlags(node) {
		row(flagLabel(f), flagDesc(f))
	}
	fmt.Fprintln(w)

	if len(visibleChildren(node)) > 0 {
		fmt.Fprintf(w, "%s\n", sDim("Run “"+path+" <command> --help” for details on a command."))
	}
}

// visibleChildren returns a node's non-hidden subcommands.
func visibleChildren(n *kong.Node) []*kong.Node {
	var out []*kong.Node
	for _, c := range n.Children {
		if !c.Hidden {
			out = append(out, c)
		}
	}
	return out
}

// cmdPath is the full space-joined command path (e.g. "darkplum sso configure").
func cmdPath(n *kong.Node) string {
	var parts []string
	for cur := n; cur != nil; cur = cur.Parent {
		if cur.Name != "" {
			parts = append([]string{cur.Name}, parts...)
		}
	}
	return strings.Join(parts, " ")
}

// gatherFlags collects a node's flags plus inherited globals (namespace, help),
// nearest-scope first, de-duplicated by name.
func gatherFlags(n *kong.Node) []*kong.Flag {
	seen := map[string]bool{}
	var out []*kong.Flag
	for cur := n; cur != nil; cur = cur.Parent {
		for _, f := range cur.Flags {
			if seen[f.Name] {
				continue
			}
			seen[f.Name] = true
			out = append(out, f)
		}
	}
	return out
}

func flagLabel(f *kong.Flag) string {
	if f.Short != 0 {
		return fmt.Sprintf("-%c, --%s", f.Short, f.Name)
	}
	return "--" + f.Name
}

func flagDesc(f *kong.Flag) string {
	d := f.Help
	if f.Default != "" {
		if d != "" {
			d += " "
		}
		d += "(default: " + f.Default + ")"
	}
	return d
}

func useColor(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
