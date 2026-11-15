package rules

import (
	"fmt"

	"github.com/roma2246/holdfast/internal/bridge"
	"github.com/roma2246/holdfast/internal/lint"
)

func init() { lint.Register(unsyncedRule{}) }

type unsyncedRule struct{}

func (unsyncedRule) ID() string { return "HF009" }
func (unsyncedRule) Description() string {
	return "The instructions contain a prohibition that is not in the guard policy"
}

func (r unsyncedRule) Check(doc *lint.Doc, repo *lint.Repo) []lint.Finding {
	var out []lint.Finding
	for _, rule := range bridge.Dedupe(bridge.Extract(doc.Path, doc.Lines)) {
		if bridge.Covered(rule, &repo.Config.Guard) {
			continue
		}
		out = append(out, lint.F(r.ID(), lint.Warning, doc, rule.Line,
			fmt.Sprintf("the prohibition \"%s\" is not reflected in the guard policy", rule.Text),
			"run holdfast sync: then the hook enforces this rule instead of relying on the agent"))
	}
	return out
}
