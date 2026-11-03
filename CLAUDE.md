# CLAUDE.md

## dx-agents profile

- Language: ASD-STE100
- Acceptance check: `make check`. It runs these checks:
  - test: `EHTTP_PENDING=1 go test -race -shuffle=on -count=1 ./...` (pending mode: an open bug is a skip, a fixed bug with a marker fails)
  - known-bugs: the pending list by bug name. `make tdd` shows the red view (each open bug fails)
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

## Tasks and decisions

- Use the dx tools for tasks and decisions. At session start, run `task_list` and `adr_list`.

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
