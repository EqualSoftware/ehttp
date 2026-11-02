package middleware_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"github.com/EqualSoftware/ehttp/internal/fixtures"
	"github.com/EqualSoftware/ehttp/middleware"
)

// The order matrix of Cache and ErrorCache: the instance closest to the
// handler wins inside one status group, and Cache and ErrorCache never decide
// the same status.
func TestCacheOrderMatrix(t *testing.T) {
	t.Parallel()

	minute, hour := time.Minute, time.Hour
	age := func(d time.Duration) string { return ehttp.CacheControl(d, false) }

	type mws = []ehttp.MiddlewareHandlerFunc

	tests := []struct {
		name          string
		global, route mws
		code          int
		want          string
	}{
		{"nested Cache, inner wins", nil, mws{middleware.Cache(hour), middleware.Cache(minute)}, 200, age(minute)},
		{"global Cache, route Cache wins", mws{middleware.Cache(hour)}, mws{middleware.Cache(minute)}, 200, age(minute)},
		{"global Cache(1m), route Cache(1h)", mws{middleware.Cache(minute)}, mws{middleware.Cache(hour)}, 200, age(hour)},
		{"nested ErrorCache, inner wins", nil, mws{middleware.ErrorCache(minute), middleware.ErrorCache(hour)}, 404, age(hour)},
		{"global ErrorCache, route ErrorCache wins", mws{middleware.ErrorCache(hour)}, mws{middleware.ErrorCache(minute)}, 404, age(minute)},
		{"Cache outside ErrorCache: 200", nil, mws{middleware.Cache(hour), middleware.ErrorCache(minute)}, 200, age(hour)},
		{"Cache outside ErrorCache: 404", nil, mws{middleware.Cache(hour), middleware.ErrorCache(minute)}, 404, age(minute)},
		{"Cache outside ErrorCache: 500", nil, mws{middleware.Cache(hour), middleware.ErrorCache(minute)}, 500, "no-store"},
		{"ErrorCache outside Cache: 200", nil, mws{middleware.ErrorCache(minute), middleware.Cache(hour)}, 200, age(hour)},
		{"ErrorCache outside Cache: 404", nil, mws{middleware.ErrorCache(minute), middleware.Cache(hour)}, 404, age(minute)},
		{"ErrorCache outside Cache: 500", nil, mws{middleware.ErrorCache(minute), middleware.Cache(hour)}, 500, "no-store"},
		{"global ErrorCache, route Cache: 200", mws{middleware.ErrorCache(minute)}, mws{middleware.Cache(hour)}, 200, age(hour)},
		{"global ErrorCache, route Cache: 404", mws{middleware.ErrorCache(minute)}, mws{middleware.Cache(hour)}, 404, age(minute)},
		{"global Cache, route ErrorCache: 200", mws{middleware.Cache(hour)}, mws{middleware.ErrorCache(minute)}, 200, age(hour)},
		{"global Cache, route ErrorCache: 404", mws{middleware.Cache(hour)}, mws{middleware.ErrorCache(minute)}, 404, age(minute)},
		{"inner off switch falls to the outer ErrorCache", nil,
			mws{middleware.ErrorCache(hour), middleware.ErrorCache(minute, middleware.ErrorCacheStatus(404, 0))}, 404, age(hour)},
		{"outer off switch, inner ErrorCache decides", nil,
			mws{middleware.ErrorCache(minute, middleware.ErrorCacheStatus(404, 0)), middleware.ErrorCache(hour)}, 404, age(hour)},
		{"only an off switch: no header", nil, mws{middleware.ErrorCache(minute, middleware.ErrorCacheStatus(404, 0))}, 404, ""},
		{"three levels", mws{middleware.Cache(hour)}, mws{middleware.Cache(minute), middleware.Cache(2 * minute)}, 200, age(2 * minute)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := ehttp.NewServer()
			s.Use(tt.global...)
			s.GET("/x", fixtures.Status(tt.code, "x"), tt.route...)

			rec := ehttptest.Do(s, fixtures.Req("GET", "/x"))

			ehttptest.Eq(t, "Cache contract: closest to the handler wins", rec.Header().Get("Cache-Control"), tt.want)
		})
	}
}

