// Package config loads and saves holdfast.toml.
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

// FileName is the name of the configuration file in the repository root.
const FileName = "holdfast.toml"

// Default values.
const (
	DefaultMaxTokens     = 2000
	DefaultMaxDeletes    = 5
	DefaultKeepSnapshots = 50
)

// DefaultLintFiles is which instruction files lint looks for when the config leaves it empty.
var DefaultLintFiles = []string{"CLAUDE.md", "AGENTS.md", ".cursorrules"}

// Config is the whole holdfast.toml.
//
// The `toml:"..."` tags are "struct tags": metadata strings that the toml
// library reads through reflection to learn the key name in the file.
type Config struct {
	Lint  LintConfig  `toml:"lint"`
	Guard GuardConfig `toml:"guard"`
}

// LintConfig is the [lint] section.
type LintConfig struct {
	Files     []string `toml:"files"`
	MaxTokens int      `toml:"max_tokens"`
	Disable   []string `toml:"disable"`
}

// Guard modes.
const (
	// ModeAutonomous: the agent works without stopping. Irreversible actions
	// are denied with a hint on how to do it properly, reversible ones
	// (covered by a snapshot) are allowed. "Ask" happens only where the
	// user asked for it themselves (ask_commands, protected_paths).
	ModeAutonomous = "autonomous"
	// ModeStrict is the original behavior: dangerous but reversible actions
	// need a human's confirmation.
	ModeStrict = "strict"
)

// DefaultProtectedBranches is the branches the agent may not push to.
var DefaultProtectedBranches = []string{"main", "master"}

// DefaultDisposablePaths is regenerable directories and files: they may be
// deleted in bulk even if git ignores them and they won't be in a restore point.
// A pattern without '/' is compared with the name of every directory or file on the path.
var DefaultDisposablePaths = []string{
	"node_modules", "dist", "build", "out", "target", "coverage", "tmp",
	".next", ".nuxt", ".turbo", ".cache", ".parcel-cache",
	"__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache", ".tox", ".venv", "venv",
	"*.pyc", "*.o", "*.class", "*.log", ".DS_Store",
}

// GuardConfig is the [guard] section.
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

// ContextNote is a hint the hook shows to the agent at the moment it touches
// files from Paths. That way the rule reaches the agent on time instead of
// sitting in a long CLAUDE.md.
type ContextNote struct {
	Paths []string `toml:"paths"`
	Note  string   `toml:"note"`
}

// Strict reports whether strict mode is on.
func (g *GuardConfig) Strict() bool { return g.Mode == ModeStrict }

// Normalize fills in the defaults and validates the mode.
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

// SyncedConfig is the [guard.synced] section; holdfast sync writes all of it.
type SyncedConfig struct {
	Rules []SyncedRule `toml:"rules"`
}

// Kinds of rules that sync carries over from CLAUDE.md into the policy.
const (
	KindDenyPath    = "deny-path"
	KindAskPath     = "ask-path"
	KindDenyCommand = "deny-command"
	KindAskCommand  = "ask-command"
)

// SyncedRule is one rule extracted from CLAUDE.md.
type SyncedRule struct {
	Kind    string `toml:"kind"`
	Pattern string `toml:"pattern"`
	Source  string `toml:"source"` // "CLAUDE.md:12", the source line
	Origin  string `toml:"origin"` // "explicit" or "heuristic"
}

// Default returns a config with the default values.
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

// Load reads root/holdfast.toml. If there is no file it returns Default()
// without an error: the config is optional.
func Load(root string) (*Config, error) {
	cfg := Default()
	path := filepath.Join(root, FileName)
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		// errors.Is knows how to "unwrap" wrapped errors and compare them
		// with the sentinel fs.ErrNotExist.
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

// LoadGuard reads only the [guard] section: that is what the hook needs,
// which has to be as fast as possible and touch nothing extra.
func LoadGuard(root string) (*GuardConfig, error) {
	// An anonymous struct with a single field: the decoder skips the
	// other sections without creating objects for them.
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

// IsDisabled reports whether a lint rule is disabled in the config.
func (c *Config) IsDisabled(ruleID string) bool {
	for _, id := range c.Lint.Disable {
		if strings.EqualFold(strings.TrimSpace(id), ruleID) {
			return true
		}
	}
	return false
}

// Template is the content that holdfast init writes.
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

// WriteTemplate creates holdfast.toml. Without force it does not overwrite the file.
func WriteTemplate(root string, force bool) (string, error) {
	path := filepath.Join(root, FileName)
	if !force {
		if _, err := os.Stat(path); err == nil {
			return path, fmt.Errorf("%s already exists (use --force)", FileName)
		}
	}
	return path, os.WriteFile(path, []byte(Template), 0o644)
}
