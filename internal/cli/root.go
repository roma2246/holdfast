// Package cli описывает команды holdfast на cobra.
package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/roma2246/holdfast/internal/lint"
	// Пустой импорт «_»: пакет нужен только ради его init(), который
	// регистрирует правила HF001–HF009 в реестре lint.
	_ "github.com/roma2246/holdfast/internal/lint/rules"
)

// Коды выхода из ТЗ.
const (
	ExitOK       = 0
	ExitFindings = 1 // lint нашёл ошибки
	ExitFailure  = 2 // сбой самого инструмента
)

// exitError — ошибка, которая несёт код выхода. Сообщение не печатается.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

// app — общее состояние для всех команд: потоки ввода-вывода и флаги.
type app struct {
	version string
	in      *bufio.Reader
	out     io.Writer
	errOut  io.Writer
	dir     string // -C / --dir
}

// Execute запускает CLI и возвращает код выхода процесса.
func Execute(version string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := &app{version: version, in: bufio.NewReader(stdin), out: stdout, errOut: stderr}
	root := a.rootCmd()
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)

	err := root.Execute()
	var ee exitError
	switch {
	case err == nil:
		return ExitOK
	case errors.As(err, &ee): // errors.As ищет в цепочке ошибку нужного типа
		return ee.code
	default:
		fmt.Fprintln(stderr, "holdfast: ошибка:", err)
		return ExitFailure
	}
}

func (a *app) rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "holdfast",
		Short: "Линтер инструкций и предохранитель для ИИ-агентов",
		Long: `Holdfast делает работу ИИ-агентов (Claude Code, Cursor, Codex) в репозитории
предсказуемой и безопасной:

  holdfast lint    проверяет CLAUDE.md / AGENTS.md / .cursorrules
  holdfast guard   контролирует, что агент делает с файлами и терминалом
  holdfast sync    превращает запреты из CLAUDE.md в политики guard
  holdfast doctor  проверяет, что хуки подключены и точки восстановления создаются`,
		Version:       a.version,
		SilenceUsage:  true, // не печатать справку при каждой ошибке
		SilenceErrors: true, // ошибки печатаем сами в Execute
	}
	root.PersistentFlags().StringVarP(&a.dir, "dir", "C", ".", "папка репозитория")
	root.AddCommand(a.lintCmd(), a.initCmd(), a.syncCmd(), a.guardCmd(), a.doctorCmd())
	return root
}

// repoRoot — корень репозитория для --dir (папка с .git или holdfast.toml).
func (a *app) repoRoot() (string, error) {
	abs, err := filepath.Abs(a.dir)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", err
	}
	return lint.FindRoot(abs), nil
}

// confirm задаёт вопрос «да/нет». По умолчанию — нет.
func (a *app) confirm(question string) bool {
	fmt.Fprintf(a.out, "%s [y/N]: ", question)
	line, _ := a.in.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes", "д", "да":
		return true
	}
	return false
}

// isTerminal — подключён ли w к терминалу (для цветного вывода).
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && isatty.IsTerminal(f.Fd())
}
