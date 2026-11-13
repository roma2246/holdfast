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
	DefaultMaxTokens  = 2000
	DefaultMaxDeletes = 5
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

// GuardConfig — секция [guard].
type GuardConfig struct {
	Mode              string        `toml:"mode"`
	ProtectedPaths    []string      `toml:"protected_paths"`
	ProtectedBranches []string      `toml:"protected_branches"`
	DenyCommands      []string      `toml:"deny_commands"`
	AskCommands       []string      `toml:"ask_commands"`
	MaxDeletes        int           `toml:"max_deletes"`
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
	if g.ProtectedBranches == nil {
		g.ProtectedBranches = append([]string(nil), DefaultProtectedBranches...)
	}
	switch g.Mode {
	case "":
		g.Mode = ModeAutonomous
	case ModeAutonomous, ModeStrict:
	default:
		return fmt.Errorf("guard.mode: неизвестный режим %q (autonomous или strict)", g.Mode)
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
		return nil, fmt.Errorf("чтение %s: %w", path, err)
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
const Template = `# Конфигурация Holdfast. Документация: README.md

[lint]
# Файлы инструкций агента, которые проверяет holdfast lint.
files = ["CLAUDE.md", "AGENTS.md", ".cursorrules"]
# Лимит размера файла (токены считаются как символы / 4).
max_tokens = 2000
# Выключенные правила, например ["HF007"].
disable = []

[guard]
# autonomous — агент работает без остановок: необратимое запрещается
#   с подсказкой, как сделать правильно, обратимое разрешается
#   (откат: holdfast guard rollback);
# strict — опасное, но обратимое требует вашего подтверждения.
mode = "autonomous"
# Ветки, push в которые агенту запрещён (в остальные — можно).
protected_branches = ["main", "master"]
# Правки в этих путях требуют вашего подтверждения (glob, ** — любая глубина).
protected_paths = []
# Команды, которые всегда запрещены (сверх встроенной политики).
deny_commands = []
# Команды, перед которыми агент должен спросить вас (в любом режиме).
ask_commands = []
# Сколько файлов можно удалить одной командой без снапшота.
max_deletes = 5

# Подсказки агенту в момент, когда он трогает эти файлы.
# Так правила не раздувают CLAUDE.md и приходят вовремя.
# [[guard.context]]
# paths = ["migrations/**"]
# note = "Существующие миграции не меняем: добавь новый файл 00N_описание.sql"
`

// WriteTemplate создаёт holdfast.toml. Без force не перезаписывает файл.
func WriteTemplate(root string, force bool) (string, error) {
	path := filepath.Join(root, FileName)
	if !force {
		if _, err := os.Stat(path); err == nil {
			return path, fmt.Errorf("%s уже существует (используйте --force)", FileName)
		}
	}
	return path, os.WriteFile(path, []byte(Template), 0o644)
}
