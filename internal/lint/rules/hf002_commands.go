package rules

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/roma2246/holdfast/internal/lint"
)

func init() { lint.Register(commandsRule{}) }

type commandsRule struct{}

func (commandsRule) ID() string { return "HF002" }
func (commandsRule) Description() string {
	return "An npm run / make / just command is missing from package.json / Makefile / justfile"
}

// We look only in `code` and in ```...``` blocks: in plain text
// "make sure" is not a call to make.
var cmdRe = regexp.MustCompile(`(?:^|[\s;&|(])(npm run|pnpm run|yarn run|bun run|make|just)\s+((?:-{1,2}[A-Za-z][\w-]*(?:=\S+)?\s+)*)([A-Za-z0-9][A-Za-z0-9:_.\-/]*)`)

func (r commandsRule) Check(doc *lint.Doc, repo *lint.Repo) []lint.Finding {
	var out []lint.Finding
	for i, line := range doc.Lines {
		n := i + 1
		var snippets []string
		if doc.InCode(n) {
			snippets = []string{stripShellComment(line)}
		} else {
			snippets = lint.InlineCode(line)
		}
		for _, s := range snippets {
			for _, m := range cmdRe.FindAllStringSubmatch(s, -1) {
				tool, name := m[1], m[3]
				// VAR=value is a make variable, not a target.
				if strings.Contains(name, "=") || (tool == "make" && strings.HasPrefix(m[2], "-C")) {
					continue
				}
				if f, ok := r.check(doc, repo, n, tool, name); ok {
					out = append(out, f)
				}
			}
		}
	}
	return out
}

// stripShellComment cuts off a "# comment" in a line from a code block.
func stripShellComment(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t') {
			return s[:i]
		}
	}
	return s
}

func (r commandsRule) check(doc *lint.Doc, repo *lint.Repo, line int, tool, name string) (lint.Finding, bool) {
	kind := lint.KindNPM
	fileName := "package.json"
	what := "script"
	switch tool {
	case "make":
		kind, fileName, what = lint.KindMake, "Makefile", "target"
	case "just":
		kind, fileName, what = lint.KindJust, "justfile", "recipe"
	}
	cmd := tool + " " + name
	names, path, found := repo.Names(kind, doc.Dir)
	if !found {
		return lint.F(r.ID(), lint.Error, doc, line,
			fmt.Sprintf("\"%s\": %s not found", cmd, fileName),
			fmt.Sprintf("create %s or remove the command from the instructions", fileName)), true
	}
	if names[name] {
		return lint.Finding{}, false
	}
	rel, err := filepath.Rel(repo.Root, path)
	if err != nil {
		rel = path
	}
	return lint.F(r.ID(), lint.Error, doc, line,
		fmt.Sprintf("\"%s\": %s %s is not in %s", cmd, what, name, filepath.ToSlash(rel)),
		"the agent will run a command that doesn't exist; update the instructions"), true
}
