// Entry point of Holdfast. All the logic lives in internal/; here we only
// start the CLI and turn the result into the process exit code.
package main

import (
	"os"
	"runtime/debug"
	"strings"

	"github.com/roma2246/holdfast/internal/cli"
)

// version is injected at build time by GoReleaser through
// -ldflags "-X main.version=1.2.3". The default is "dev".
var version = "dev"

func main() {
	os.Exit(cli.Execute(resolveVersion(version), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// resolveVersion: with `go install …@v0.1.1` no ldflags are set, but Go itself
// embeds the module version in the binary, so we take it from debug.ReadBuildInfo.
func resolveVersion(v string) string {
	if v != "dev" {
		return v
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if mv := info.Main.Version; mv != "" && mv != "(devel)" {
			return strings.TrimPrefix(mv, "v")
		}
	}
	return v
}
