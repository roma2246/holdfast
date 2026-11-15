// Package config загружает и сохраняет holdfast.toml.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// FileName — имя конфигурационного файла в корне репозитория.
const FileName = "holdfast.toml"

// Значения по умолчанию.
const (
	DefaultMaxTokens     = 2000
	DefaultMaxDeletes    = 5
	DefaultKeepSnapshots = 50
)

// DefaultLintFiles — какие файлы инструкций ищет lint, если в конфиге пусто.
var DefaultLintFiles = []string{"CLAUDE.md", "AGENTS.md", ".cursorrules"}

// Config — весь holdfast.toml.
//
// Теги `toml:"..."` — это «struct tags»: строки-метаданные, которые
// библиотека toml читает через рефлексию, чтобы знать имя ключа в файле.
type Config struct {
	Lint  LintConfig  `toml:"lint"`
	Guard GuardConfig `toml:"guard"`
}

// LintConfig — секция [lint].
type LintConfig struct {
	Files     []string `toml:"files"`
	MaxTokens int      `toml:"max_tokens"`
	Disable   []string `toml:"disable"`
}

// Режимы guard.
const (
	// ModeAutonomous — агент работает без остановок: необратимое
	// запрещается с подсказкой, как сделать правильно, обратимое
	// (под защитой снапшота) разрешается. «Спросить» — только там,
	// где пользователь сам попросил (ask_commands, protected_paths).
	ModeAutonomous = "autonomous"
	// ModeStrict — поведение из исходного ТЗ: опасное, но обратимое
	// требует подтверждения человека.
	ModeStrict = "strict"
)

// DefaultProtectedBranches — ветки, push в которые агенту запрещён.
var DefaultProtectedBranches = []string{"main", "master"}

// DefaultDisposablePaths — пересоздаваемые папки и файлы: их можно удалять
// массово, даже если git их игнорирует и в точку восстановления они не попадут.
// Шаблон без '/' сравнивается с именем любой папки или файла на пути.
var DefaultDisposablePaths = []string{
	"node_modules", "dist", "build", "out", "target", "coverage", "tmp",
	".next", ".nuxt", ".turbo", ".cache", ".parcel-cache",
	"__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache", ".tox", ".venv", "venv",
	"*.pyc", "*.o", "*.class", "*.log", ".DS_Store",
}

// GuardConfig — секция [guard].
type GuardConfig struct {
	Mode              string        `toml:"mode"`
	ProtectedPaths    []string      `toml:"protected_paths"`
	ProtectedBranches []string      `toml:"protected_branches"`
	DenyCommands      []string      `toml:"deny_commands"`
	AskCommands       []string      `toml:"ask_commands"`
	MaxDeletes        int           `toml:"max_deletes"`
	KeepSnapshots     int           `toml:"keep_snapshots"`
	DisposablePaths   []string      `toml:"disposable_paths"`
	Context           []ContextNote `toml:"context"`
	Synced            SyncedConfig  `toml:"synced"`
}

// ContextNote — подсказка, которую хук показывает агенту в момент, когда
// тот трогает файлы из Paths. Так правило попадает к агенту вовремя, а
// не лежит в длинном CLAUDE.md.
type ContextNote struct {
	Paths []string `toml:"paths"`
	Note  string   `toml:"note"`
}

// Strict — включён ли строгий режим.
func (g *GuardConfig) Strict() bool { return g.Mode == ModeStrict }

// Normalize подставляет значения по умолчанию и проверяет режим.
func (g *GuardConfig) Normalize() error {
	if g.MaxDeletes <= 0 {
		g.MaxDeletes = DefaultMaxDeletes
	}
	if g.KeepSnapshots <= 0 {
		g.KeepSnapshots = DefaultKeepSnapshots
	}
	if g.ProtectedBranches == nil {
		g.ProtectedBranches = append([]string(nil), DefaultProtectedBranches...)
	}
	if g.DisposablePaths == nil {
		g.DisposablePaths = append([]string(nil), DefaultDisposablePaths...)
	}
	switch g.Mode {
	case "":
		g.Mode = ModeAutonomous
	case ModeAutonomous, ModeStrict:
	default:
		return fmt.Errorf("guard.mode: unknown mode %q (autonomous or strict)", g.Mode)
	}
	return nil
}

// SyncedConfig — секция [guard.synced], её целиком пишет holdfast sync.
type SyncedConfig struct {
	Rules []SyncedRule `toml:"rules"`
}

// Виды правил, которые sync переносит из CLAUDE.md в политику.
const (
	KindDenyPath    = "deny-path"
	KindAskPath     = "ask-path"
	KindDenyCommand = "deny-command"
	KindAskCommand  = "ask-command"
)

