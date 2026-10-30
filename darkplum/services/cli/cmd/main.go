package main

import (
	"os"

	"github.com/alecthomas/kong"

	"github.com/scalecraft-dev/darkplum/cli/internal/commands"
)

func main() {
	var cli commands.CLI
	kctx := kong.Parse(&cli,
		kong.Name("darkplum"),
		kong.Description("Operator CLI for darkplum — admin, sso, status."),
		kong.Help(commands.HelpPrinter),
		kong.UsageOnError(),
	)
	err := kctx.Run(&commands.Globals{Namespace: cli.Namespace})
	if commands.IsReported(err) { // already shown as a ✗ row with a hint
		os.Exit(1)
	}
	kctx.FatalIfErrorf(err)
}
