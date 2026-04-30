package main

import (
	"anvil/cmd/root"

	"github.com/alecthomas/kong"
)

var globalCLI CLI

func main() {
	globalCLI.app = root.NewApp()
	ctx := kong.Parse(&globalCLI,
		kong.Name("anvil"),
		kong.Description("Container runtimes on macOS with minimal setup."),
		kong.UsageOnError(),
		kong.Bind(&globalCLI.Globals),
	)
	err := ctx.Run()
	ctx.FatalIfErrorf(err)
}
