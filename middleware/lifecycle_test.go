package middleware_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"github.com/EqualSoftware/ehttp/internal/fixtures"
	"github.com/EqualSoftware/ehttp/middleware"
)

func bGet(t *testing.T, c *http.Client, url string) (*http.Response, string, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := c.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)

	return resp, string(b), err
}

// S5: a handler that writes before the deadline keeps its response.
func TestTimeoutKeepsResponseWrittenBeforeDeadline(t *testing.T) {
	t.Parallel()

	srv := ehttp.NewServer()
	srv.Use(middleware.Timeout(30 * time.Millisecond))
	srv.GET("/t", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		res.Status(http.StatusCreated)
		res.String("early")
		<-req.Context().Done()
	})

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	resp, body, err := bGet(t, ts.Client(), ts.URL+"/t")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	ehttptest.Eq(t, "design 2.3 option A (author decision)", resp.StatusCode, http.StatusCreated)
	ehttptest.Eq(t, "design 2.3 option A (author decision)", body, "early")
}

// S6: after the headers are sent, Recover aborts the connection (HTTP/1 and HTTP/2).
func TestRecoverAbortsAfterHeadersSent(t *testing.T) {
	t.Parallel()

	native := ehttp.WrapHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()

		panic("late failure")
	}))

	for _, h2 := range []bool{false, true} {
		name := "http1"
		if h2 {
			name = "http2"
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srv := ehttp.NewServer()
			srv.Use(middleware.Recover())
			srv.GET("/p", native)

			ts := httptest.NewUnstartedServer(srv)
			ts.EnableHTTP2 = h2

			if h2 {
				ts.StartTLS()
			} else {
				ts.Start()
			}

			t.Cleanup(ts.Close)

			resp, body, err := bGet(t, ts.Client(), ts.URL+"/p")
			ehttptest.True(t, "S6 / net/http ErrAbortHandler", err != nil, "no connection error: status %v body %q", resp, body)
			ehttptest.True(t, "S6", !strings.Contains(body, `"code"`), "error JSON glued to the body: %q", body)
		})
	}
}

// S6: a panic before any write gives 500 without the panic value.
func TestRecoverBeforeWriteOverNetwork(t *testing.T) {
	t.Parallel()

	srv := ehttp.NewServer()
	srv.Use(middleware.Recover())
	srv.GET("/p", func(*ehttp.ResponseWriter, *ehttp.Request) { panic("db password=hunter2") })

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	resp, body, err := bGet(t, ts.Client(), ts.URL+"/p")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	ehttptest.Eq(t, "S6", resp.StatusCode, http.StatusInternalServerError)
	ehttptest.True(t, "S6", !strings.Contains(body, "hunter2"), "body leaks the panic value: %q", body)
}

// S10(b): Cache with Authorization sends one private Cache-Control line.
func TestCacheAuthorizationIsPrivate(t *testing.T) {
	t.Parallel()

	rec := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.Cache(time.Minute)), fixtures.Req("GET", "/x", "Authorization", "Bearer t"))

	ehttptest.Eq(t, "S10(b)", rec.Header().Get("Cache-Control"), "private, max-age=60, stale-while-revalidate=60, stale-if-error=60")
	ehttptest.Eq(t, "S10(b)", len(rec.Header().Values("Cache-Control")), 1)
}

// Scenario A: the handler sets Cache-Control itself on an error and the request has Authorization.
func TestErrorCacheAuthorizationWithHandlerCacheControl(t *testing.T) {
	t.Parallel()

	h := fixtures.Handler(func(res *ehttp.ResponseWriter, _ *ehttp.Request) {
		res.Append("Cache-Control", "no-store")
		res.Status(http.StatusNotFound)
		res.String("nope")
	}, middleware.ErrorCache(time.Minute))
	rec := ehttptest.Do(h, fixtures.Req("GET", "/x", "Authorization", "Bearer t"))

	ehttptest.Eq(t, "S10(b)", rec.Code, http.StatusNotFound)
	ehttptest.True(t, "S10(b)", strings.HasPrefix(rec.Header().Get("Cache-Control"), "private,"), "Cache-Control = %q", rec.Header().Get("Cache-Control"))
}

func bTimeoutServer(t *testing.T, outer ehttp.MiddlewareHandlerFunc, h ehttp.HandlerFunc) *httptest.Server {
	t.Helper()

	srv := ehttp.NewServer()
	srv.Use(outer, middleware.Timeout(30*time.Millisecond))
	srv.GET("/t", h)

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	return ts
}

// A 503 is never cached (RFC 9110 §15.6.4).
func TestTimeout503IsNotCached(t *testing.T) {
	t.Parallel()

	slow := func(_ *ehttp.ResponseWriter, req *ehttp.Request) { <-req.Context().Done() }

	for name, outer := range map[string]ehttp.MiddlewareHandlerFunc{
		"Cache outside":      middleware.Cache(time.Minute),
		"ErrorCache outside": middleware.ErrorCache(time.Minute),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ts := bTimeoutServer(t, outer, slow)
			resp, _, err := bGet(t, ts.Client(), ts.URL+"/t")
			if err != nil {
				t.Fatalf("get: %v", err)
			}

			ehttptest.Eq(t, "RFC 9110 §15.6.4", resp.StatusCode, http.StatusServiceUnavailable)
			ehttptest.Eq(t, "503 is not cached", strings.Join(resp.Header.Values("Cache-Control"), "|"), "no-store")
		})
	}
}

func TestErrorCacheSkips503FromHandler(t *testing.T) {
	t.Parallel()

	rec := ehttptest.Do(fixtures.Handler(fixtures.Status(503, "x"), middleware.ErrorCache(time.Minute)), fixtures.Req("GET", "/x"))

	ehttptest.Eq(t, "RFC 9110 §15.6.4", rec.Header().Get("Cache-Control"), "")
}

// Scenario B: the client is gone after the handler sent a status. The context
// is cancelled before ErrorCache sets the cache header, and the request has
// Authorization. res.Header() is then an empty map: no panic is allowed.
func TestErrorCacheAuthorizationClientGone(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	h := fixtures.Handler(func(res *ehttp.ResponseWriter, _ *ehttp.Request) {
		res.SendStatus(http.StatusNotFound)
		cancel()
	}, middleware.ErrorCache(time.Minute))
	req := fixtures.Req("GET", "/x", "Authorization", "Bearer t").WithContext(ctx)

	var got any

	func() {
		defer func() { got = recover() }()

		ehttptest.Do(h, req)
	}()

	ehttptest.True(t, "S10(b)", got == nil, "ErrorCache panicked: %v", got)
}
