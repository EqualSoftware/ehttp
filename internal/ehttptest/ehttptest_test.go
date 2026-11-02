package ehttptest

import (
	"strings"
	"testing"
)

// The SELF tests call decide with an explicit mode, so they do not depend on
// EHTTP_PENDING and never appear in the pending list.

func failEq(t testing.TB) { Eq(t, "self", 1, 2) }
func failFatal(t testing.TB) {
	t.Fatalf("stop")
	panic("unreachable")
}

func failPanic(t testing.TB) {
	var m map[string]int
	m["x"] = 1 //nolint:staticcheck // the nil-map write is the panic under test
}

func passing(t testing.TB) { Eq(t, "self", 1, 1) }

func TestKnownBugModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		check   func(testing.TB)
		pending bool
		kind    verdictKind
		prefix  string
	}{
		{"default fails on assertion", failEq, false, verdictFail, "pending sample-bug: "},
		{"default fails on fatal", failFatal, false, verdictFail, "pending sample-bug: "},
		{"default fails on panic", failPanic, false, verdictFail, "pending sample-bug: "},
		{"default passes when fixed", passing, false, verdictPass, ""},
		{"pending skips on assertion", failEq, true, verdictSkip, "pending sample-bug: "},
		{"pending skips on fatal", failFatal, true, verdictSkip, "pending sample-bug: "},
		{"pending skips on panic", failPanic, true, verdictSkip, "pending sample-bug: "},
		{"pending fails when fixed", passing, true, verdictFail, "known bug sample-bug no longer reproduces"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			v := decide(t, "sample-bug", tc.check, tc.pending)
			Eq(t, "KnownBug modes", v.kind, tc.kind)
			True(t, "KnownBug modes", strings.HasPrefix(v.msg, tc.prefix), "message %q, want prefix %q", v.msg, tc.prefix)
		})
	}
}

func TestKnownBugPendingReportsFirstMessage(t *testing.T) {
	t.Parallel()

	v := decide(t, "sample-bug-2", func(t testing.TB) {
		t.Errorf("one")
		t.Errorf("two")
	}, true)
	Eq(t, "KnownBug modes", v.msg, "pending sample-bug-2: one")

	v = decide(t, "sample-bug-2", func(t testing.TB) {
		t.Errorf("one")
		t.Errorf("two")
	}, false)
	Eq(t, "KnownBug modes", v.msg, "pending sample-bug-2: one; two")
}
