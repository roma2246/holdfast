package rules

import (
	"fmt"
	"strings"

	"github.com/roma2246/holdfast/internal/lint"
)

func init() { lint.Register(vagueRule{}) }

type vagueRule struct{}

func (vagueRule) ID() string { return "HF007" }
func (vagueRule) Description() string {
	return "Расплывчатые инструкции без конкретики"
}

// Фразы, которые агент и так «знает» и которые ничего не уточняют.
var vaguePhrases = []string{
	"write good code", "write clean code", "write quality code", "write high quality code",
	"write high-quality code", "write readable code", "write better code",
	"follow best practices", "use best practices", "follow good practices",
	"be careful", "use common sense", "make it good", "make it clean",
	"code should be clean", "keep the code clean", "do your best", "be smart",
	"пиши чисто", "пиши хороший код", "пиши чистый код", "пиши качественный код",
	"пишите чистый код", "пишите хороший код", "следуй лучшим практикам",
	"следуйте лучшим практикам", "лучшие практики", "будь аккуратен", "будь внимателен",
	"делай хорошо", "код должен быть чистым",
}

func (r vagueRule) Check(doc *lint.Doc, _ *lint.Repo) []lint.Finding {
	var out []lint.Finding
	for i, raw := range doc.Lines {
		if doc.InCode(i + 1) {
			continue
		}
		line := strings.ToLower(raw)
		for _, p := range vaguePhrases {
			if strings.Contains(line, p) {
				out = append(out, lint.F(r.ID(), lint.Info, doc, i+1,
					fmt.Sprintf("расплывчатая инструкция «%s»", p),
					"замените конкретикой: какой линтер, какой стиль, какие лимиты, пример"))
				break
			}
		}
	}
	return out
}
