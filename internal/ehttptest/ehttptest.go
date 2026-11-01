// Package ehttptest holds the conventions shared by every ehttp test:
// each assertion carries its basis (spec section or proposal ID), and
// known bugs run as strict expected failures.
package ehttptest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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
// e.g. "RFC 9110 §15.5.6" or "S9".
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

// KnownBug runs check as a strict expected failure for bug id.
// The test is skipped while check fails, and fails once check passes,
// so a fixed bug forces its marker to be removed.
func KnownBug(t *testing.T, id string, check func(t testing.TB)) {
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

	if x.failed {
		t.Skipf("known bug %s: %s", id, strings.Join(x.msgs, "; "))
	}

	t.Errorf("known bug %s no longer reproduces: remove the KnownBug marker", id)
}

type xfail struct {
	testing.TB
	failed bool
	msgs   []string
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
