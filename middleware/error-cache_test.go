package middleware_test

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"github.com/EqualSoftware/ehttp/internal/fixtures"
	"github.com/EqualSoftware/ehttp/middleware"
)

func TestErrorCache(t *testing.T) {
	t.Parallel()

	const basis = "ErrorCache contract"

	tests := []struct {
		code      int
		wantCache bool
	}{
		{200, false},
		{204, false},
		{301, false},
		{399, false},
		{400, true},
		{404, true},
		{500, true},
		{503, true},
	}

	for _, tt := range tests {
		t.Run(bStatusName(tt.code), func(t *testing.T) {
			t.Parallel()

			rec := ehttptest.Do(fixtures.Handler(fixtures.Status(tt.code, "payload"), middleware.ErrorCache(time.Minute)), fixtures.Req("GET", "/x"))
			cc := rec.Header().Get("Cache-Control")

			ehttptest.Eq(t, basis+" status kept", rec.Code, tt.code)
			ehttptest.Eq(t, basis+" body kept", rec.Body.String(), "payload")
			ehttptest.Eq(t, basis+" content-type kept", rec.Header().Get("Content-Type"), ehttp.MIMETextPlainCharsetUTF8)
			ehttptest.Eq(t, basis+" cache only >=400", strings.Contains(cc, "max-age=60"), tt.wantCache)
		})
	}

	t.Run("zero duration marks errors no-store", func(t *testing.T) {
		t.Parallel()

		rec := ehttptest.Do(fixtures.Handler(fixtures.Status(500, "x"), middleware.ErrorCache(0)), fixtures.Req("GET", "/x"))

		ehttptest.True(t, "RFC 9111 §5.2.2.5", strings.Contains(rec.Header().Get("Cache-Control"), "no-store"),
			"Cache-Control = %q", rec.Header().Get("Cache-Control"))
	})

	t.Run("status without body", func(t *testing.T) {
		t.Parallel()

		h := fixtures.Handler(func(res *ehttp.ResponseWriter, req *ehttp.Request) { res.SendStatus(404) }, middleware.ErrorCache(time.Minute))
		rec := ehttptest.Do(h, fixtures.Req("GET", "/x"))

		ehttptest.Eq(t, basis, rec.Code, 404)
		ehttptest.Eq(t, basis, rec.Body.Len(), 0)
		ehttptest.True(t, basis, strings.Contains(rec.Header().Get("Cache-Control"), "max-age=60"), "no cache header on a bodyless 404")
	})

	t.Run("handler that writes nothing still answers 200", func(t *testing.T) {
		t.Parallel()

		rec := ehttptest.Do(fixtures.Handler(func(*ehttp.ResponseWriter, *ehttp.Request) {}, middleware.ErrorCache(time.Minute)), fixtures.Req("GET", "/x"))

		ehttptest.Eq(t, "net/http implicit 200", rec.Code, 200)
	})
}

func bStatusName(code int) string { return "status " + strconv.Itoa(code) }

// C4: ErrorCache only needs the status. Buffering every body keeps whole
// responses in memory and defeats streaming and Range.
func TestErrorCacheDoesNotBufferBody(t *testing.T) {
	t.Parallel()

	ehttptest.KnownBug(t, "C4", func(t testing.TB) {
		rec := httptest.NewRecorder()
		seen := -1
		h := fixtures.Handler(func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			res.String("first chunk")
			seen = rec.Body.Len() // what has reached the client before the handler returns
		}, middleware.ErrorCache(time.Minute))

		h.ServeHTTP(rec, fixtures.Req("GET", "/x"))

		ehttptest.True(t, "C4", seen > 0, "handler wrote a body but %d bytes reached the underlying writer before it returned", seen)
	})
}

// S10(c): statuses that depend on who asked must not be cached by URL.
func TestErrorCacheClientDependentStatuses(t *testing.T) {
	t.Parallel()

	for _, code := range []int{401, 403, 407, 429} {
		t.Run(bStatusName(code), func(t *testing.T) {
			t.Parallel()

			ehttptest.KnownBug(t, "S10", func(t testing.TB) {
				rec := ehttptest.Do(fixtures.Handler(fixtures.Status(code, "x"), middleware.ErrorCache(time.Minute)), fixtures.Req("GET", "/x"))
				cc := rec.Header().Get("Cache-Control")

				ehttptest.True(t, "S10(c)", !strings.Contains(cc, "max-age=60"), "status %d is cached: %q", code, cc)
			})
		})
	}
}

// ErrorCache followed by a handler error keeps working when composed with
// Recover on the outside: the panic skips the interceptor, Recover answers.
func TestErrorCacheWithRecover(t *testing.T) {
	t.Parallel()

	h := fixtures.Handler(func(res *ehttp.ResponseWriter, req *ehttp.Request) { panic("boom") },
		middleware.Recover(), middleware.ErrorCache(time.Minute))
	rec := ehttptest.Do(h, fixtures.Req("GET", "/x"))

	ehttptest.Eq(t, "middleware composition", rec.Code, 500)
	ehttptest.True(t, "middleware composition", rec.Body.Len() > 0, "empty body after recovered panic")
}
