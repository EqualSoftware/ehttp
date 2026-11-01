# Test, quality and performance gates for ehttp.
# `make check` is the gate for the push hook: test + allocs + lint + vuln.

GOVULNCHECK_VERSION ?= v1.9.0
BENCHSTAT_VERSION   ?= v0.0.0-20261009192801-be2c69fb417e
FUZZTIME            ?= 30s
BENCHCOUNT          ?= 6
FUZZ_TARGETS        := FuzzRouterMatch FuzzParseRange FuzzAccepts FuzzVary FuzzCorsOrigin

.PHONY: test allocs cover lint vuln fuzz bench bench-compare golden-update known-bugs check

test:
	go test -race -shuffle=on -count=1 ./...

# Allocation ceilings; skipped under -race, so they run here.
allocs:
	go test -count=1 -run='^TestAllocs' ./test/perf

cover:
	go test -count=1 -coverpkg=./... -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -n 1

lint:
	golangci-lint run ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

# One target at a time: `go test -fuzz` accepts a single target per run.
fuzz:
	@for t in $(FUZZ_TARGETS); do \
		echo "== $$t ($(FUZZTIME))"; \
		go test -run='^$$' -fuzz="^$$t$$" -fuzztime=$(FUZZTIME) . || exit 1; \
	done

bench:
	go test -run='^$$' -bench=. -benchmem -count=$(BENCHCOUNT) ./... > bench.txt
	@echo "wrote bench.txt"

# Compare a saved baseline (bench-base.txt) with the current run (bench.txt).
bench-compare:
	go run golang.org/x/perf/cmd/benchstat@$(BENCHSTAT_VERSION) bench-base.txt bench.txt

golden-update:
	go test -count=1 -run TestGolden -update ./test/golden

known-bugs:
	@go test -count=1 -v ./... 2>&1 | grep 'known bug'

check: test allocs lint vuln
