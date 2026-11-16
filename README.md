# Holdfast

**English** · [Русский](README.ru.md)

**Restore points and guardrails for AI coding agents.** `guard` works with Claude Code;
`lint` checks instruction files for any agent (Claude Code, Cursor, Codex).

You give Claude Code a task, it runs for twenty minutes, and the project no longer builds.
Some files were changed with the edit tool, others with `sed -i`, a code generator, or `rm`,
and you no longer know what the tree looked like before.

**Holdfast gives you a one-command rollback of the whole working tree to the moment before
each prompt — including Bash edits that Claude Code's `/rewind` doesn't track.**

![demo](docs/demo.gif)

```bash
go install github.com/roma2246/holdfast/cmd/holdfast@latest   # or: brew install --cask roma2246/tap/holdfast
cd my-project && holdfast guard install                       # once per project
```

That's it: a restore point is now saved before every prompt you send.

## What it does

- **Restore points.** Before every prompt you send to the agent, Holdfast saves the state of
  the project. If everything falls apart after a task, one command brings the files back —
  including what the agent did with terminal commands.
- **Guardrails.** The hook checks the agent's actions without slowing it down with questions:
  dangerous things (`rm -rf ~`, `git push --force`, `curl | sh`, reading `.env`) are denied
  with a hint on how to do it properly, and project rules are shown to the agent at the moment
  it touches a file.
- **`holdfast lint`** finds broken paths, non-existent commands, contradictions, duplicates
  and secrets in `CLAUDE.md` / `AGENTS.md` / `.cursorrules`.
- **`holdfast sync`** turns the prohibitions in `CLAUDE.md` into hook rules.

