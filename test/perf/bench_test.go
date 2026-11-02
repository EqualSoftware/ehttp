// Benchmarks. Baseline numbers for the router, the middleware stack of
// examples/server and a few hot middlewares. ns/op is noisy and only reported;
// the deterministic gate is alloc_test.go.
//
// The router fixture is the GitHub API surface used by
// go-http-routing-benchmark (testdata/github_routes.txt), converted to the
// ehttp syntax (:param, trailing /...), plus two regex routes.
package perf_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/examples/server/app"
	"github.com/EqualSoftware/ehttp/middleware"
)

// discard is a ResponseWriter that keeps no body, so benchmarks measure the
// framework and not httptest.ResponseRecorder.
type discard struct{ h http.Header }

func newDiscard() *discard                     { return &discard{h: http.Header{}} }
func (d *discard) Header() http.Header         { return d.h }
func (d *discard) Write(b []byte) (int, error) { return len(b), nil }
func (d *discard) WriteHeader(int)             {}

func noop(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.SendStatus(ehttp.StatusOK) }

// githubServer registers the fixture. Handlers do nothing but SendStatus.
func githubServer(tb testing.TB) *ehttp.Server {
	tb.Helper()

	f, err := os.Open("testdata/github_routes.txt")
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()

	s := ehttp.NewServer()
	root := s.Route("") // Server has no Handle; a prefix-less group does
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		method, pattern, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		root.Handle(method, pattern, noop)
	}
	if err := sc.Err(); err != nil {
		tb.Fatal(err)
	}

	s.GET("/regex/:id|^[0-9]+$", noop)
	s.GET("/regex/:a|^[a-z]+$/x/:b|^[0-9]+$", noop)

	return s
}

// exampleApp is the real examples/server app.
func exampleApp(tb testing.TB) *ehttp.Server {
	tb.Helper()

	return app.New(filepath.Join("..", "..", "examples", "server"), time.Unix(1700000000, 0))
}

func bench(b *testing.B, h http.Handler, req *http.Request) {
	b.Helper()
	b.ReportAllocs()

	w := newDiscard()
	for b.Loop() {
		clear(w.h) // a fresh header each request: the first write of each field is measured
		h.ServeHTTP(w, req)
	}
}

func BenchmarkRouter(b *testing.B) {
	h := githubServer(b)

	cases := []struct{ name, method, path string }{
		{"static_first", "GET", "/authorizations"},
		{"static_mid", "GET", "/gists/public"},
		{"static_deep", "GET", "/user/keys"},                // near the end of ~236 routes: worst case of the linear scan
		{"c1_wrong_method_first", "DELETE", "/user/keys/1"}, // GET registered first -> 405
		{"param_1", "GET", "/users/octocat"},
		{"param_2", "GET", "/repos/octocat/hello/stargazers"},
		{"param_3", "GET", "/repos/octocat/hello/issues/42/comments"},
		{"wildcard", "GET", "/repos/octocat/hello/contents/a/b/c.txt"},
		{"regex_1", "GET", "/regex/12345"},
		{"regex_2", "GET", "/regex/abc/x/99"},
		{"not_found", "GET", "/this/path/does/not/exist"},
		{"method_not_allowed", "PATCH", "/gists/public"},
	}

	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			bench(b, h, httptest.NewRequestWithContext(context.Background(), c.method, c.path, http.NoBody))
		})
	}
}

