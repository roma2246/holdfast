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
	return "В инструкции есть запрет, которого нет в политике guard"
}

func (r unsyncedRule) Check(doc *lint.Doc, repo *lint.Repo) []lint.Finding {
	var out []lint.Finding
	for _, rule := range bridge.Dedupe(bridge.Extract(doc.Path, doc.Lines)) {
		if bridge.Covered(rule, &repo.Config.Guard) {
			continue
		}
		out = append(out, lint.F(r.ID(), lint.Warning, doc, rule.Line,
			fmt.Sprintf("запрет «%s» не отражён в политике guard", rule.Text),
			"запустите holdfast sync — тогда хук будет проверять это правило, а не надеяться на агента"))
	}
	return out
}