func TestCacheOrderWithTimeout(t *testing.T) {
	t.Parallel()

	slow := func(_ *ehttp.ResponseWriter, req *ehttp.Request) { <-req.Context().Done() }
	to := middleware.Timeout(time.Millisecond)

	tests := map[string]struct {
		route []ehttp.MiddlewareHandlerFunc
		want  string
	}{
		"ErrorCache outside":           {[]ehttp.MiddlewareHandlerFunc{middleware.ErrorCache(time.Minute), to}, "no-store"},
		"Cache and ErrorCache outside": {[]ehttp.MiddlewareHandlerFunc{middleware.Cache(time.Minute), middleware.ErrorCache(time.Minute), to}, "no-store"},
		"Cache outside only":           {[]ehttp.MiddlewareHandlerFunc{middleware.Cache(time.Minute), to}, ""},
		"ErrorCache(503) outside":      {[]ehttp.MiddlewareHandlerFunc{middleware.ErrorCache(time.Minute, middleware.ErrorCacheStatus(503, 7*time.Second)), to}, ehttp.CacheControl(7*time.Second, false)},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rec := ehttptest.Do(fixtures.Handler(slow, tt.route...), fixtures.Req("GET", "/x"))

			ehttptest.Eq(t, "RFC 9110 §15.6.4", rec.Code, 503)
			ehttptest.Eq(t, "Cache contract", rec.Header().Get("Cache-Control"), tt.want)
		})
	}
}

func TestCacheOrderWithWeakEtag(t *testing.T) {
	t.Parallel()

	body := func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.String("payload") }
	tag := ehttptest.Do(fixtures.Handler(body, middleware.WeakEtag()), fixtures.Req("GET", "/x")).Header().Get("Etag")
	cond := func(k, v string) *http.Request { return fixtures.Req("GET", "/x", k, v) }

	tests := []struct {
		name  string
		route []ehttp.MiddlewareHandlerFunc
		req   *http.Request
		code  int
		want  string
	}{
		{"304, Cache outside", mws2(middleware.Cache(time.Hour), middleware.WeakEtag()), cond("If-None-Match", tag), 304, ehttp.CacheControl(time.Hour, false)},
		{"304, Cache inside", mws2(middleware.WeakEtag(), middleware.Cache(time.Hour)), cond("If-None-Match", tag), 304, ehttp.CacheControl(time.Hour, false)},
		{"304, ErrorCache outside", mws2(middleware.ErrorCache(time.Minute), middleware.WeakEtag()), cond("If-None-Match", tag), 304, ""},
		{"412, ErrorCache outside, Cache inside", mws2(middleware.ErrorCache(time.Minute), middleware.WeakEtag(), middleware.Cache(time.Hour)), cond("If-Match", `"nope"`), 412, "no-store"},
		{"412, Cache outside, ErrorCache inside", mws2(middleware.Cache(time.Hour), middleware.WeakEtag(), middleware.ErrorCache(time.Minute)), cond("If-Match", `"nope"`), 412, ""},
		{"412, Cache only", mws2(middleware.Cache(time.Hour), middleware.WeakEtag()), cond("If-Match", `"nope"`), 412, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := ehttptest.Do(fixtures.Handler(body, tt.route...), tt.req)

			ehttptest.Eq(t, "RFC 9110 §13.1", rec.Code, tt.code)
			ehttptest.Eq(t, "Cache contract", strings.Join(rec.Header().Values("Cache-Control"), "|"), tt.want)
		})
	}
}

func mws2(m ...ehttp.MiddlewareHandlerFunc) []ehttp.MiddlewareHandlerFunc { return m }
