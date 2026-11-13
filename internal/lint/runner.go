package lint

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sync/errgroup"
)

// skipDirs — папки, в которые обход не заходит.
var skipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "testdata": true,
	"dist": true, "build": true, "target": true,
}

// Discover находит файлы инструкций (CLAUDE.md, AGENTS.md, ...) в корне и
// вложенных папках. Скрытые папки (.git, .holdfast) пропускаются.
func Discover(root string, names []string) ([]string, error) {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var out []string
	// WalkDir вызывает функцию для каждого файла и папки в дереве.
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // нечитаемые папки просто пропускаем
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (skipDirs[name] || (len(name) > 1 && name[0] == '.')) {
				return filepath.SkipDir
			}
			return nil
		}
		if want[d.Name()] {
			out = append(out, path)
		}
		return nil
	})
	return out, err
}

// Run проверяет файлы files всеми правилами rules и возвращает
// отсортированные находки.
func Run(ctx context.Context, repo *Repo, files []string, rules []Rule) ([]Finding, error) {
	results := make([][]Finding, len(files))

	// errgroup — группа горутин: Wait() ждёт их все и возвращает первую
	// ошибку. SetLimit ограничивает число одновременно работающих.
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for i, path := range files {
		// Каждая горутина пишет только в свою ячейку results[i],
		// поэтому мьютекс не нужен.
		g.Go(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			rel, err := filepath.Rel(repo.Root, path)
			if err != nil {
				rel = path
			}
			doc, err := ParseFile(path, filepath.ToSlash(rel))
			if err != nil {
				return err
			}
			results[i] = CheckDoc(doc, repo, rules)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	var all []Finding
	for _, r := range results {
		all = append(all, r...)
	}
	SortFindings(all)
	return all, nil
}

// CheckDoc применяет правила к одному документу с учётом выключателей.
func CheckDoc(doc *Doc, repo *Repo, rules []Rule) []Finding {
	var out []Finding
	for _, rule := range rules {
		if repo.Config.IsDisabled(rule.ID()) {
			continue
		}
		for _, f := range rule.Check(doc, repo) {
			if !doc.IsDisabled(f.RuleID, f.Line) {
				out = append(out, f)
			}
		}
	}
	return out
}

// ResolveTarget превращает аргумент PATH команды lint в корень репозитория
// и список файлов. Если PATH — файл, проверяется только он.
func ResolveTarget(path string) (root string, single string, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", "", err
	}
	if info.IsDir() {
		return abs, "", nil
	}
	return FindRoot(filepath.Dir(abs)), abs, nil
}

// FindRoot поднимается от dir вверх до папки с .git или holdfast.toml.
// Если такой нет — возвращает сам dir.
func FindRoot(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		for _, marker := range []string{".git", "holdfast.toml"} {
			if _, err := os.Stat(filepath.Join(d, marker)); err == nil {
				return d
			}
		}
		if filepath.Dir(d) == d {
			return dir
		}
	}
}
