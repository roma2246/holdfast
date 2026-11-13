// Точка входа Holdfast. Вся логика живёт в internal/, здесь только
// запуск CLI и преобразование результата в код выхода процесса.
package main

import (
	"os"

	"github.com/roma2246/holdfast/internal/cli"
)

// version подставляется при сборке GoReleaser'ом через
// -ldflags "-X main.version=1.2.3". По умолчанию — "dev".
var version = "dev"

func main() {
	os.Exit(cli.Execute(version, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
