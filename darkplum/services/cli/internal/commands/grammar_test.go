package commands

import (
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
