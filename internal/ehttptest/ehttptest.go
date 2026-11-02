// Package ehttptest holds the conventions shared by every ehttp test:
// each assertion carries its basis (a spec section or a reason), and
// known bugs run as strict expected failures.
package ehttptest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
)

// Do serves req through h in memory and returns the recorded response.
func Do(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Eq fails t when got != want. basis names why the expectation holds,
// e.g. "RFC 9110 §15.5.6".
func Eq[T comparable](t testing.TB, basis string, got, want T) {
	t.Helper()

	if got != want {
		t.Errorf("[%s] got %#v, want %#v", basis, got, want)
	}
}

// True fails t when cond is false.
func True(t testing.TB, basis string, cond bool, format string, args ...any) {
	t.Helper()

	if !cond {
		t.Errorf("[%s] %s", basis, fmt.Sprintf(format, args...))
	}
}

// PendingEnv selects the pending mode of KnownBug when it is not empty.
const PendingEnv = "EHTTP_PENDING"

// KnownBug runs check for the open defect name. The name is a kebab-case
// description of the defect, for example "sample-bug". A test that covers
// two defects joins the names with "+". The check asserts the correct
// behavior. Use it like this:
//
//	ehttptest.KnownBug(t, "sample-bug", func(t testing.TB) {
//		ehttptest.Eq(t, "RFC 9110 §1", got, want)
//	})
//
// Default mode: a failure of check fails the test with a message that starts
// with "pending <name>:". A pass is a pass. This is the TDD view (red until the
// bug is fixed).
//
// Pending mode (EHTTP_PENDING set): a failure skips the test with the same
// "pending <name>:" prefix, so a gate can stay green and list the open work.
// A pass fails the test, so a fixed bug forces its marker to be removed.
//
// Keep setup (ports, fixtures, servers) outside check: in pending mode a
// Fatal inside check is reported as pending, not as a setup failure.
func KnownBug(t *testing.T, name string, check func(t testing.TB)) {
	t.Helper()

	v := decide(t, name, check, os.Getenv(PendingEnv) != "")

	switch v.kind {
	case verdictFail:
		t.Error(v.msg)
	case verdictSkip:
		t.Skip(v.msg)
	}
}

type verdictKind int

const (
	verdictPass verdictKind = iota
	verdictFail
	verdictSkip
)

type verdict struct {
	kind verdictKind
	msg  string
}

// decide runs check and maps its result to a verdict for the given mode.
func decide(t testing.TB, name string, check func(t testing.TB), pending bool) verdict {
	t.Helper()

	x := &xfail{TB: t}
	done := make(chan struct{})

	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				x.fail(fmt.Sprintf("panic: %v", r))
			}
		}()
		check(x)
	}()
	<-done

	if !pending {
		if x.failed {
			return verdict{verdictFail, fmt.Sprintf("pending %s: %s", name, x.summary())}
		}

		return verdict{}
	}

	if x.failed {
		return verdict{verdictSkip, fmt.Sprintf("pending %s: %s", name, x.first())}
	}

	return verdict{verdictFail, fmt.Sprintf("known bug %s no longer reproduces: remove the KnownBug marker", name)}
}

type xfail struct {
	testing.TB
	failed bool
	msgs   []string
}

func (x *xfail) summary() string {
	if len(x.msgs) == 0 {
		return "failed"
	}

	return strings.Join(x.msgs, "; ")
}

func (x *xfail) first() string {
	if len(x.msgs) == 0 {
		return "failed"
	}

	return x.msgs[0]
}

func (x *xfail) fail(msg string) {
	x.failed = true

	if msg != "" {
		x.msgs = append(x.msgs, msg)
	}
}

func (x *xfail) Fail()                        { x.fail("") }
func (x *xfail) Failed() bool                 { return x.failed }
func (x *xfail) Error(args ...any)            { x.fail(fmt.Sprint(args...)) }
func (x *xfail) Errorf(f string, args ...any) { x.fail(fmt.Sprintf(f, args...)) }
func (x *xfail) FailNow()                     { x.fail(""); runtime.Goexit() }
func (x *xfail) Fatal(args ...any)            { x.fail(fmt.Sprint(args...)); runtime.Goexit() }
func (x *xfail) Fatalf(f string, args ...any) { x.fail(fmt.Sprintf(f, args...)); runtime.Goexit() }
func (x *xfail) Skip(args ...any)             { x.FailNow() }
func (x *xfail) Skipf(f string, args ...any)  { x.FailNow() }
func (x *xfail) SkipNow()                     { x.FailNow() }
func (x *xfail) Helper()                      {}
