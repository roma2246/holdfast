# Holdfast

A Go CLI: `holdfast lint` checks agent instruction files,
`holdfast guard` is a Claude Code PreToolUse hook plus snapshots and rollback,
`holdfast sync` carries the prohibitions from CLAUDE.md over into the guard policy.

## Commands

- Build: `go build ./...`
- Run: `go run ./cmd/holdfast --help`
- Tests: `go test ./...`
- Coverage: `go test -coverpkg=./internal/... -coverprofile=cover.out ./... && go tool cover -func=cover.out | tail -1`
- Hook benchmark: `go test -bench=BenchmarkHook -run='^$' ./internal/guard/`
- Linter: `golangci-lint run ./...`
- Format: `gofmt -w .`
- Local release build: `goreleaser build --snapshot --clean`

## Structure

- `cmd/holdfast/main.go`: the entry point, it only calls `cli.Execute`.
- `internal/cli/`: cobra commands; exit codes: 0 ok, 1 lint findings, 2 failure.
- `internal/config/`: `holdfast.toml`; the `[guard.synced]` section is written only by `sync`.
- `internal/lint/`: parser, rule registry, report; `internal/lint/rules/` holds one rule per file.
- `internal/guard/`: the policy (`policy.go`, `shell.go`, `massdelete.go`), the hooks (`hook.go`, `checkpoint.go`), the log, snapshots and rollback (`snapshot.go`), hook installation.
- `internal/bridge/`: extracts prohibitions from CLAUDE.md.
- `testdata/lint/HFxxx/{bad,good}/`: rule fixtures; `testdata/lint/all/`: all rules at once.

## Conventions

- A new lint rule: a file `internal/lint/rules/hfNNN_name.go` with `lint.Register` in `init()`,
  fixtures `testdata/lint/HFNNN/bad` and `good`, and the ID in the list in `rules_test.go`.
- Shell commands are parsed only through the AST (`mvdan.cc/sh/v3/syntax`), with no substring search.
- A new guard case is a row in a table test: `TestAutonomousMode` (the default mode) or `TestCheckCommand` (strict).
- In autonomous mode a denial always tells the agent what to do instead; "ask" happens only where the user configured it explicitly.
- The hook must not crash and must answer in under 10 ms: check `TestHookLatency`.
- Comments in English; keep short explanations for non-obvious Go constructs. CLI output and hook messages are in English too; Russian stays only in `README.ru.md` and in recognizing Russian phrases in users' instruction files.
- Coverage stays at 80% or higher.
- Guarantees (lossless rollback, worktree isolation, reversible deletion) are checked by property tests in `guarantees_test.go`; a new guarantee means a new property there.
- The README promises no more than the tests verify: what the hook does not catch is listed in the "Limitations" section.

<!-- holdfast
deny-paths: go.sum
ask-commands: goreleaser release, git tag
-->