// SyncedRule — одно правило, извлечённое из CLAUDE.md.
type SyncedRule struct {
	Kind    string `toml:"kind"`
	Pattern string `toml:"pattern"`
	Source  string `toml:"source"` // "CLAUDE.md:12" — строка-источник
	Origin  string `toml:"origin"` // "explicit" или "heuristic"
}

// Default возвращает конфиг со значениями по умолчанию.
func Default() *Config {
	return &Config{
		Lint: LintConfig{
			Files:     append([]string(nil), DefaultLintFiles...),
			MaxTokens: DefaultMaxTokens,
		},
		Guard: GuardConfig{
			Mode:              ModeAutonomous,
			MaxDeletes:        DefaultMaxDeletes,
			KeepSnapshots:     DefaultKeepSnapshots,
			DisposablePaths:   append([]string(nil), DefaultDisposablePaths...),
			ProtectedBranches: append([]string(nil), DefaultProtectedBranches...),
		},
	}
}

// Load читает root/holdfast.toml. Если файла нет — возвращает Default()
// без ошибки: конфиг необязателен.
func Load(root string) (*Config, error) {
	cfg := Default()
	path := filepath.Join(root, FileName)
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		// errors.Is умеет «разворачивать» обёрнутые ошибки и сравнивать
		// с эталонной fs.ErrNotExist.
		if errors.Is(err, fs.ErrNotExist) {
			return Default(), nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := cfg.normalize(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// LoadGuard читает только секцию [guard] — это нужно хуку, который
// должен работать максимально быстро и не трогать лишнее.
func LoadGuard(root string) (*GuardConfig, error) {
	// Анонимная структура только с одним полем: декодер пропустит
	// остальные секции, не создавая для них объектов.
	var partial struct {
		Guard GuardConfig `toml:"guard"`
	}
	_, err := toml.DecodeFile(filepath.Join(root, FileName), &partial)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := partial.Guard.Normalize(); err != nil {
		return nil, err
	}
	return &partial.Guard, nil
}

func (c *Config) normalize() error {
	if len(c.Lint.Files) == 0 {
		c.Lint.Files = append([]string(nil), DefaultLintFiles...)
	}
	if c.Lint.MaxTokens <= 0 {
		c.Lint.MaxTokens = DefaultMaxTokens
	}
	return c.Guard.Normalize()
}

// IsDisabled сообщает, выключено ли правило lint в конфиге.
func (c *Config) IsDisabled(ruleID string) bool {
	for _, id := range c.Lint.Disable {
		if strings.EqualFold(strings.TrimSpace(id), ruleID) {
			return true
		}
	}
	return false
}

// Template — содержимое, которое пишет holdfast init.
const Template = `# Holdfast configuration. Documentation: README.md

[lint]
# Agent instruction files that holdfast lint checks.
files = ["CLAUDE.md", "AGENTS.md", ".cursorrules"]
# File size limit (tokens are counted as characters / 4).
max_tokens = 2000
# Disabled rules, for example ["HF007"].
disable = []

[guard]
# autonomous: the agent works without stopping. Irreversible actions are denied
#   with a hint on how to do it properly, reversible ones are allowed
#   (rollback: holdfast guard rollback);
# strict: dangerous but reversible actions need your confirmation.
mode = "autonomous"
# Branches the agent may not push to (any other branch is fine).
protected_branches = ["main", "master"]
# Edits under these paths need your confirmation (glob, ** matches any depth).
protected_paths = []
# Commands that are always denied (on top of the built-in policy).
deny_commands = []
# Commands the agent has to ask you about first (in any mode).
ask_commands = []
# How many files one command may delete without a restore point. Above that,
# the hook first takes a restore point itself and verifies the files are in it.
max_deletes = 5
# Regenerable directories and files: they may be deleted in bulk even if git
# ignores them (they are not included in restore points). The defaults are
# node_modules, dist, build, target, __pycache__, .venv, *.pyc, *.log and the like.
# disposable_paths = ["node_modules", "dist", "build"]
# How many recent restore points to keep (holdfast guard list).
keep_snapshots = 50

# Hints for the agent at the moment it touches these files.
# This keeps the rules out of CLAUDE.md and delivers them right on time.
# [[guard.context]]
# paths = ["migrations/**"]
# note = "Don't change existing migrations: add a new file 00N_description.sql"
`

// WriteTemplate создаёт holdfast.toml. Без force не перезаписывает файл.
func WriteTemplate(root string, force bool) (string, error) {
	path := filepath.Join(root, FileName)
	if !force {
		if _, err := os.Stat(path); err == nil {
			return path, fmt.Errorf("%s already exists (use --force)", FileName)
		}
	}
	return path, os.WriteFile(path, []byte(Template), 0o644)
}