func BenchmarkServeHTTPExampleStack(b *testing.B) {
	h := app.NewQuiet(filepath.Join("..", "..", "examples", "server"), time.Unix(1700000000, 0))

	b.Run("root_plain", func(b *testing.B) {
		req := httptest.NewRequestWithContext(context.Background(), "GET", "/", http.NoBody)
		req.Header.Set("Accept", "text/plain") // no Accept would render the HTML template
		bench(b, h, req)
	})
	b.Run("root_json", func(b *testing.B) {
		req := httptest.NewRequestWithContext(context.Background(), "GET", "/", http.NoBody)
		req.Header.Set("Accept", "application/json")
		bench(b, h, req)
	})
	b.Run("root_304", func(b *testing.B) {
		req := httptest.NewRequestWithContext(context.Background(), "GET", "/", http.NoBody)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		req.Header.Set("If-None-Match", rec.Header().Get("Etag"))
		bench(b, h, req)
	})
	b.Run("param_json", func(b *testing.B) {
		bench(b, h, httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/hello/octocat", http.NoBody))
	})
	b.Run("not_found", func(b *testing.B) {
		bench(b, h, httptest.NewRequestWithContext(context.Background(), "GET", "/nope", http.NoBody))
	})
}

func BenchmarkWeakEtag(b *testing.B) {
	for _, tc := range []struct {
		name string
		size int
	}{{"1KB", 1 << 10}, {"64KB", 64 << 10}} {
		b.Run(tc.name, func(b *testing.B) {
			body := strings.Repeat("x", tc.size)
			s := ehttp.NewServer()
			s.GET("/", func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.String(body) }, middleware.WeakEtag())
			bench(b, s, httptest.NewRequestWithContext(context.Background(), "GET", "/", http.NoBody))
		})
	}
}

func BenchmarkErrorCache(b *testing.B) {
	s := ehttp.NewServer()
	s.Use(middleware.ErrorCache(time.Minute))
	s.GET("/ok", func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.String("ok") })
	s.GET("/err", func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.SendStatus(ehttp.StatusInternalServerError) })

	for _, p := range []string{"/ok", "/err"} {
		b.Run(p[1:], func(b *testing.B) {
			bench(b, s, httptest.NewRequestWithContext(context.Background(), "GET", p, http.NoBody))
		})
	}
}

func BenchmarkCache(b *testing.B) {
	s := ehttp.NewServer()
	s.GET("/", func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.String("ok") }, middleware.Cache(time.Minute))
	bench(b, s, httptest.NewRequestWithContext(context.Background(), "GET", "/", http.NoBody))
}

func BenchmarkCacheAndErrorCache(b *testing.B) {
	s := ehttp.NewServer()
	s.Use(middleware.ErrorCache(time.Minute))
	s.GET("/", func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.String("ok") }, middleware.Cache(time.Minute))
	s.GET("/err", func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.SendStatus(ehttp.StatusNotFound) }, middleware.Cache(time.Minute))

	for _, p := range []string{"/", "/err"} {
		b.Run(p, func(b *testing.B) {
			bench(b, s, httptest.NewRequestWithContext(context.Background(), "GET", p, http.NoBody))
		})
	}
}

func BenchmarkCorsPreflight(b *testing.B) {
	s := ehttp.NewServer()
	s.Use(ehttp.Cors(ehttp.DefaultCorsOptions()))
	s.POST("/body", noop)

	req := httptest.NewRequestWithContext(context.Background(), "OPTIONS", "/body", http.NoBody)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type, x-token")
	bench(b, s, req)
}

func BenchmarkCorsSimple(b *testing.B) {
	s := ehttp.NewServer()
	s.Use(ehttp.Cors(ehttp.DefaultCorsOptions()))
	s.GET("/", noop)

	req := httptest.NewRequestWithContext(context.Background(), "GET", "/", http.NoBody)
	req.Header.Set("Origin", "http://localhost:3000")
	bench(b, s, req)
}

func BenchmarkMethodNotAllowed(b *testing.B) {
	s := ehttp.NewServer()
	s.POST("/x", noop)
	s.PUT("/x", noop)

	bench(b, s, httptest.NewRequestWithContext(context.Background(), "GET", "/x", http.NoBody))
}

// BenchmarkLastModified304 serves a 64 KB body to a conditional request that
// gets 304: the middleware must not copy the body.
func BenchmarkLastModified304(b *testing.B) {
	modified := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	body := strings.Repeat("x", 64<<10)
	s := ehttp.NewServer()
	s.GET("/", func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.String(body) }, middleware.LastModified(modified))

	req := httptest.NewRequestWithContext(context.Background(), "GET", "/", http.NoBody)
	req.Header.Set("If-Modified-Since", modified.Format(http.TimeFormat))
	bench(b, s, req)
}
