package middleware_test

import (
	"net/http"
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
		{204, false}, // no content: RFC 9110 §15.3.5
		{301, false},
		{399, false},
		{400, true},
		{404, true},
		{500, true},
		{429, true},
		{503, true}, // no fixed exclusion: Timeout sends its own no-store
		{599, true},
	}

	for _, tt := range tests {
		t.Run(bStatusName(tt.code), func(t *testing.T) {
			t.Parallel()

			// A 204 has no content (RFC 9110 §15.3.5): the handler sends none.
			h, body, ctype := fixtures.Status(tt.code, "payload"), "payload", ehttp.MIMETextPlainCharsetUTF8
			if tt.code == 204 {
				h = func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.SendStatus(204) }
				body, ctype = "", ""
			}

			rec := ehttptest.Do(fixtures.Handler(h, middleware.ErrorCache(time.Minute)), fixtures.Req("GET", "/x"))
			cc := rec.Header().Get("Cache-Control")

			ehttptest.Eq(t, basis+" status kept", rec.Code, tt.code)
			ehttptest.Eq(t, basis+" body kept (204: RFC 9110 §15.3.5, empty)", rec.Body.String(), body)
			ehttptest.Eq(t, basis+" content-type kept", rec.Header().Get("Content-Type"), ctype)
			ehttptest.Eq(t, basis+" cache only >=400", strings.Contains(cc, "max-age=60"), tt.wantCache)
		})
	}

	t.Run("zero duration sets no header", func(t *testing.T) {
		t.Parallel()

		rec := ehttptest.Do(fixtures.Handler(fixtures.Status(500, "x"), middleware.ErrorCache(0)), fixtures.Req("GET", "/x"))

		ehttptest.Eq(t, "ErrorCache contract: zero is off", rec.Header().Get("Cache-Control"), "")
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

// ErrorCache only needs the status. Buffering every body keeps whole
// responses in memory and defeats streaming and Range.
func TestErrorCacheDoesNotBufferBody(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	seen := -1
	h := fixtures.Handler(func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		res.String("first chunk")
		seen = rec.Body.Len() // what has reached the client before the handler returns
	}, middleware.ErrorCache(time.Minute))

	h.ServeHTTP(rec, fixtures.Req("GET", "/x"))

	ehttptest.True(t, "ErrorCache streams the body to the client", seen > 0, "handler wrote a body but %d bytes reached the underlying writer before it returned", seen)
}

// 401, 403 and 407 depend on the credentials: always private (RFC 9111 §5.2.2.7).
func TestErrorCachePrivateStatuses(t *testing.T) {
	t.Parallel()

	for _, code := range []int{401, 403, 407} {
		t.Run(bStatusName(code), func(t *testing.T) {
			t.Parallel()

			rec := ehttptest.Do(fixtures.Handler(fixtures.Status(code, "x"), middleware.ErrorCache(time.Minute)), fixtures.Req("GET", "/x"))

			ehttptest.Eq(t, "ErrorCache contract", rec.Header().Get("Cache-Control"), "private, max-age=60, stale-while-revalidate=60, stale-if-error=60")
		})
	}
}

// A request with Authorization gets a private answer (RFC 9111 §3.5); one without stays public.
func TestErrorCacheAuthorizationIsPrivate(t *testing.T) {
	t.Parallel()

	h := fixtures.Handler(fixtures.Status(404, "x"), middleware.ErrorCache(time.Minute))

	rec := ehttptest.Do(h, fixtures.Req("GET", "/x", "Authorization", "Bearer t"))
	ehttptest.Eq(t, "ErrorCache contract", rec.Header().Get("Cache-Control"), "private, max-age=60, stale-while-revalidate=60, stale-if-error=60")

	rec = ehttptest.Do(h, fixtures.Req("GET", "/x"))
	ehttptest.Eq(t, "ErrorCache contract", rec.Header().Get("Cache-Control"), "max-age=60, stale-while-revalidate=60, stale-if-error=60")
}

func TestErrorCacheStatusOverride(t *testing.T) {
	t.Parallel()

	mw := middleware.ErrorCache(time.Minute, middleware.ErrorCacheStatus(405, 5*time.Second), middleware.ErrorCacheStatus(500, 0))

	tests := map[int]string{
		404: "max-age=60, stale-while-revalidate=60, stale-if-error=60", // default
		405: "max-age=5, stale-while-revalidate=5, stale-if-error=5",    // override
		500: "",                                                         // 0 = off
	}

	for code, want := range tests {
		t.Run(bStatusName(code), func(t *testing.T) {
			t.Parallel()

			rec := ehttptest.Do(fixtures.Handler(fixtures.Status(code, "x"), mw), fixtures.Req("GET", "/x"))

			ehttptest.Eq(t, "ErrorCache contract", rec.Header().Get("Cache-Control"), want)
		})
	}
}

// A Cache-Control that is already set wins, with or without Authorization.
func TestErrorCacheKeepsExistingCacheControl(t *testing.T) {
	t.Parallel()

	h := fixtures.Handler(func(res *ehttp.ResponseWriter, _ *ehttp.Request) {
		res.Set("Cache-Control", "no-store")
		res.Status(401)
		res.String("x")
	}, middleware.ErrorCache(time.Minute))

	for _, hdr := range [][]string{nil, {"Authorization", "Bearer t"}} {
		rec := ehttptest.Do(h, fixtures.Req("GET", "/x", hdr...))

		ehttptest.Eq(t, "ErrorCache contract: an existing value wins", strings.Join(rec.Header().Values("Cache-Control"), "|"), "no-store")
	}
}

func TestErrorCacheBootPanics(t *testing.T) {
	t.Parallel()

	tests := map[string]func(){
		"negative default": func() { middleware.ErrorCache(-time.Second) },
		"negative status":  func() { middleware.ErrorCache(time.Minute, middleware.ErrorCacheStatus(404, -time.Second)) },
		"status below 400": func() { middleware.ErrorCache(time.Minute, middleware.ErrorCacheStatus(399, time.Second)) },
		"status above 599": func() { middleware.ErrorCache(time.Minute, middleware.ErrorCacheStatus(600, time.Second)) },
		"status given twice": func() {
			middleware.ErrorCache(time.Minute, middleware.ErrorCacheStatus(404, time.Second), middleware.ErrorCacheStatus(404, 0))
		},
	}

	for name, fn := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var got any

			func() {
				defer func() { got = recover() }()

				fn()
			}()

			msg, _ := got.(string)
			ehttptest.True(t, "boot message format", strings.HasPrefix(msg, "ehttp: ") && strings.Contains(msg, "Fix: "), "panic = %v", got)
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

// A flush under ErrorCache commits the response: it works, and the error
// status still gets its Cache-Control.
func TestFlushInsideErrorCacheWorks(t *testing.T) {
	t.Parallel()

	var got error

	h := fixtures.Handler(func(res *ehttp.ResponseWriter, _ *ehttp.Request) {
		res.Status(404)
		got = http.NewResponseController(res).Flush()
		_, _ = res.Write([]byte("x"))
	}, middleware.ErrorCache(time.Minute))

	rec := ehttptest.Do(h, fixtures.Req("GET", "/x"))

	ehttptest.Eq(t, "ResponseController.Flush", got, nil)
	ehttptest.Eq(t, "ErrorCache contract status kept", rec.Code, 404)
	ehttptest.Eq(t, "ErrorCache contract cache set before the flush", strings.Contains(rec.Header().Get("Cache-Control"), "max-age=60"), true)
	ehttptest.Eq(t, "body still sent once", rec.Body.String(), "x")
}

// A 503 is temporary (RFC 9110 §15.6.4): it gets the default, and an option turns it off.
func TestErrorCache503(t *testing.T) {
	t.Parallel()

	rec := ehttptest.Do(fixtures.Handler(fixtures.Status(503, "x"), middleware.ErrorCache(time.Minute)), fixtures.Req("GET", "/x"))
	ehttptest.Eq(t, "ErrorCache contract: 503 gets the default", rec.Header().Get("Cache-Control"), "max-age=60, stale-while-revalidate=60, stale-if-error=60")

	h := fixtures.Handler(fixtures.Status(503, "x"), middleware.ErrorCache(time.Minute, middleware.ErrorCacheStatus(503, 0)))
	rec = ehttptest.Do(h, fixtures.Req("GET", "/x"))
	ehttptest.Eq(t, "ErrorCache contract: zero turns 503 off", rec.Header().Get("Cache-Control"), "")
}
