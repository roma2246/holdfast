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

// Repo is the repository context shared by all files: the root, the config
// and a cache of parsed package.json / Makefile / justfile.
//
// Files are checked in parallel, so the cache is protected by a mutex.
type Repo struct {
	Root   string
	Config *config.Config

	mu    sync.Mutex
	cache map[string]map[string]bool // file path → set of names

	// sync.Once guarantees that the name index is built exactly once, even
	// if several goroutines ask for it at the same time.
	namesOnce sync.Once
	baseNames map[string]bool
}

// maxIndexedFiles limits the walk of huge repositories.
const maxIndexedFiles = 100_000

// HasBaseName reports whether the repository has a file or directory with
// this name anywhere. Needed for mentions like "policy.go" without a path.
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

// NewRepo creates a repository context.
func NewRepo(root string, cfg *config.Config) *Repo {
	if cfg == nil {
		cfg = config.Default()
	}
	return &Repo{Root: root, Config: cfg, cache: map[string]map[string]bool{}}
}

// Exists checks whether a path (absolute) exists.
func (r *Repo) Exists(abs string) bool {
	_, err := os.Stat(abs)
	return err == nil
}

// Kind is a source of commands for HF002.
type Kind int

const (
	KindNPM Kind = iota
	KindMake
	KindJust
)

// Names returns the set of scripts/targets for the file of the given kind
// nearest to dir (walking up to the repository root). found=false means there is no such file.
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
		// Don't go above the repository root.
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
	// "build test: deps": targets before the colon; ":=" and "::=" assignments are excluded.
	makeTargetRe = regexp.MustCompile(`^([A-Za-z0-9_./%-][A-Za-z0-9_./% -]*?)\s*::?(?:[^=]|$)`)
	// "@build arg='x': deps": the justfile recipe name, then parameters.
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
