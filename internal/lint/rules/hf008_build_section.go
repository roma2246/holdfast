package rules

import (
	"regexp"
	"strings"

	"github.com/roma2246/holdfast/internal/lint"
)

func init() { lint.Register(buildSectionRule{}) }

type buildSectionRule struct{}

func (buildSectionRule) ID() string { return "HF008" }
func (buildSectionRule) Description() string {
	return "No section with build and test commands"
}

var buildHeadingRe = regexp.MustCompile(`(?i)build|test|command|script|develop|run|сбор|тест|команд|запуск|разработ`)

func (r buildSectionRule) Check(doc *lint.Doc, _ *lint.Repo) []lint.Finding {
	for _, s := range doc.Sections {
		if buildHeadingRe.MatchString(s.Title) {
			return nil
		}
	}
	// Without headings (for example .cursorrules) a code block or a
	// `command` with the word build/test is enough.
	if len(doc.Sections) == 0 {
		for i, line := range doc.Lines {
			l := strings.ToLower(line)
			if doc.InCode(i+1) || (strings.Contains(line, "`") && (strings.Contains(l, "test") || strings.Contains(l, "build"))) {
				return nil
			}
		}
	}
	return []lint.Finding{lint.F(r.ID(), lint.Info, doc, 1,
		"no section with build and test commands",
		"add \"## Commands\" with the exact build / test / lint commands: the agent will use them to check its own work")}
}
