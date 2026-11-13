package rules

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/roma2246/holdfast/internal/lint"
)

func init() { lint.Register(secretsRule{}) }

type secretsRule struct{}

func (secretsRule) ID() string { return "HF006" }
func (secretsRule) Description() string {
	return "Похоже на секрет: API-ключ, токен, пароль"
}

type secretPattern struct {
	name string
	re   *regexp.Regexp
	// group — номер группы со значением секрета (0 — всё совпадение).
	group int
}

var secretPatterns = []secretPattern{
	{"приватный ключ", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`), 0},
	{"AWS access key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), 0},
	{"GitHub token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{40,})\b`), 0},
	{"Anthropic/OpenAI key", regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_\-]{20,}`), 0},
	{"Slack token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`), 0},
	{"Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`), 0},
	{"Stripe key", regexp.MustCompile(`\b[sr]k_live_[0-9A-Za-z]{20,}\b`), 0},
	{"JWT", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`), 0},
	// password = "..." / API_KEY: ... / пароль: ...
	{"пароль или ключ", regexp.MustCompile(`(?i)\b(?:api[_-]?key|secret(?:[_-]?key)?|access[_-]?token|auth[_-]?token|token|password|passwd|pwd|пароль)\b["']?\s*[:=]\s*["']?([^\s"'` + "`" + `,;]{8,})`), 1},
}

// Значения-заглушки, которые не считаем секретами.
var placeholderRe = regexp.MustCompile(`(?i)^(?:<.*>|\$\{?.*|x{4,}.*|\*{3,}.*|your[_-].*|example.*|changeme|dummy.*|test.*|placeholder.*|\.\.\..*|redacted|todo)$`)

func (r secretsRule) Check(doc *lint.Doc, _ *lint.Repo) []lint.Finding {
	var out []lint.Finding
	for i, line := range doc.Lines {
		for _, p := range secretPatterns {
			m := p.re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			value := m[p.group]
			if placeholderRe.MatchString(value) {
				continue
			}
			out = append(out, lint.F(r.ID(), lint.Error, doc, i+1,
				fmt.Sprintf("похоже на секрет (%s): %s", p.name, maskSecret(value)),
				"удалите секрет из файла и перевыпустите его; агенту давайте имя переменной окружения, а не значение"))
			break // одной находки на строку достаточно
		}
	}
	return out
}

// maskSecret оставляет первые 4 символа, чтобы отчёт сам не стал утечкой.
func maskSecret(s string) string {
	if len(s) <= 4 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + strings.Repeat("*", min(len(s)-4, 12))
}
