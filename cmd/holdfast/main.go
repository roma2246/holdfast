// Точка входа Holdfast. Вся логика живёт в internal/, здесь только
// запуск CLI и преобразование результата в код выхода процесса.
package main

import (
	"os"
	"runtime/debug"
	"strings"

	"github.com/roma2246/holdfast/internal/cli"
)

// version подставляется при сборке GoReleaser'ом через
// -ldflags "-X main.version=1.2.3". По умолчанию — "dev".
var version = "dev"

func main() {
	os.Exit(cli.Execute(resolveVersion(version), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// resolveVersion: при `go install …@v0.1.1` ldflags не задаются, но Go сам
// встраивает в бинарник версию модуля — берём её из debug.ReadBuildInfo.
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
