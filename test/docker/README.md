# Docker

## Benchmarks

`make bench` always runs in the `bench` image (Alpine, `golang:1.27.3-alpine`).
It limits the container to `BENCHCPUS` CPUs (a fixed cpuset, default `2`) and
`BENCHMEM` of memory (default `1g`), and sets `GOMAXPROCS` to `BENCHCPUS`.
The module is mounted read-only. The Go caches are in the volume
`ehttp-bench-cache`. The result goes to `$BENCHOUT` (default: a new folder in
`$TMPDIR`); `make bench` prints the path.

```
make bench BENCHCOUNT=6
make bench BENCHCOUNT=1 BENCHOUT=/tmp/run1
```

Limit on macOS: Docker runs in a Linux VM, so the numbers depend on the VM
size and the host load. Compare only runs from the same environment.
