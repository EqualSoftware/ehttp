# CLAUDE.md

## dx-agents profile

- Language: ASD-STE100
- Acceptance check: `make check`. It runs these checks:
  - test: `go test -race -shuffle=on -count=1 ./...`
  - allocs: `go test -count=1 -run='^TestAllocs' ./test/perf`
  - lint: `golangci-lint run ./...` (0 issues)
  - vuln: `govulncheck ./...`
- Independent review areas:
  - Request parsing: router match, `Range`, `Accept`, `Vary` and CORS origin parsing (the fuzz targets)
  - Security middleware: `secure`, `body-limit`, `timeout`, `recover`
  - Concurrency and shared state: `breaker`, `cache`, `error-cache`
  - The HTTP client and the server lifecycle (`client.go`, `server.go`)
- Commit scopes: `router`, `server`, `client`, `breaker`, `cache`, `middleware`, `test`, `build`
- Main branch: `main`

## Task files

- At session start, read `.tasks/lessons.md` and open `.tasks/todo.md`.
- Write each approved plan to `.tasks/todo.md` with checkable items: Goal, Plan, Verify, Review. Mark items complete as you go. Add the Review section at the end.
- After a correction from the user, add one rule to `.tasks/lessons.md`: Mistake, Rule, Check.

## Communication

- Lead with the answer, then the details.
- Show diffs, not descriptions.
- Flag risks before acting, not after.
- Say "I don't know" when you don't know.
- Keep summaries short and scannable.

## Rules

- Never invent requirements. Ask before you assume.
- No secrets in code or logs.
- Nothing is deleted, pushed or deployed without approval.
