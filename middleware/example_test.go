package middleware_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/middleware"
)

// show prints the Cache-Control of GET path.
func show(s *ehttp.Server, path string) {
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), "GET", path, http.NoBody))

	cc := rec.Header().Get("Cache-Control")
	if cc == "" {
		cc = "(none)"
	}

	fmt.Printf("%d %s\n", rec.Code, cc)
}

func status(code int) ehttp.HandlerFunc {
	return func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.SendStatus(code) }
}

// A global ErrorCache decides the errors. A Cache on a route decides the
// statuses below 400 of that route.
func ExampleErrorCache() {
	s := ehttp.NewServer()
	s.Use(middleware.ErrorCache(time.Minute))
	s.GET("/page", status(200), middleware.Cache(time.Hour))
	s.GET("/other", status(200))
	s.GET("/missing", status(404))
	s.GET("/broken", status(500))
	s.GET("/limited", status(429))

	for _, path := range []string{"/page", "/other", "/missing", "/broken", "/limited"} {
		show(s, path)
	}

	// Output:
	// 200 max-age=3600, stale-while-revalidate=3600, stale-if-error=3600
	// 200 (none)
	// 404 max-age=60, stale-while-revalidate=60, stale-if-error=60
	// 500 no-store
	// 429 no-store
}

// ErrorCacheStatus sets the lifetime of one status. Zero sends no header.
func ExampleErrorCacheStatus() {
	s := ehttp.NewServer()
	s.Use(middleware.ErrorCache(time.Minute,
		middleware.ErrorCacheStatus(405, 5*time.Second),
		middleware.ErrorCacheStatus(404, 0),
		middleware.ErrorCacheStatus(503, 10*time.Second),
	))
	s.GET("/a", status(405))
	s.GET("/b", status(404))
	s.GET("/c", status(503))
	s.GET("/d", status(410))

	for _, path := range []string{"/a", "/b", "/c", "/d"} {
		show(s, path)
	}

	// Output:
	// 405 max-age=5, stale-while-revalidate=5, stale-if-error=5
	// 404 (none)
	// 503 max-age=10, stale-while-revalidate=10, stale-if-error=10
	// 410 max-age=60, stale-while-revalidate=60, stale-if-error=60
}

// ErrorCache(0) sends no-store for the default group.
func ExampleErrorCache_zero() {
	s := ehttp.NewServer()
	s.Use(middleware.ErrorCache(0))
	s.GET("/missing", status(404))
	s.GET("/broken", status(500))

	show(s, "/missing")
	show(s, "/broken")

	// Output:
	// 404 no-store
	// 500 no-store
}

// A Cache-Control that the handler sets wins.
func ExampleErrorCache_handlerValue() {
	s := ehttp.NewServer()
	s.Use(middleware.ErrorCache(time.Minute))
	s.GET("/gone", func(res *ehttp.ResponseWriter, _ *ehttp.Request) {
		res.Set("Cache-Control", "max-age=7")
		res.SendStatus(410)
	})

	show(s, "/gone")

	// Output:
	// 410 max-age=7
}

// Cache(0) sends the long no-store form.
func ExampleCache() {
	s := ehttp.NewServer()
	s.GET("/live", status(200), middleware.Cache(0))
	s.GET("/fresh", status(200), middleware.Cache(time.Minute))

	show(s, "/live")
	show(s, "/fresh")

	// Output:
	// 200 no-store, no-cache, must-revalidate, proxy-revalidate
	// 200 max-age=60, stale-while-revalidate=60, stale-if-error=60
}
