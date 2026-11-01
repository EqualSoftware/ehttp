package ehttptest

import "testing"

func TestKnownBugSkipsWhileFailing(t *testing.T) {
	KnownBug(t, "SELF-1", func(t testing.TB) {
		Eq(t, "self", 1, 2)
	})
}

func TestKnownBugSkipsOnFatal(t *testing.T) {
	KnownBug(t, "SELF-2", func(t testing.TB) {
		t.Fatalf("stop")
		panic("unreachable")
	})
}

func TestKnownBugSkipsOnPanic(t *testing.T) {
	KnownBug(t, "SELF-3", func(t testing.TB) {
		var m map[string]int
		m["x"] = 1 //nolint:staticcheck // the nil-map write is the panic under test
	})
}
