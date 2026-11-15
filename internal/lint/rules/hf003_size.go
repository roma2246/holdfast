package rules

import (
	"fmt"
	"unicode/utf8"

	"github.com/roma2246/holdfast/internal/lint"
)

func init() { lint.Register(sizeRule{}) }

type sizeRule struct{}

func (sizeRule) ID() string          { return "HF003" }
func (sizeRule) Description() string { return "The file exceeds the token limit" }

// EstimateTokens is a rough estimate: characters / 4 (no tokenizer call).
func EstimateTokens(text string) int {
	// utf8.RuneCountInString counts characters, not bytes:
	// in UTF-8 a Cyrillic letter takes 2 bytes.
	return utf8.RuneCountInString(text) / 4
}

func (r sizeRule) Check(doc *lint.Doc, repo *lint.Repo) []lint.Finding {
	limit := repo.Config.Lint.MaxTokens
	tokens := EstimateTokens(doc.Text)
	if tokens <= limit {
		return nil
	}
	return []lint.Finding{lint.F(r.ID(), lint.Warning, doc, 1,
		fmt.Sprintf("the file is ~%d tokens, the limit is %d", tokens, limit),
		"every instruction token eats into the agent's context; move the rules for individual directories into [[guard.context]] in holdfast.toml: the hook shows them to the agent when it works with those files")}
}
