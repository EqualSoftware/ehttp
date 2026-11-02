package middleware_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"github.com/EqualSoftware/ehttp/internal/fixtures"
	"github.com/EqualSoftware/ehttp/middleware"
)

func TestCache(t *testing.T) {
	t.Parallel()

	const basis = "RFC 9111 §5.2.2"

	t.Run("positive duration sets max-age and stale directives", func(t *testing.T) {
		t.Parallel()

		rec := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.Cache(time.Minute)), fixtures.Req("GET", "/x"))
		cc := rec.Header().Get("Cache-Control")

		ehttptest.Eq(t, basis, rec.Code, 200)
		for d, why := range map[string]string{
			"max-age=60":                "RFC 9111 §5.2.2.1",
			"stale-while-revalidate=60": "RFC 5861 §3",
			"stale-if-error=60":         "RFC 5861 §4",
		} {
			ehttptest.True(t, why, strings.Contains(cc, d), "Cache-Control %q lacks %q", cc, d)
		}
	})

	t.Run("zero duration disables caching", func(t *testing.T) {
		t.Parallel()

		rec := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.Cache(0)), fixtures.Req("GET", "/x"))
		cc := rec.Header().Get("Cache-Control")

		for _, d := range []string{"no-store", "no-cache", "must-revalidate"} {
			ehttptest.True(t, "RFC 9111 §5.2.2.5", strings.Contains(cc, d), "Cache-Control %q lacks %q", cc, d)
		}
		ehttptest.True(t, "RFC 9111 §5.2.2.5", !strings.Contains(cc, "max-age"), "Cache-Control %q has max-age", cc)
	})

	t.Run("calls next and keeps its body", func(t *testing.T) {
		t.Parallel()

		rec := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.Cache(time.Minute)), fixtures.Req("GET", "/x"))

		ehttptest.Eq(t, "middleware contract", rec.Body.String(), "ok")
	})

	t.Run("the handler can override the header (author decision: the last writer wins)", func(t *testing.T) {
		t.Parallel()

		h := fixtures.Handler(func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			res.Set("Cache-Control", "no-store")
			res.String("ok")
		}, middleware.Cache(time.Minute))

		rec := ehttptest.Do(h, fixtures.Req("GET", "/x"))

		ehttptest.Eq(t, "handler value sent (Production)", rec.Header().Get("Cache-Control"), "no-store")
	})

	t.Run("applies below 400 only", func(t *testing.T) {
		t.Parallel()

		for code, want := range map[int]bool{200: true, 204: true, 301: true, 304: true, 399: true, 400: false, 404: false, 500: false, 503: false} {
			rec := ehttptest.Do(fixtures.Handler(fixtures.Status(code, ""), middleware.Cache(time.Minute)), fixtures.Req("GET", "/x"))
			cc := rec.Header().Get("Cache-Control")

			ehttptest.Eq(t, "Cache contract (errors belong to ErrorCache) status "+strconv.Itoa(code), strings.Contains(cc, "max-age=60"), want)
		}
	})

	t.Run("Cache(1h) with a 500: ErrorCache decides", func(t *testing.T) {
		t.Parallel()

		h := fixtures.Handler(fixtures.Status(500, "x"), middleware.ErrorCache(time.Minute), middleware.Cache(time.Hour))
		rec := ehttptest.Do(h, fixtures.Req("GET", "/x"))

		ehttptest.Eq(t, "ErrorCache contract", rec.Header().Get("Cache-Control"), "no-store")
	})

	t.Run("a raw header set before WriteHeader wins", func(t *testing.T) {
		t.Parallel()

		h := fixtures.Handler(func(res *ehttp.ResponseWriter, _ *ehttp.Request) {
			res.Header().Set("Cache-Control", "max-age=7")
			res.String("ok")
		}, middleware.Cache(time.Minute))

		ehttptest.Eq(t, "Cache contract", ehttptest.Do(h, fixtures.Req("GET", "/x")).Header().Get("Cache-Control"), "max-age=7")
	})

	t.Run("a native handler and HEAD", func(t *testing.T) {
		t.Parallel()

		native := ehttp.WrapHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))

		for _, method := range []string{"GET", "HEAD"} {
			rec := ehttptest.Do(fixtures.Handler(native, middleware.Cache(time.Minute)), fixtures.Req(method, "/x"))

			ehttptest.True(t, "Cache contract "+method, strings.Contains(rec.Header().Get("Cache-Control"), "max-age=60"), "Cache-Control %q", rec.Header().Get("Cache-Control"))
		}
	})
}

