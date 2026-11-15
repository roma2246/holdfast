package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/roma2246/holdfast/internal/config"
	"github.com/roma2246/holdfast/internal/guard"
)

// check — результат одной проверки doctor.
type check struct {
	ok     bool
	title  string // что проверяли
	detail string // что нашли
	hint   string // как исправить (только для ✗)
}

func (a *app) doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Проверить, что Holdfast подключён и работает",
		Long: `Проверяет установку и печатает ✓ или ✗ с подсказкой, как исправить:
  git установлен; хуки прописаны в .claude/settings.json или settings.local.json;
  Claude Code найдёт бинарник holdfast; holdfast.toml читается; в журнале нет
  свежих ошибок; точки восстановления есть.

Код выхода: 0 — всё в порядке, 2 — есть проблемы.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := a.repoRoot()
			if err != nil {
				return err
			}
			checks := runDoctor(root, time.Now())
			a.printChecks(root, checks)
			for _, c := range checks {
				if !c.ok {
					return exitError{ExitFailure}
				}
			}
			return nil
		},
	}
}

// runDoctor выполняет все проверки по порядку.
func runDoctor(root string, now time.Time) []check {
	gitOK, gitCheck := checkGitInstalled()
	hooks, hooksCheck := checkHooks(root)
	checks := []check{gitCheck, hooksCheck}
	checks = append(checks, checkBinaries(root, hooks)...)
	checks = append(checks, checkConfig(root))
	checks = append(checks, checkLog(root, now)...)
	return append(checks, checkSnapshots(root, gitOK, now))
}

func (a *app) printChecks(root string, checks []check) {
	r := lipgloss.NewRenderer(a.out)
	okMark := r.NewStyle().Foreground(lipgloss.Color("10")).Render("✓")
	badMark := r.NewStyle().Foreground(lipgloss.Color("9")).Bold(true).Render("✗")
	faint := r.NewStyle().Faint(true)

	fmt.Fprintf(a.out, "holdfast doctor — %s\n\n", root)
	problems := 0
	for _, c := range checks {
		mark := okMark
		if !c.ok {
			mark = badMark
			problems++
		}
		fmt.Fprintf(a.out, "%s %s", mark, c.title)
		if c.detail != "" {
			fmt.Fprint(a.out, faint.Render(" — "+c.detail))
		}
		fmt.Fprintln(a.out)
		if !c.ok && c.hint != "" {
			fmt.Fprintf(a.out, "  → %s\n", c.hint)
		}
	}
	fmt.Fprintln(a.out)
	if problems == 0 {
		fmt.Fprintln(a.out, "Всё в порядке: действия агента проверяются, точки восстановления создаются.")
		return
	}
	fmt.Fprintf(a.out, "Проблем: %d\n", problems)
}

func checkGitInstalled() (bool, check) {
	c := check{title: "git установлен"}
	path, err := exec.LookPath("git")
	if err != nil {
		c.detail = "git не найден в PATH"
		c.hint = "установите git: точки восстановления и откат работают через него"
		return false, c
	}
	c.ok = true
	c.detail = path
	if out, err := exec.Command(path, "--version").Output(); err == nil {
		c.detail = strings.TrimSpace(string(out))
	}
	return true, c
}

// checkHooks ищет оба хука holdfast в настройках Claude Code.
func checkHooks(root string) ([]guard.InstalledHook, check) {
	c := check{title: "хуки Claude Code"}
	hooks, err := guard.FindHooks(root)
	if err != nil {
		c.detail = err.Error()
		c.hint = "исправьте JSON в файле: пока он не читается, Claude Code не вызовет хуки"
		return hooks, c
	}
	files := map[string]bool{}
	have := map[string]bool{}
	for _, h := range hooks {
		have[h.Event] = true
		files[relTarget(root, h.File)] = true
	}
	var missing []string
	if !have["PreToolUse"] {
		missing = append(missing, "PreToolUse → holdfast guard check (проверка действий агента)")
	}
	if !have["UserPromptSubmit"] {
		missing = append(missing, "UserPromptSubmit → holdfast guard checkpoint (точки восстановления)")
	}
	if len(missing) > 0 {
		c.detail = "нет " + strings.Join(missing, "; ")
		c.hint = "holdfast guard install (или --local, чтобы не коммитить настройки) и перезапустите сессию Claude Code"
		return hooks, c
	}
	c.ok = true
	c.detail = "PreToolUse и UserPromptSubmit в " + strings.Join(sortedKeys(files), ", ")
	return hooks, c
}

// checkBinaries проверяет, что Claude Code найдёт программу из каждого хука.
// Хуков нет — проверяем holdfast в PATH: его пропишет guard install.
func checkBinaries(root string, hooks []guard.InstalledHook) []check {
	bins := map[string]bool{}
	for _, h := range hooks {
		bins[h.Bin] = true
	}
	if len(bins) == 0 {
		bins[guard.DefaultBin] = true
	}
	var out []check
	for _, bin := range sortedKeys(bins) {
		c := check{title: "бинарник для хуков: " + bin}
		path, err := resolveBin(root, bin)
		if err != nil {
			c.detail = err.Error()
			c.hint = binHint(bin)
		} else {
			c.ok, c.detail = true, path
		}
		out = append(out, c)
	}
	return out
}

// resolveBin находит программу так же, как shell, в котором Claude Code
// запускает хук: имя без «/» ищется в PATH, путь — относительно папки
// проекта (Claude Code запускает хуки из неё).
func resolveBin(root, bin string) (string, error) {
	if !strings.Contains(bin, "/") {
		path, err := exec.LookPath(bin)
		if err != nil {
			return "", fmt.Errorf("%s не найден в PATH", bin)
		}
		return path, nil
	}
	path := bin
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	fi, err := os.Stat(path)
	switch {
	case err != nil:
		return "", fmt.Errorf("файла %s нет", path)
	case fi.IsDir() || fi.Mode().Perm()&0o111 == 0: // ни одного бита «x»
		return "", fmt.Errorf("%s не исполняемый файл", path)
	}
	return path, nil
}

func binHint(bin string) string {
	self, err := os.Executable()
	if err != nil || strings.Contains(bin, "/") {
		return "переустановите holdfast или пропишите хуки заново с верным путём: holdfast guard uninstall && holdfast guard install --bin /путь/к/holdfast"
	}
	return fmt.Sprintf("добавьте папку с holdfast в PATH или пропишите полный путь: holdfast guard uninstall && holdfast guard install --bin %s", self)
}

func checkConfig(root string) check {
	c := check{title: config.FileName + " читается"}
	if _, err := os.Stat(filepath.Join(root, config.FileName)); errors.Is(err, fs.ErrNotExist) {
		c.ok, c.detail = true, "файла нет — действуют встроенные правила"
		return c
	}
	if _, err := config.Load(root); err != nil {
		c.detail = err.Error()
		c.hint = "исправьте файл: пока он не читается, правила проекта не применяются, а действия агента требуют подтверждения"
		return c
	}
	c.ok = true
	return c
}

// recentWindow — какие ошибки в журнале считаются свежими.
const recentWindow = 24 * time.Hour

// checkLog ищет в журнале ошибки, которые ещё не исправлены:
//   - config-error после последнего изменения holdfast.toml (и не старше
//     суток): если файл с тех пор правили, ошибка, скорее всего, устранена;
//   - неудачную последнюю точку восстановления (.holdfast/checkpoint-error
//     удаляется при первой же удачной точке).
func checkLog(root string, now time.Time) []check {
	entries, err := guard.ReadLog(root)
	if err != nil {
		return []check{{title: "журнал .holdfast/log.jsonl", detail: err.Error(),
			hint: "проверьте права на папку .holdfast/"}}
	}
	dayAgo := now.Add(-recentWindow)
	// Ошибка config-error свежая, если она записана позже последней правки
	// holdfast.toml. Файла нет — конфиг не может не читаться.
	var cfgChanged time.Time
	cfgExists := false
	if fi, err := os.Stat(filepath.Join(root, config.FileName)); err == nil {
		cfgExists, cfgChanged = true, fi.ModTime()
	}
	var (
		cfgErrors, snapErrors, checkpoints, total int
		lastCfgErr                                string
	)
	for _, e := range entries {
		if !e.Time.After(dayAgo) {
			continue
		}
		total++
		switch {
		case e.Rule == "config-error":
			if cfgExists && e.Time.After(cfgChanged) {
				cfgErrors++
				lastCfgErr = e.Reason
			}
		case e.Decision == "snapshot" && e.Rule == "error":
			snapErrors++
		case e.Decision == "snapshot":
			checkpoints++
		}
	}

	logCheck := check{title: "в журнале нет свежих ошибок config-error", ok: cfgErrors == 0}
	if cfgErrors > 0 {
		logCheck.detail = fmt.Sprintf("%d после последнего изменения %s: %s", cfgErrors, config.FileName, truncate(lastCfgErr, 120))
		logCheck.hint = "исправьте " + config.FileName + " и проверьте: holdfast guard log --decision ask"
	} else {
		logCheck.detail = fmt.Sprintf("записей за сутки: %d", total)
	}

	cpCheck := check{title: "точки восстановления создаются без ошибок", ok: true}
	if msg := guard.LastCheckpointError(root); msg != "" {
		cpCheck.ok = false
		cpCheck.detail = truncate(msg, 160)
		cpCheck.hint = "устраните причину; следующая точка создастся перед следующим запросом (или вручную: holdfast guard start)"
	} else if snapErrors > 0 {
		cpCheck.detail = fmt.Sprintf("за сутки неудачных точек: %d, после них точки создаются", snapErrors)
	} else if checkpoints > 0 {
		cpCheck.detail = fmt.Sprintf("за сутки: %d", checkpoints)
	} else {
		cpCheck.detail = "ошибок нет"
	}
	return []check{logCheck, cpCheck}
}

func checkSnapshots(root string, gitOK bool, now time.Time) check {
	c := check{title: "точки восстановления в " + guard.SnapshotRefPrefix}
	if !gitOK {
		c.detail = "без git точек восстановления нет"
		c.hint = "установите git"
		return c
	}
	g, err := guard.OpenGit(root)
	if err != nil {
		c.detail = "папка не в git-репозитории"
		c.hint = "git init: точки восстановления хранятся в git"
		return c
	}
	snaps, err := g.Snapshots()
	if err != nil {
		c.detail = err.Error()
		return c
	}
	if len(snaps) == 0 {
		c.detail = "пока нет ни одной"
		c.hint = "точка появится перед следующим запросом к Claude Code (нужны хуки); вручную: holdfast guard start"
		return c
	}
	c.ok = true
	c.detail = fmt.Sprintf("%d, последняя %s", len(snaps), humanAgo(now.Sub(snaps[0].Time)))
	return c
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out) // сортировка на месте, пакет slices — с Go 1.21

	return out
}
