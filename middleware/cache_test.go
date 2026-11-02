package middleware_test

import (
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

	t.Run("the handler cannot override the header (a middleware owns the header)", func(t *testing.T) {
		t.Parallel()

		h := fixtures.Handler(func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			res.Set("Cache-Control", "no-store")
			res.String("ok")
		}, middleware.Cache(time.Minute))

		rec := ehttptest.Do(h, fixtures.Req("GET", "/x"))

		ehttptest.Eq(t, "middleware value kept (Production)", rec.Header().Get("Cache-Control"), "max-age=60, stale-while-revalidate=60, stale-if-error=60")
	})

	t.Run("same on every status the handler picks", func(t *testing.T) {
		t.Parallel()

		ok := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.Cache(time.Minute)), fixtures.Req("GET", "/x"))
		h := fixtures.Handler(func(res *ehttp.ResponseWriter, req *ehttp.Request) { res.SendStatus(404) }, middleware.Cache(time.Minute))
		rec := ehttptest.Do(h, fixtures.Req("GET", "/x"))
		cc := rec.Header().Get("Cache-Control")

		ehttptest.Eq(t, "middleware contract", rec.Code, 404)
		ehttptest.True(t, "RFC 9111 §5.2.2.1", strings.Contains(cc, "max-age=60"), "Cache-Control %q lacks max-age=60 on 404", cc)
		ehttptest.Eq(t, "middleware contract (same value on every status)", cc, ok.Header().Get("Cache-Control"))
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

// RFC 9111 §1.2.2: delta-seconds is a non-negative integer.
func TestCacheNegativeDuration(t *testing.T) {
	t.Parallel()

	ehttptest.KnownBug(t, "cache-negative-duration", func(t testing.TB) {
		rec := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.Cache(-time.Minute)), fixtures.Req("GET", "/x"))
		cc := rec.Header().Get("Cache-Control")

		ehttptest.True(t, "RFC 9111 §1.2.2", !strings.Contains(cc, "=-"), "Cache-Control %q has a negative delta-seconds", cc)
	})
}
