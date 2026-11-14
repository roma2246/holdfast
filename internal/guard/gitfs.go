package guard

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Хук должен отвечать за миллисекунды, а запуск `git` занимает 5–20 мс.
// Поэтому нужные хуку факты (текущая ветка, есть ли снапшот, есть ли тег)
// читаем прямо из файлов в .git — это доли миллисекунды.

// gitDir возвращает папку .git репозитория root. В worktree и сабмодулях
// .git — это файл со строкой "gitdir: <путь>".
func gitDir(root string) string {
	p := filepath.Join(root, ".git")
	info, err := os.Stat(p)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return p
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	dir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir:"))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	return dir
}

// findGitRoot поднимается от dir вверх до папки, где есть .git.
// "" — dir не внутри git-репозитория.
func findGitRoot(dir string) string {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

// commonDir — общая папка репозитория (для worktree refs лежат там).
func commonDir(gd string) string {
	data, err := os.ReadFile(filepath.Join(gd, "commondir"))
	if err != nil {
		return gd
	}
	dir := strings.TrimSpace(string(data))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(gd, dir)
	}
	return filepath.Clean(dir)
}

// currentBranch — имя текущей ветки; "" — detached HEAD или не git.
func currentBranch(root string) string {
	gd := gitDir(root)
	if gd == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(gd, "HEAD"))
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(data))
	if !strings.HasPrefix(head, "ref: refs/heads/") {
		return ""
	}
	return strings.TrimPrefix(head, "ref: refs/heads/")
}

// refExists ищет ссылку и в виде файла, и в packed-refs.
func refExists(root, ref string) bool {
	gd := gitDir(root)
	if gd == "" {
		return false
	}
	cd := commonDir(gd)
	if _, err := os.Stat(filepath.Join(cd, filepath.FromSlash(ref))); err == nil {
		return true
	}
	return packedRefsHas(cd, func(name string) bool { return name == ref })
}

// hasSnapshot — есть ли хотя бы один снапшот holdfast (refs/holdfast/*).
func hasSnapshot(root string) bool {
	gd := gitDir(root)
	if gd == "" {
		return false
	}
	cd := commonDir(gd)
	entries, err := os.ReadDir(filepath.Join(cd, "refs", "holdfast"))
	if err == nil && len(entries) > 0 {
		return true
	}
	return packedRefsHas(cd, func(name string) bool { return strings.HasPrefix(name, SnapshotRefPrefix) })
}

func packedRefsHas(commonDir string, match func(string) bool) bool {
	f, err := os.Open(filepath.Join(commonDir, "packed-refs"))
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// Формат строки: "<sha> <refname>".
		if _, name, ok := strings.Cut(sc.Text(), " "); ok && match(name) {
			return true
		}
	}
	return false
}
