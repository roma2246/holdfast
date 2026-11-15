package guard

import (
	"os"
	"path/filepath"
	"strings"
)

// Where a `git push` without an explicit branch goes is decided by the git
// configuration: remote.<name>.push, push.default, the upstream
// (branch.<branch>.merge), remote.<name>.mirror. Running `git config` would
// take 5–20 ms, so we read the files ourselves.
//
// Values are collected from all files at once (global, repository, worktree,
// plus include and includeIf regardless of the condition). That is more
// cautious than git itself: if a risky value is set anywhere at all, the hook
// takes it into account. The system gitconfig is not read.

// gitConfig is the key values: section and key name in lower case, the
// subsection as is ("remote.origin.push", "branch.Feature.merge").
type gitConfig map[string][]string

func readGitConfig(repo, home string) gitConfig {
	c := gitConfig{}
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		xdg = filepath.Join(home, ".config")
	}
	c.readFile(filepath.Join(xdg, "git", "config"), home, 0)
	c.readFile(filepath.Join(home, ".gitconfig"), home, 0)
	if gd := gitDir(repo); gd != "" {
		c.readFile(filepath.Join(commonDir(gd), "config"), home, 0)
		c.readFile(filepath.Join(gd, "config.worktree"), home, 0)
	}
	return c
}

func (c gitConfig) add(key, value string) {
	c[key] = append(c[key], value)
}

// isTrue reports whether at least one value of the key means "yes".
func (c gitConfig) isTrue(key string) bool {
	for _, v := range c[key] {
		switch strings.ToLower(v) {
		case "true", "yes", "on", "1":
			return true
		}
	}
	return false
}

// readFile parses a file in git config format: [section "subsection"],
// key = value lines, # and ; comments.
func (c gitConfig) readFile(path, home string, depth int) {
	if depth > 10 {
		return // circular include
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				continue
			}
			section = parseSection(line[1:end])
			line = strings.TrimSpace(line[end+1:]) // [push] default = x on a single line
		}
		if line == "" || line[0] == '#' || line[0] == ';' || section == "" {
			continue
		}
		name, raw, hasValue := strings.Cut(line, "=")
		name = strings.ToLower(strings.TrimSpace(name))
		value := "true" // a key without "=" is a boolean "yes"
		if hasValue {
			value = configValue(raw)
		}
		c.add(section+"."+name, value)
		if name == "path" && (section == "include" || strings.HasPrefix(section, "includeif.")) {
			c.readFile(includePath(value, path, home), home, depth+1)
		}
	}
}

// parseSection: `remote "origin"` → remote.origin, `push` → push.
func parseSection(s string) string {
	s = strings.TrimSpace(s)
	if name, sub, ok := strings.Cut(s, " "); ok {
		return strings.ToLower(name) + "." + strings.Trim(strings.TrimSpace(sub), `"`)
	}
	return strings.ToLower(s) // the old form [branch.main]
}

// configValue removes quotes, escapes and a trailing comment.
func configValue(s string) string {
	var b strings.Builder
	quoted := false
	s = strings.TrimSpace(s)
	for i := 0; i < len(s); i++ {
		switch ch := s[i]; {
		case ch == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		case ch == '"':
			quoted = !quoted
		case (ch == '#' || ch == ';') && !quoted:
			return strings.TrimSpace(b.String())
		default:
			b.WriteByte(ch)
		}
	}
	return strings.TrimSpace(b.String())
}

func includePath(value, from, home string) string {
	switch {
	case value == "~" || strings.HasPrefix(value, "~/"):
		return filepath.Join(home, strings.TrimPrefix(value, "~"))
	case filepath.IsAbs(value):
		return value
	}
	return filepath.Join(filepath.Dir(from), value)
}

// normalizeKey brings a key from `git -c` to the gitConfig form:
// Remote.origin.PUSH → remote.origin.push (the subsection keeps its case).
func normalizeKey(key string) string {
	first, last := strings.IndexByte(key, '.'), strings.LastIndexByte(key, '.')
	if first < 0 || first == last {
		return strings.ToLower(key)
	}
	return strings.ToLower(key[:first]) + key[first:last] + strings.ToLower(key[last:])
}

// addArgs adds `git -c key=value` values. unknown means a value is computed
// (a variable, --config-env) and not known in advance.
func (c gitConfig) addArgs(args []word, home string) (unknown bool) {
	for _, w := range args {
		if w.dynamic {
			unknown = true
			continue
		}
		key, value, ok := strings.Cut(w.s, "=")
		if !ok {
			value = "true"
		}
		key = normalizeKey(key)
		c.add(key, value)
		if key == "include.path" {
			c.readFile(includePath(value, "", home), home, 1)
		}
	}
	return unknown
}

// pushRemotes is where a `git push` without a remote name goes.
func (c gitConfig) pushRemotes(branch string) []string {
	var out []string
	for _, key := range []string{"branch." + branch + ".pushremote", "remote.pushdefault", "branch." + branch + ".remote"} {
		out = append(out, c[key]...)
	}
	return append(out, "origin")
}

// pushSpecs is the refspecs from remote.<name>.push for these remotes.
func (c gitConfig) pushSpecs(remotes []string) []string {
	var out []string
	for _, r := range remotes {
		out = append(out, c["remote."+r+".push"]...)
	}
	return out
}
