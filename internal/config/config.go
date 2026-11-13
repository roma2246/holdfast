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

// GuardConfig — секция [guard].
type GuardConfig struct {
	ProtectedPaths []string     `toml:"protected_paths"`
	DenyCommands   []string     `toml:"deny_commands"`
	AskCommands    []string     `toml:"ask_commands"`
	MaxDeletes     int          `toml:"max_deletes"`
	Synced         SyncedConfig `toml:"synced"`
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
		Guard: GuardConfig{MaxDeletes: DefaultMaxDeletes},
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
	cfg.normalize()
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
	partial.Guard.MaxDeletes = DefaultMaxDeletes
	_, err := toml.DecodeFile(filepath.Join(root, FileName), &partial)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if partial.Guard.MaxDeletes <= 0 {
		partial.Guard.MaxDeletes = DefaultMaxDeletes
	}
	return &partial.Guard, nil
}

func (c *Config) normalize() {
	if len(c.Lint.Files) == 0 {
		c.Lint.Files = append([]string(nil), DefaultLintFiles...)
	}
	if c.Lint.MaxTokens <= 0 {
		c.Lint.MaxTokens = DefaultMaxTokens
	}
	if c.Guard.MaxDeletes <= 0 {
		c.Guard.MaxDeletes = DefaultMaxDeletes
	}
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
# Правки в этих путях требуют подтверждения (glob, ** — любая глубина).
protected_paths = []
# Команды, которые всегда запрещены (сверх встроенной политики).
deny_commands = ["git push --force"]
# Команды, перед которыми агент должен спросить разрешения.
ask_commands = ["git push", "npm publish"]
# Удаление большего числа файлов одной командой требует подтверждения.
max_deletes = 5
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
