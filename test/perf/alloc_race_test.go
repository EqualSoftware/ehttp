//go:build race

package perf_test

// The race detector changes allocation counts, so the gate runs only
// without -race (same rule as the standard library's alloc tests).
const raceEnabled = true
