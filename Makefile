# Test, quality and performance gates for ehttp.
# `make check` is the gate for the push hook: test + allocs + lint + vuln.

GOVULNCHECK_VERSION ?= v1.9.0
BENCHSTAT_VERSION   ?= v0.0.0-20261009192801-be2c69fb417e
FUZZTIME            ?= 30s
BENCHCOUNT          ?= 6
FUZZ_TARGETS        := FuzzRouterMatch FuzzParseRange FuzzAccepts FuzzVary FuzzCorsOrigin

TESTLOG             ?= $(or $(TMPDIR),/tmp)/ehttp-test.log

.PHONY: test tdd allocs cover lint vuln fuzz bench bench-compare golden-update known-bugs check

# Pending mode: a test of an open bug (ehttptest.KnownBug) is skipped as
# "pending <ID>", and a fixed bug fails. The gate stays green while the work
# is listed by `known-bugs`. The full log is kept in $(TESTLOG).
test:
	@EHTTP_PENDING=1 go test -race -shuffle=on -count=1 -v ./... > $(TESTLOG) 2>&1; rc=$$?; \
	if [ $$rc -ne 0 ]; then cat $(TESTLOG); else grep -E '^(ok|\?) ' $(TESTLOG); fi; \
	exit $$rc

# TDD (red) view: no env, so every open bug is a FAIL "pending <ID>: ...".
tdd:
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
	go test -count=1 -run TestGolden ./test/golden -update

# Pending list: one line per bug ID with the count of pending tests, then the
# total. A test with a combined ID ("S3+S10") counts once for each ID in the
# table and once in the total. Reads the log of `test`.
known-bugs: test
	@ids=$$(grep -E '^ *[A-Za-z0-9_./-]+\.go:[0-9]+: pending [A-Za-z0-9+-]+:' $(TESTLOG) \
		| sed -E 's/^.*: pending ([A-Za-z0-9+-]+):.*$$/\1/'); \
	total=$$(printf '%s\n' "$$ids" | grep -c .); \
	echo "== pending (open bugs)"; \
	printf '%s\n' "$$ids" | tr '+' '\n' | sort | uniq -c \
		| awk '{ printf "%-8s %d\n", $$2, $$1; n++ } END { printf "total    %d pending tests, %d distinct IDs\n", '"$$total"', n }'

check: test allocs lint vuln known-bugs
