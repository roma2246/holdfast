// Package rules holds the lint rules: one rule per file.
// Every rule registers itself in init(), so importing the package is
// enough: import _ ".../lint/rules".
package rules

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/roma2246/holdfast/internal/lint"
)

// init() is called automatically when the package is loaded, before main().
func init() { lint.Register(pathsRule{}) }

type pathsRule struct{}

func (pathsRule) ID() string          { return "HF001" }
func (pathsRule) Description() string { return "A path in the text does not exist" }

var (
	// A path candidate: segments separated by '/', with ./ or ../ allowed at the start.
	pathTokenRe = regexp.MustCompile(`(?:\.{1,2}/)?[A-Za-z0-9_.\-]+(?:/[A-Za-z0-9_.\-]+)+/?|(?:\.{1,2}/)?[A-Za-z0-9_\-][A-Za-z0-9_.\-]*\.[A-Za-z0-9]{1,8}`)
	// Extensions by which a single word is treated as a file name.
	fileExtRe = regexp.MustCompile(`\.(md|mdx|go|mod|sum|ts|tsx|js|jsx|mjs|cjs|json|ya?ml|toml|py|rb|rs|java|kt|swift|c|h|cpp|hpp|cs|php|sh|sql|txt|html|css|scss|vue|svelte|lock|env|ini|cfg|conf|proto|graphql|xml|csv)$`)
	domainRe  = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)*\.(com|org|net|io|dev|cc|in|sh|ai|app|co|me|xyz|edu|gov|ru)$`)
	versionRe = regexp.MustCompile(`^v?\d+(\.\d+)+$`)
	// Node.js, Next.js, Vue.js are technology names, not files.
	techNameRe = regexp.MustCompile(`^[A-Z][A-Za-z]*\.js$`)
	// Placeholder patterns: HFxxx, hfNNN_name.go, YYYY-MM-DD.md.
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
		// Inside `code` any path is taken; in plain text only the "obviously
		// path-like" ones: with a file extension, a leading ./ or a trailing /.
		// That way "and/or" and "TCP/IP" are not treated as paths.
		var cands []string
		for _, code := range lint.InlineCode(line) {
			if strings.ContainsAny(code, " \t") {
				continue // this is a command, not a path
			}
			cands = append(cands, pathTokenRe.FindAllString(code, -1)...)
		}
		for _, tok := range pathTokenRe.FindAllString(stripLinksAndURLs(lint.StripInlineCode(line)), -1) {
			tok = strings.TrimRight(tok, ".,:;") // the period at the end of a sentence
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
					fmt.Sprintf("path \"%s\" does not exist", tok),
					"fix the path or remove the mention: the agent will waste time looking for a file that doesn't exist"))
			}
		}
	}
	return out
}

var linkURLRe = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://\S+|\]\([^)]*\)|\S+@\S+`)

// stripLinksAndURLs removes URLs, markdown link targets and email addresses.
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
	// github.com/user/repo, mvdan.cc/sh are modules/domains, not paths.
	if domainRe.MatchString(strings.ToLower(first)) {
		return false
	}
	// A single word without '/' counts only if it looks like a file (README.md).
	if !strings.Contains(tok, "/") && !fileExtRe.MatchString(tok) {
		return false
	}
	return true
}

// pathExists looks for the path relative to the document's directory and the
// repository root. A bare file name ("policy.go") is searched for across the
// whole repository: text often mentions a file whose path is given nearby.
func pathExists(doc *lint.Doc, repo *lint.Repo, tok string) bool {
	rel := filepath.FromSlash(strings.TrimSuffix(tok, "/"))
	for _, base := range []string{doc.Dir, repo.Root} {
		if repo.Exists(filepath.Join(base, rel)) {
			return true
		}
	}
	return !strings.Contains(tok, "/") && repo.HasBaseName(tok)
}
