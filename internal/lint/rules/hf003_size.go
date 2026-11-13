package rules

import (
	"fmt"
	"unicode/utf8"

	"github.com/roma2246/holdfast/internal/lint"
)

func init() { lint.Register(sizeRule{}) }

type sizeRule struct{}

func (sizeRule) ID() string          { return "HF003" }
func (sizeRule) Description() string { return "Файл больше лимита токенов" }

// EstimateTokens — грубая оценка: символы / 4 (без вызова токенизатора).
func EstimateTokens(text string) int {
	// utf8.RuneCountInString считает символы, а не байты:
	// в UTF-8 русская буква занимает 2 байта.
	return utf8.RuneCountInString(text) / 4
}

func (r sizeRule) Check(doc *lint.Doc, repo *lint.Repo) []lint.Finding {
	limit := repo.Config.Lint.MaxTokens
	tokens := EstimateTokens(doc.Text)
	if tokens <= limit {
		return nil
	}
	return []lint.Finding{lint.F(r.ID(), lint.Warning, doc, 1,
		fmt.Sprintf("файл ~%d токенов, лимит %d", tokens, limit),
		"каждый токен инструкции съедает контекст агента; правила для отдельных папок вынесите в [[guard.context]] holdfast.toml — хук покажет их агенту в момент работы с этими файлами")}
}