The hook is a set of guardrails, not a sandbox: it parses the text of commands and does not
isolate the agent at the file-system level. What it doesn't catch is listed under
[Limitations](#limitations).

A single static binary; restore points need git installed.
The hook answers in ~0.1 ms (≈3 ms including process startup).

> To re-record the GIF: `./docs/demo/record.sh` — [VHS](https://github.com/charmbracelet/vhs) in Docker, needs Docker and Go. Script: [docs/demo.tape](docs/demo.tape).

## Installation

```bash
go install github.com/roma2246/holdfast/cmd/holdfast@latest
```

```bash
brew install --cask roma2246/tap/holdfast
```

Or download an archive for your OS from [Releases](https://github.com/roma2246/holdfast/releases).

## Quick start

```bash
cd my-project
holdfast guard install   # once: connects Holdfast to Claude Code
holdfast doctor          # check that everything is connected (✓/✗ with hints)
```

Nothing else is needed: before every prompt you send to the agent, Holdfast saves a restore
point on its own. If a task broke everything:

```bash
holdfast guard list                     # restore points: when, before which prompt, what changed since
holdfast guard diff --to 2              # what exactly changed after point 2
holdfast guard rollback --to 2          # bring the files back to point 2 (shows the list and asks first)
holdfast guard rollback --to 2 src/     # or selectively roll back only specific files or directories
```

Claude Code's built-in `/rewind` [does not track](https://code.claude.com/docs/en/checkpointing#limitations)
files changed only by terminal commands (`sed -i`, `rm`, code generators), subagent edits to
other files, or your own manual changes. A Holdfast restore point is a snapshot of the whole
working tree, so rollback brings those back too.

Optional:

```bash
holdfast init            # holdfast.toml: your own rules and hints for the agent
holdfast lint            # checks CLAUDE.md / AGENTS.md / .cursorrules
holdfast guard log       # what the agent was allowed, what was denied, and why
```

## Checking and uninstalling

`holdfast doctor` checks the installation and, for every problem, prints how to fix it:

```
✓ git is installed — git version 2.50.1
✓ Claude Code hooks — PreToolUse and UserPromptSubmit in .claude/settings.json
✓ binary for the hooks: holdfast — /usr/local/bin/holdfast
✓ holdfast.toml is readable
✓ no recent config-error entries in the log — entries in the last 24 h: 42
✓ restore points are created without errors — in the last 24 h: 12
✓ restore points in refs/holdfast/ — 12, the latest 3 min ago
```

What it checks: git is installed; both hooks are present in `.claude/settings.json` or
`settings.local.json`; the program the hooks call is on `PATH` or at the given path
(`guard install --bin`); `holdfast.toml` is readable; the log has no `config-error` entries
since the config was last edited and the latest restore point did not fail; restore points exist.
Exit code is 0 if everything is fine and 2 if there are problems.

Uninstalling:

```bash
holdfast guard uninstall          # remove the hooks from .claude/settings.json (--local — from settings.local.json)
holdfast guard uninstall --purge  # also delete the .holdfast/ log and all restore points (asks first)
```

`uninstall` removes only Holdfast's hooks: other settings and other tools' hooks stay as they were.
Without `--purge` restore points are kept, and you can still roll back to them after the hooks
are removed. `--purge` deletes the `refs/holdfast*/` refs in all working trees of the
repository; git's regular garbage collection (`git gc`) removes the objects themselves. Then
remove the binary (`brew uninstall --cask holdfast` or the file in `$(go env GOPATH)/bin`).

## How it compares

Based on each project's own README and docs (November 2026). "Not in README" means the README
doesn't mention it — not that the feature is definitely absent.

| | **Holdfast** | Claude Code [`/rewind`](https://code.claude.com/docs/en/checkpointing) | [destructive_command_guard](https://github.com/Dicklesworthstone/destructive_command_guard) (dcg) | [cc-safety-net](https://github.com/kenryu42/cc-safety-net) |
|---|---|---|---|---|
| What it is | PreToolUse hook + restore points (Go) | Built-in checkpoints | PreToolUse hook (Rust) | PreToolUse hook (Node.js 22+) |
| Agents | Claude Code (hooks); `lint` for any instruction files | Claude Code | Claude Code, Codex CLI, Gemini CLI, Copilot CLI, Cursor, OpenCode and more | Claude Code, Codex, Cursor, Gemini CLI, OpenCode and more |
| Automatic restore point before each prompt | yes | yes | — | — |
| Rollback restores files changed only by Bash commands | yes (files visible to git) | no — only files Claude already edited with its file tools | no rollback | no rollback |
| Rollback restores subagent and manual edits | yes (files visible to git) | mostly no (some forked skills are restored) | — | — |
| Rewinds the conversation | no | yes (code and conversation separately) | — | — |
| Blocks destructive shell commands | yes | — | yes | yes |
| How commands are analysed | bash AST ([mvdan/sh](https://github.com/mvdan/sh)), incl. `bash -c`, `eval`, `$(…)` | — | keyword filter + regex patterns; tree-sitter/ast-grep for heredocs and inline scripts | parses the command; nested `bash -c`/`sudo`/`env` |
| Looks inside `python -c` / `node -e` code | no | — | yes | yes |
| `git push --force` | denied | — | denied (`--force-with-lease` allowed) | denied (`--force-with-lease` allowed) |
| Plain `git push` to `main`/`master` | denied with a hint (autonomous) / ask (strict) | — | not in README | no such rule in its blocked-commands list |
| Reading `.env`, SSH keys | denied | — | not in README (packs cover secret-manager CLIs) | denied |
| What the agent gets on a block | the reason + what to do instead | — | the reason + a tip | the reason |
| Modes | autonomous / strict | — | per-pack and per-rule deny / ask / warn / log | Standard / Strict / Paranoid |
| Lint for CLAUDE.md / AGENTS.md | yes | — | not in README | not in README |
| Install | `go install`, Homebrew, release archives | built in | verified installer script, Homebrew | `npx -y cc-safety-net@latest install` |

In short: dcg and cc-safety-net support many more agents and ship far larger rule sets (cloud
CLIs, databases, Kubernetes, interpreter one-liners). Holdfast's focus is different: undoing
whatever the agent did to the working tree, and denials that tell an autonomous agent what to
do instead of stopping to ask you.

## lint

```
$ holdfast lint
CLAUDE.md:3   HF001 error    path "src/server/" does not exist
CLAUDE.md:8   HF004 warning  contradiction: "use yarn" (line 7) and "never use yarn"
CLAUDE.md:15  HF002 error    "npm run e2e": script e2e is not in package.json
    ↳ the agent will run a command that doesn't exist; update the instructions
```

| ID    | What it checks                                                                 | Level   |
|-------|--------------------------------------------------------------------------------|---------|
| HF001 | A path in the text (`src/api/`, `docs/setup.md`) does not exist                 | error   |
| HF002 | `npm run X` / `make X` / `just X` is missing from package.json / Makefile / justfile | error |
| HF003 | File exceeds the limit (2000 tokens by default ≈ characters / 4)               | warning |
| HF004 | Contradiction: "use yarn" and "never use yarn"                                 | warning |
| HF005 | Duplicate and near-identical items (word Jaccard similarity > 90%)             | warning |
| HF006 | Looks like a secret: API key, token, password                                  | error   |
| HF007 | Vague instructions ("write good code")                                         | info    |
| HF008 | No section with build and test commands                                        | info    |
| HF009 | The instructions contain a prohibition that is not in the guard policy (run `sync`) | warning |

All checks are local, no LLM calls. Files are checked in parallel.

- `--format json` — machine-readable report for CI.
- `--strict` — warnings count as errors too.
- Exit codes: `0` — no errors, `1` — there are errors, `2` — the tool itself failed.
- Disable a rule: `disable = ["HF007"]` in `holdfast.toml`,
  `<!-- holdfast-disable HF003 -->` for the whole file, or
  `<!-- holdfast-disable-next-line HF001 -->` for the next line.

## guard

`holdfast guard install` adds this to `.claude/settings.json` (without overwriting anything
else; `--local` writes to `.claude/settings.local.json` instead):

```json
{
  "hooks": {
    "PreToolUse": [{
      "matcher": "Bash|Edit|Write|MultiEdit|NotebookEdit|Read",
      "hooks": [{ "type": "command", "command": "holdfast guard check", "timeout": 10 }]
    }],
    "UserPromptSubmit": [{
      "hooks": [{ "type": "command", "command": "holdfast guard checkpoint", "timeout": 30 }]
    }]
  }
}
```

The first hook checks every agent action; the second saves a restore point before every
prompt you send.

Before each tool call Claude Code passes JSON to the hook. The hook parses the command with a
real bash parser ([mvdan.cc/sh](https://github.com/mvdan/sh)) rather than substring search:
every simple command is checked in `&&`, `;`, `|` chains, `$(...)` and `<(...)` substitutions,
`bash -c "..."`, `eval`, functions and loops; `sudo`/`env`/`timeout`/`xargs` and `cd` are
taken into account. `echo "rm -rf /"` is a harmless echo, while `ls && rm -rf ~` is denied.

### Guardrails, not a gate

By default Holdfast runs in **`autonomous`** mode: the agent doesn't wait for a human.

- **Irreversible actions are denied — with a hint on how to do it properly.** The agent gets
  the reason and picks another route on its own: `git push` to `main` → "create a branch and
  open a PR", `rm -rf $DIR` → "spell out the path", reading `.env` → "look up variable names in
  `.env.example`".
- **Reversible actions are allowed — if they really are reversible.** Before a mass deletion
  inside the project (`rm -r`, `find -delete`, `git clean -f` on more than `max_deletes`
  files) the hook lists the files to be deleted, **takes a restore point itself** and checks
  that every file made it in. If they did, the deletion goes through without questions. If
  not (git ignores those files), it is denied with an explanation. The exception is
  regenerable folders such as `node_modules` and `dist` (`disposable_paths`): they can always
  be deleted.
- **The rule arrives with the action.** When the agent reads or edits a file, the hook adds
  the rules for that specific file to its context: lines of `CLAUDE.md`/`AGENTS.md` that
  mention the path, and notes from `[[guard.context]]`. Each hint is shown once per session.
  Rules for individual folders can be moved out of CLAUDE.md into the config, making it shorter.
- **"Ask" only where you asked for it yourself**: `ask_commands`, `protected_paths`, `ask-paths`.

| | autonomous (default) | strict |
|---|---|---|
| `rm -r` on `/`, `~`, the whole repository, outside the repository; `git push --force`; `git reset --hard`; `curl … \| sh`; `.env`, `*.pem`, `id_rsa` | deny | deny |
| `git push` to your own branch | allow | ask |
| `git push` to `main`/`master` — including via `git -c remote.origin.push=…`, `push.default=upstream`, `git switch main && git push`; `--tags`, `--delete`, `--all` | deny with hint | ask |
| `git push` whose destination can't be determined: `push.default=matching`, branch or remote in a variable, `GIT_DIR`/`GIT_CONFIG_*`, `git config` before the push | deny with hint "name the branch explicitly" | ask |
| editing Holdfast's own files: `holdfast.toml`, `.claude/settings*.json` (project and `~`), `.holdfast/`, `refs/holdfast` | deny with hint | ask |
| deleting more than `max_deletes` project files: `rm -r`, `find -delete`, `git clean -f` | allow, if everything being deleted made it into the restore point just taken | ask |
| mass deletion of files git ignores (except `disposable_paths`) | deny with hint | ask |
| deletion touching `.git`; `find`/`git clean` outside the project | deny | deny / ask |
| `holdfast guard rollback`, `holdfast sync`, `holdfast init`, `holdfast guard install`, `holdfast guard uninstall` run by the agent | ask | ask |
| `npm publish`, `rm -rf $VAR`, `xargs rm -r`, deleting files outside git repositories | deny with hint | ask |
| deletion in temp folders (`/tmp/...`), unless it is someone else's git repository | allow | deny / ask |
| deleting individual files in other git repositories (no more than `max_deletes`) | allow | ask |
| `ask_commands`, `protected_paths` from the config | ask | ask |

Path and secret rules apply to the resulting files, not to how they are spelled: `cat .en[v]`,
`cat link-to-.env`, `rm go.su*`, `cp other/go.sum .` get the same decision as the direct path.
`cd` is handled the way the shell handles it: inside `( … )`, `$( … )`, `bash -c`, a pipeline,
a background command and `env -C` it does not change the folder for subsequent commands.

If the hook makes no decision, Claude Code's normal permission check applies — the hook never
silently approves an action. The hook never crashes: on an internal error it answers "ask"
and records this in the log. The log is `.holdfast/log.jsonl` (the folder excludes itself from git).
When the log reaches 5 MB it is renamed to `log.1.jsonl` (the previous `log.1.jsonl` is
deleted), so it never takes more than 10 MB on disk; `holdfast guard log` reads both parts.

### Restore points

- **Created automatically** before every prompt to the agent and before a mass deletion; if
  nothing has changed since the previous point, no new one is created. Manually:
  `holdfast guard start`. The last `keep_snapshots` (50) are kept.
- **What's inside:** everything git sees: tracked files with uncommitted changes, and new
  files. Files from `.gitignore` (`.env`, `node_modules`) are not included in restore points,
  and rollback doesn't touch them.
- **Where they live:** in `refs/holdfast/` of your own git repository, separately for each
  working tree (`git worktree`). Branches, commits, stash and the index are not changed;
  nothing is sent anywhere.
- **Rollback loses nothing.** Before a rollback Holdfast makes a plan and backs up everything
  that will be overwritten or deleted — even files git currently ignores. If something can't be
  backed up, the rollback doesn't start. To undo a rollback:
  `holdfast guard rollback --snapshot <backup name>` (the command is printed after the rollback).
- **Selective rollback:** Specify paths or globs to restore only individual files or directories
  without discarding the rest of the agent's work: `holdfast guard rollback --to 2 path/to/file.go "src/**"`.
- **A human decides on rollback:** the agent can look at `guard list` and `guard diff`, but for
  `guard rollback` the hook asks for your confirmation.
- **If a restore point wasn't created,** Holdfast doesn't stay silent: a warning appears in
  Claude Code, in the log and in `holdfast guard list`.
- **Speed:** on a 20,000-file project a restore point takes ~0.15 s (the first one, a couple of seconds).

### Limitations

Holdfast honestly does what is described above, but no more:

- **It is not a sandbox.** The hook reads the text of the command. A Python script, a `make`
  target or any program can change and delete files in ways that aren't visible from the
  arguments. Restore points protect against such changes, not denials.
- **A restore point is a git snapshot, not a disk copy.** It does not include files from
  `.gitignore`, the contents of nested git repositories and submodules, permissions (other
  than the executable bit) or empty folders. Anything outside the project folder (databases,
  external services, other repositories) will not be brought back by a rollback.
- **Path denials see commands, not the file system.** Globs, `{a,b}`, symbolic links and
  `cp file folder/` are expanded, but writing through a hard link created earlier, and edits
  made by git commands (`git checkout -- file`, `git restore`, `git stash`), are not checked
  by these denials.
- **`cd` inside a condition** (`[ -d x ] && cd x`) is assumed to have run.
- **The destination of `git push`** is taken from the arguments, `git -c` and git config files
  (global, repository, worktree and those pulled in via include). The system gitconfig and
  environment variables set outside the command are not taken into account.
- **Integration with Claude Code only.** In other agents `lint`, manual restore points
  (`guard start`) and rollback work, but the hooks don't.
- **`doctor` sees its own `PATH`, not the one Claude Code runs the hooks with.** If Claude
  Code wasn't started from the same terminal (for example, the app launched from the Dock),
  `PATH` may differ. It is more reliable to set the full path:
  `holdfast guard uninstall && holdfast guard install --bin "$(command -v holdfast)"`.
- **Windows:** the project builds, but the path policy is designed for macOS and Linux.

## sync

```markdown
<!-- holdfast
deny-paths: migrations/**, .github/workflows/**
ask-commands: docker compose down
-->
```

`holdfast sync` moves such blocks into the `[guard.synced]` section of `holdfast.toml`,
recording the source line. Keys: `deny-paths`, `ask-paths`, `deny-commands`, `ask-commands`.

Phrases in the text — "never edit `migrations/`", "don't touch generated/", «не трогай `vendor/`»,
"never run `make deploy`" — are found heuristically: sync lists them and adds only the ones
you confirm (`--yes` — accept all, `--no-heuristics` — explicit blocks only,
`--dry-run` — write nothing).

## Configuration

```toml
[lint]
files = ["CLAUDE.md", "AGENTS.md", ".cursorrules"]
max_tokens = 2000
disable = ["HF007"]

[guard]
mode = "autonomous"                    # or "strict" — human confirmation for risky actions
protected_branches = ["main", "master"]
protected_paths = ["migrations/**"]    # ** — any depth, * — within one folder
deny_commands = ["terraform destroy"]  # the pattern's words are matched in the command in order
ask_commands = ["docker compose down"] # "ask" — in any mode
max_deletes = 5                        # above this, the hook first takes a restore point and checks it
keep_snapshots = 50                    # how many recent restore points to keep
disposable_paths = ["node_modules", "dist", "build"]  # regenerable: can always be deleted

[[guard.context]]                      # a hint for the agent when it works with these files
paths = ["migrations/**"]
note = "Don't change existing migrations: add a new file 00N_description.sql"
```

## Development

```bash
go build ./...
go test ./...
golangci-lint run ./...
go test -bench=BenchmarkHook -run='^$' ./internal/guard/
goreleaser build --snapshot --clean
```

Structure and conventions are in [CLAUDE.md](CLAUDE.md).

## License

[MIT](LICENSE)
