# Image for the benchmarks. Alpine only, exact tag.
# Build from the repository root: `make bench` does it.

# bench: a controlled place to run `go test -bench`. The module is mounted
# read-only at /src, the Go caches live in the volume /cache, and the results
# go to /out.
FROM golang:1.27.3-alpine AS bench
ARG BENCHSTAT_VERSION=v0.0.0-20261009192801-be2c69fb417e
RUN GOBIN=/usr/local/bin go install golang.org/x/perf/cmd/benchstat@${BENCHSTAT_VERSION} \
 && adduser -D -u 10001 bench \
 && mkdir /cache /out && chown bench /cache /out
USER bench
ENV CGO_ENABLED=0 GOCACHE=/cache/build GOMODCACHE=/cache/mod GOTOOLCHAIN=local
WORKDIR /src
