package lint

import (
	"bufio"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/roma2246/holdfast/internal/config"
)

// Repo — контекст репозитория, общий для всех файлов: корень, конфиг и
// кэш разобранных package.json / Makefile / justfile.
//
// Файлы проверяются параллельно, поэтому кэш защищён мьютексом.
type Repo struct {
	Root   string
	Config *config.Config

	mu    sync.Mutex
	cache map[string]map[string]bool // путь к файлу → множество имён

	// sync.Once гарантирует, что индекс имён построится ровно один раз,
	// даже если его одновременно запросят несколько горутин.
	namesOnce sync.Once
	baseNames map[string]bool
}

// maxIndexedFiles ограничивает обход огромных репозиториев.
const maxIndexedFiles = 100_000

// HasBaseName — есть ли где-нибудь в репозитории файл или папка с таким
// именем. Нужен для упоминаний вроде «policy.go» без пути.
func (r *Repo) HasBaseName(name string) bool {
	r.namesOnce.Do(func() {
		r.baseNames = map[string]bool{}
		n := 0
		_ = filepath.WalkDir(r.Root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && path != r.Root && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			r.baseNames[d.Name()] = true
			if n++; n > maxIndexedFiles {
				return filepath.SkipAll
			}
			return nil
		})
	})
	return r.baseNames[name]
}

// NewRepo создаёт контекст репозитория.
func NewRepo(root string, cfg *config.Config) *Repo {
	if cfg == nil {
		cfg = config.Default()
	}
	return &Repo{Root: root, Config: cfg, cache: map[string]map[string]bool{}}
}

// Exists проверяет, существует ли путь (абсолютный).
func (r *Repo) Exists(abs string) bool {
	_, err := os.Stat(abs)
	return err == nil
}

// Kind — источник команд для HF002.
type Kind int

const (
	KindNPM Kind = iota
	KindMake
	KindJust
)

// Names возвращает множество скриптов/целей для ближайшего к dir файла
// нужного типа (поднимаясь до корня репозитория). found=false — файла нет.
func (r *Repo) Names(kind Kind, dir string) (names map[string]bool, file string, found bool) {
	candidates := map[Kind][]string{
		KindNPM:  {"package.json"},
		KindMake: {"Makefile", "makefile", "GNUmakefile"},
		KindJust: {"justfile", "Justfile", ".justfile"},
	}[kind]

	for d := dir; ; d = filepath.Dir(d) {
		for _, c := range candidates {
			p := filepath.Join(d, c)
			if r.Exists(p) {
				return r.load(kind, p), p, true
			}
		}
		// Не выходим за пределы корня репозитория.
		if d == r.Root || d == filepath.Dir(d) || !strings.HasPrefix(d, r.Root) {
			break
		}
	}
	return nil, "", false
}

func (r *Repo) load(kind Kind, path string) map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.cache[path]; ok {
		return m
	}
	var m map[string]bool
	switch kind {
	case KindNPM:
		m = parsePackageJSON(path)
	case KindMake:
		m = parseTargets(path, makeTargetRe)
	case KindJust:
		m = parseTargets(path, justRecipeRe)
	}
	r.cache[path] = m
	return m
}

func parsePackageJSON(path string) map[string]bool {
	out := map[string]bool{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &pkg) == nil {
		for k := range pkg.Scripts {
			out[k] = true
		}
	}
	return out
}

var (
	// "build test: deps" — цели до двоеточия; исключаем присваивания ":=" и "::=".
	makeTargetRe = regexp.MustCompile(`^([A-Za-z0-9_./%-][A-Za-z0-9_./% -]*?)\s*::?(?:[^=]|$)`)
	// "@build arg='x': deps" — имя рецепта justfile, затем параметры.
	justRecipeRe = regexp.MustCompile(`^@?([A-Za-z0-9_-]+)(?:\s+[^:=]*)?:(?:[^=]|$)`)
)

func parseTargets(path string, re *regexp.Regexp) map[string]bool {
	out := map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "\t") || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "#") {
			continue
		}
		if m := re.FindStringSubmatch(line); m != nil {
			for _, name := range strings.Fields(m[1]) {
				out[name] = true
			}
		}
	}
	return out
}
