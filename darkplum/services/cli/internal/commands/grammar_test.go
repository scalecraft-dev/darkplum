package commands

import (
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

// The kong grammar is built at start-up; a duplicate flag or a malformed tag is a
// panic there, before any command runs. Build it in a test, and parse the commands
// added with the release machinery.
func TestGrammarBuildsAndParses(t *testing.T) {
	var cli CLI
	p, err := kong.New(&cli, kong.Name("darkplum"), kong.Exit(func(int) {}))
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"version"},
		{"version", "--json"},
		{"version", "--bundle-manifest"},
		{"verify-release", "--cache-dir", "/tmp/x"},
		{"init", "--adopt", "--dir", "/x"},
		{"--install", "acme", "init", "--adopt"},
		{"--install", "acme", "status"},
		{"kubeconfig", "--write"},
		{"status", "--context", "kind-x"},
	} {
		cli = CLI{}
		if _, err := p.Parse(args); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
	// the global --install is read by every command through Globals
	cli = CLI{}
	if _, err := p.Parse([]string{"--install", "acme", "kubeconfig"}); err != nil || cli.Install != "acme" {
		t.Errorf("--install = %q, %v", cli.Install, err)
	}
}

// Flags marked hidden (the development-only deploy flags) stay out of --help.
func TestHiddenFlagsAreNotInHelp(t *testing.T) {
	var cli CLI
	var buf strings.Builder
	k, err := kong.New(&cli, kong.Name("darkplum"), kong.Help(HelpPrinter), kong.Writers(&buf, &buf), kong.Exit(func(int) {}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Parse([]string{"deploy", "--help"}); err != nil {
		t.Log(err)
	}
	help := buf.String()
	for _, hidden := range []string{"--release-images", "--unsigned-images", "--gpu-wait"} {
		if strings.Contains(help, hidden) {
			t.Errorf("%s is listed in deploy --help", hidden)
		}
	}
	if !strings.Contains(help, "--confirm-plan") {
		t.Errorf("--confirm-plan is missing from deploy --help:\n%s", help)
	}
}
