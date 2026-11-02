// C7 gate - allocation ceilings per hot path. testing.AllocsPerRun is
// deterministic, so this test blocks. Each ceiling is the value measured on
// 2026-11-01 (go1.27.3, darwin/arm64). A fix that lowers allocations may lower
// the ceiling in the same PR; a change that raises it must justify it.
//
// Not parallel: AllocsPerRun reads global runtime counters.
package perf_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/middleware"
	"go.uber.org/zap"
)

func TestAllocsHotPaths(t *testing.T) { //nolint:paralleltest // AllocsPerRun is global
	if raceEnabled {
		t.Skip("allocation gate runs without -race: make allocs")
	}

	gh := githubServer(t)
	stack := exampleApp(t)

	corsSrv := ehttp.NewServer()
	corsSrv.POST("/body", noop)
	corsSrv.GET("/", noop)
	cors := corsSrv

	preflight := httptest.NewRequestWithContext(context.Background(), "OPTIONS", "/body", http.NoBody)
	preflight.Header.Set("Origin", "http://localhost:3000")
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	preflight.Header.Set("Access-Control-Request-Headers", "content-type, x-token")

	simple := httptest.NewRequestWithContext(context.Background(), "GET", "/", http.NoBody)
	simple.Header.Set("Origin", "http://localhost:3000")

	// The real app renders the HTML template when Accept is absent.
	plainRoot := httptest.NewRequestWithContext(context.Background(), "GET", "/", http.NoBody)
	plainRoot.Header.Set("Accept", "text/plain")

	logSrv := ehttp.NewServer(ehttp.WithLogger(zap.NewNop()))
	logSrv.Use(middleware.Log(middleware.LogOptions{}))
	logSrv.GET("/", noop)

	cases := []struct {
		name    string
		h       http.Handler
		req     *http.Request
		ceiling float64 // measured after the owned headers (B7c); keep equal to the measure
	}{
		{"router static first route", gh, httptest.NewRequestWithContext(context.Background(), "GET", "/authorizations", http.NoBody), 3},
		{"router static mid", gh, httptest.NewRequestWithContext(context.Background(), "GET", "/gists/public", http.NoBody), 3},
		{"router static deep (scan ~236)", gh, httptest.NewRequestWithContext(context.Background(), "GET", "/user/keys", http.NoBody), 3},
		{"router 1 param", gh, httptest.NewRequestWithContext(context.Background(), "GET", "/users/octocat", http.NoBody), 3},
		{"router 3 params", gh, httptest.NewRequestWithContext(context.Background(), "GET", "/repos/o/r/issues/42/comments", http.NoBody), 3},
		{"router wildcard", gh, httptest.NewRequestWithContext(context.Background(), "GET", "/repos/o/r/contents/a/b/c.txt", http.NoBody), 4},
		{"router regex (compiled per request, S12)", gh, httptest.NewRequestWithContext(context.Background(), "GET", "/regex/12345", http.NoBody), 3},
		{"router 404", gh, httptest.NewRequestWithContext(context.Background(), "GET", "/this/path/does/not/exist", http.NoBody), 4},
		{"router 405 (C1 path)", gh, httptest.NewRequestWithContext(context.Background(), "DELETE", "/user/keys/1", http.NoBody), 8},
		{"cors simple", cors, simple, 5},
		{"cors preflight", cors, preflight, 7},
		{"route with Log middleware (nop logger)", logSrv, httptest.NewRequestWithContext(context.Background(), "GET", "/", http.NoBody), 6},
		{"stack GET / html (no Accept)", stack, httptest.NewRequestWithContext(context.Background(), "GET", "/", http.NoBody), 72},
		{"stack GET / plain", stack, plainRoot, 94},
		{"stack GET /api/v1/hello/:name", stack, httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/hello/octocat", http.NoBody), 46},
		{"stack 404", stack, httptest.NewRequestWithContext(context.Background(), "GET", "/nope", http.NoBody), 40},
	}

	for _, c := range cases {
		w := newDiscard()
		got := testing.AllocsPerRun(200, func() { c.h.ServeHTTP(w, c.req) })
		t.Logf("%-45s allocs/op=%v ceiling=%v", c.name, got, c.ceiling)

		if got > c.ceiling {
			t.Errorf("[C7] %s: %v allocs/op, ceiling %v", c.name, got, c.ceiling)
		}
	}
}