// RFC 9111 §3.5: a shared cache may reuse a response to a request with
// Authorization only if the response has `public`. Emitting `public`
// unconditionally lets authenticated responses be shared.
func TestCachePublic(t *testing.T) {
	t.Parallel()

	t.Run("no public directive on the default path", func(t *testing.T) {
		t.Parallel()

		rec := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.Cache(time.Minute)), fixtures.Req("GET", "/x"))
		cc := rec.Header().Get("Cache-Control")

		ehttptest.True(t, "RFC 9111 §3.5", !strings.Contains(cc, "public"), "Cache-Control %q has public", cc)
	})

	t.Run("private when the request has Authorization", func(t *testing.T) {
		t.Parallel()

		rec := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.Cache(time.Minute)), fixtures.Req("GET", "/x", "Authorization", "Bearer t"))
		cc := rec.Header().Get("Cache-Control")

		ehttptest.True(t, "Cache with Authorization is private", strings.Contains(cc, "private"), "Cache-Control %q lacks private for an authenticated request", cc)
		ehttptest.True(t, "Cache with Authorization is not public", !strings.Contains(cc, "public"), "Cache-Control %q has public for an authenticated request", cc)
	})
}

// RFC 9111 §1.2.2: delta-seconds is a non-negative integer, so a negative
// duration is a mistake of the developer and panics at boot.
func TestCacheNegativeDuration(t *testing.T) {
	t.Parallel()

	msg := bootMessage(func() { middleware.Cache(-time.Minute) })

	ehttptest.True(t, "RFC 9111 §1.2.2", strings.HasPrefix(msg, "ehttp: duration -1m0s in Cache: "), "message %q", msg)
	ehttptest.True(t, "DX message: fix part", strings.Contains(msg, ". Fix: "), "message %q has no Fix part", msg)

	middleware.Cache(0) // zero is valid: no-store
}

// max-age takes whole seconds: a duration above 0 and under 1s panics at boot, 1.5s is cut to 1.
func TestCacheSubSecondDuration(t *testing.T) {
	t.Parallel()

	for name, fn := range map[string]func(){
		"Cache":            func() { middleware.Cache(500 * time.Millisecond) },
		"ErrorCache":       func() { middleware.ErrorCache(time.Nanosecond) },
		"ErrorCacheStatus": func() { middleware.ErrorCache(time.Minute, middleware.ErrorCacheStatus(404, 999*time.Millisecond)) },
	} {
		msg := bootMessage(fn)

		ehttptest.True(t, "RFC 9111 §1.2.2 ("+name+")", strings.HasPrefix(msg, "ehttp: duration ") && strings.Contains(msg, " in "+name+": "), "message %q", msg)
		ehttptest.True(t, "DX message: why and fix ("+name+")", strings.Contains(msg, "whole seconds") && strings.Contains(msg, "Fix: use 1s or more, or 0 to turn it off."), "message %q", msg)
	}

	// Valid: zero, one second, and a fraction above one second (cut to 1).
	middleware.Cache(0)
	middleware.ErrorCache(0, middleware.ErrorCacheStatus(404, 0))
	middleware.ErrorCache(time.Second, middleware.ErrorCacheStatus(404, 1500*time.Millisecond))

	rec := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.Cache(1500*time.Millisecond)), fixtures.Req("GET", "/x"))
	ehttptest.Eq(t, "delta-seconds are whole", rec.Header().Get("Cache-Control"), "max-age=1, stale-while-revalidate=1, stale-if-error=1")
}

// bootMessage returns the panic message of do, or "" when do does not panic.
func bootMessage(do func()) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg, _ = r.(string)
		}
	}()

	do()

	return ""
}

// readFromRecorder counts the calls of ReadFrom, like the sendfile path of net/http.
type readFromRecorder struct {
	*httptest.ResponseRecorder
	calls int
}

func (r *readFromRecorder) ReadFrom(src io.Reader) (int64, error) {
	r.calls++

	return io.Copy(struct{ io.Writer }{r.ResponseRecorder}, src)
}

// Cache must not remove the ReadFrom fast path of SendFile.
func TestCacheKeepsReadFrom(t *testing.T) {
	t.Parallel()

	name := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(name, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}

	for label, mw := range map[string]ehttp.MiddlewareHandlerFunc{"Cache": middleware.Cache(time.Minute), "ErrorCache": middleware.ErrorCache(time.Minute)} {
		h := fixtures.Handler(func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.SendFile(name) }, mw)
		rec := &readFromRecorder{ResponseRecorder: httptest.NewRecorder()}
		h.ServeHTTP(rec, fixtures.Req("GET", "/x"))

		ehttptest.Eq(t, label+" body", rec.Body.String(), "0123456789")
		ehttptest.True(t, label+" passes ReadFrom to the inner writer", rec.calls > 0, "ReadFrom calls = %d", rec.calls)
	}

	rec := httptest.NewRecorder() // no ReaderFrom inside: io.Copy fallback
	fixtures.Handler(func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.SendFile(name) }, middleware.Cache(time.Minute)).ServeHTTP(rec, fixtures.Req("GET", "/x"))
	ehttptest.Eq(t, "fallback body", rec.Body.String(), "0123456789")
	ehttptest.True(t, "Cache applies on the ReadFrom path", strings.Contains(rec.Header().Get("Cache-Control"), "max-age=60"), "Cache-Control %q", rec.Header().Get("Cache-Control"))
}
