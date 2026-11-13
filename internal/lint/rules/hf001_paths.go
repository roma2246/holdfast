// Package rules содержит правила lint: одно правило — один файл.
// Каждое правило регистрирует себя в init(), поэтому достаточно
// импортировать пакет: import _ ".../lint/rules".
package rules

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/roma2246/holdfast/internal/lint"
)

// init() вызывается автоматически при загрузке пакета, до main().
func init() { lint.Register(pathsRule{}) }

type pathsRule struct{}

func (pathsRule) ID() string          { return "HF001" }
func (pathsRule) Description() string { return "Путь из текста не существует" }

var (
	// Кандидат в путь: сегменты через '/', допускается ./ или ../ в начале.
	pathTokenRe = regexp.MustCompile(`(?:\.{1,2}/)?[A-Za-z0-9_.\-]+(?:/[A-Za-z0-9_.\-]+)+/?|(?:\.{1,2}/)?[A-Za-z0-9_\-][A-Za-z0-9_.\-]*\.[A-Za-z0-9]{1,8}`)
	// Расширения, по которым одиночное слово считаем именем файла.
	fileExtRe = regexp.MustCompile(`\.(md|mdx|go|mod|sum|ts|tsx|js|jsx|mjs|cjs|json|ya?ml|toml|py|rb|rs|java|kt|swift|c|h|cpp|hpp|cs|php|sh|sql|txt|html|css|scss|vue|svelte|lock|env|ini|cfg|conf|proto|graphql|xml|csv)$`)
	domainRe  = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)*\.(com|org|net|io|dev|cc|in|sh|ai|app|co|me|xyz|edu|gov|ru)$`)
	versionRe = regexp.MustCompile(`^v?\d+(\.\d+)+$`)
	// Node.js, Next.js, Vue.js — названия технологий, а не файлы.
	techNameRe = regexp.MustCompile(`^[A-Z][A-Za-z]*\.js$`)
	// Шаблоны-заглушки: HFxxx, hfNNN_name.go, YYYY-MM-DD.md.
	placeholderPathRe = regexp.MustCompile(`xxx|XXX|NNN|YYYY|your[-_]|example[-_/.]`)
)

func (r pathsRule) Check(doc *lint.Doc, repo *lint.Repo) []lint.Finding {
	var out []lint.Finding
	seen := map[string]bool{}
	for i, line := range doc.Lines {
		n := i + 1
		if doc.InCode(n) || strings.Contains(line, "<!--") {
			continue
		}
		// Внутри `кода` берём любые пути, в обычном тексте — только
		// «явно путевые»: с расширением файла, ./ в начале или / в конце.
		// Так «and/or» и «TCP/IP» не считаются путями.
		var cands []string
		for _, code := range lint.InlineCode(line) {
			if strings.ContainsAny(code, " \t") {
				continue // это команда, а не путь
			}
			cands = append(cands, pathTokenRe.FindAllString(code, -1)...)
		}
		for _, tok := range pathTokenRe.FindAllString(stripLinksAndURLs(lint.StripInlineCode(line)), -1) {
			tok = strings.TrimRight(tok, ".,:;") // точка в конце предложения
			if strings.HasSuffix(tok, "/") || strings.HasPrefix(tok, "./") || fileExtRe.MatchString(tok) {
				cands = append(cands, tok)
			}
		}
		for _, tok := range cands {
			tok = strings.TrimRight(tok, ".,:;")
			if !looksLikePath(tok) || seen[tok] {
				continue
			}
			if !pathExists(doc, repo, tok) {
				seen[tok] = true
				out = append(out, lint.F(r.ID(), lint.Error, doc, n,
					fmt.Sprintf("путь «%s» не существует", tok),
					"исправьте путь или удалите упоминание — агент потратит время на поиск несуществующего файла"))
			}
		}
	}
	return out
}

var linkURLRe = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://\S+|\]\([^)]*\)|\S+@\S+`)

// stripLinksAndURLs убирает URL, цели markdown-ссылок и email-адреса.
func stripLinksAndURLs(s string) string {
	return linkURLRe.ReplaceAllString(s, " ")
}

func looksLikePath(tok string) bool {
	if tok == "" || strings.Contains(tok, "...") || strings.ContainsAny(tok, "*?{}$<>") {
		return false
	}
	if versionRe.MatchString(tok) || techNameRe.MatchString(tok) || placeholderPathRe.MatchString(tok) {
		return false
	}
	first := strings.SplitN(strings.TrimPrefix(strings.TrimPrefix(tok, "./"), "../"), "/", 2)[0]
	// github.com/user/repo, mvdan.cc/sh — это модули/домены, а не пути.
	if domainRe.MatchString(strings.ToLower(first)) {
		return false
	}
	// Одиночное слово без '/' — только если похоже на файл (README.md).
	if !strings.Contains(tok, "/") && !fileExtRe.MatchString(tok) {
		return false
	}
	return true
}

// pathExists ищет путь относительно папки документа и корня репозитория.
// Голое имя файла («policy.go») ищется по всему репозиторию: в тексте
// так часто упоминают файл, путь к которому дан рядом.
func pathExists(doc *lint.Doc, repo *lint.Repo, tok string) bool {
	rel := filepath.FromSlash(strings.TrimSuffix(tok, "/"))
	for _, base := range []string{doc.Dir, repo.Root} {
		if repo.Exists(filepath.Join(base, rel)) {
			return true
		}
	}
	return !strings.Contains(tok, "/") && repo.HasBaseName(tok)
}
