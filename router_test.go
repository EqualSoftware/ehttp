package ehttp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/EqualSoftware/ehttp/internal/ehttptest"
)

// aDo serves one in-memory request. kv is a flat list of header name/value pairs.
func aDo(h http.Handler, method, target string, kv ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, target, http.NoBody)

	for i := 0; i+1 < len(kv); i += 2 {
		req.Header.Set(kv[i], kv[i+1])
	}

	return ehttptest.Do(h, req)
}

func aText(body string) HandlerFunc {
	return func(res *ResponseWriter, _ *Request) { res.String(body) }
}

// aTrace returns a middleware that appends name to log before calling next.
func aTrace(log *[]string, name string) MiddlewareHandlerFunc {
	return func(next Handler) HandlerFunc {
		return func(res *ResponseWriter, req *Request) {
			*log = append(*log, name)
			next.ServeHTTP(res, req)
		}
	}
}

// aAllow returns the sorted members of the Allow header.
func aAllow(rec *httptest.ResponseRecorder) string {
	raw := rec.Header().Get(HeaderAllow)
	if raw == "" {
		return ""
	}

	parts := strings.Split(raw, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}

	sort.Strings(parts)

	return strings.Join(parts, ",")
}

func aMatchServer() *Server {
	s := NewServer()
	s.GET("/", aText("root"))
	s.GET("/users", aText("users"))
	s.GET("/users/me", aText("me"))
	s.GET("/users/:id", func(res *ResponseWriter, req *Request) { res.String("user:" + req.Param("id")) })
	s.GET("/items/:id|[0-9]+", func(res *ResponseWriter, req *Request) { res.String("item:" + req.Param("id")) })
	s.GET("/static/...", func(res *ResponseWriter, req *Request) { res.String("static:" + req.Param("...")) })
	s.GET("/about/", aText("about"))
	s.GET("/files/:name", func(res *ResponseWriter, req *Request) { res.String("file:" + req.Param("name")) })

	return s
}

func TestRouterMatching(t *testing.T) {
	t.Parallel()

	h := aMatchServer()

	tests := []struct {
		name   string
		basis  string
		target string
		code   int
		body   string
	}{
		{"root", "Express routing", "/", 200, "root"},
		{"static", "Express routing", "/users", 200, "users"},
		{"static before param wins by registration order", "Express: routes match in registration order", "/users/me", 200, "me"},
		{"param", "Express route parameters", "/users/7", 200, "user:7"},
		{"param does not match empty segment", "Express: :param needs at least one char", "/users//", 404, ""},
		{"param missing segment", "Express route parameters", "/files", 404, ""},
		{"extra segment is not matched", "Express routing", "/users/7/extra", 404, ""},
		{"regex param matches", "README :param|regex", "/items/42", 200, "item:42"},
		{"regex param rejects", "README :param|regex", "/items/abc", 404, ""},
		{"wildcard takes the rest of the path", "README /...", "/static/a/b.css", 200, "static:a/b.css"},
		{"wildcard single segment", "README /...", "/static/a", 200, "static:a"},
		{"wildcard needs a segment", "Express: /static/* does not match /static", "/static", 404, ""},
		{"trailing slash ignored on request", "Express: strict routing is off by default", "/users/", 200, "users"},
		{"trailing slash ignored on route", "Express: strict routing is off by default", "/about", 200, "about"},
		{"trailing slash on route and request", "Express: strict routing is off by default", "/about/", 200, "about"},
		{"query string is not part of the path", "RFC 3986 §3.4", "/users?x=1", 200, "users"},
		{"path is case sensitive", "net/http ServeMux", "/Users", 404, ""},
		{"empty inner segment never matches static", "Express routing", "/users//me", 404, ""},
		{"encoded slash stays inside one segment", "RFC 3986 §2.2 (%2F is data, not a separator)", "/files/a%2Fb", 200, "file:a%2Fb"},
		{"unknown path", "RFC 9110 §15.5.5", "/nope", 404, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := aDo(h, "GET", tt.target)
			ehttptest.Eq(t, tt.basis, rec.Code, tt.code)
			ehttptest.Eq(t, tt.basis, rec.Body.String(), tt.body)
		})
	}
}

func TestRouterRegistrationOrderDecidesStaticVsParam(t *testing.T) {
	t.Parallel()

	paramFirst := NewServer()
	paramFirst.GET("/users/:id", aText("param"))
	paramFirst.GET("/users/me", aText("static"))

	ehttptest.Eq(t, "Express: first registered route wins", aDo(paramFirst, "GET", "/users/me").Body.String(), "param")
}

func TestRouterRegexAnchoring(t *testing.T) {
	t.Parallel()

	s := NewServer()
	s.GET("/items/:id|[0-9]+", aText("ok"))

	for _, target := range []string{"/items/abc1", "/items/abc1;drop", "/items/42abc"} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()

			ehttptest.KnownBug(t, "S12", func(t testing.TB) {
				rec := aDo(s, "GET", target)
				ehttptest.Eq(t, "S12: regex must match the whole segment", rec.Code, 404)
			})
		})
	}
}

func TestRouterRoutePath(t *testing.T) {
	t.Parallel()

	s := NewServer()
	s.GET("/users/:id", func(res *ResponseWriter, req *Request) { res.String(req.RoutePath()) })
	api := s.Route("/api")
	api.GET("/items/:id", func(res *ResponseWriter, req *Request) { res.String(req.RoutePath()) })
	api.Route("/v1").GET("/ping", func(res *ResponseWriter, req *Request) { res.String(req.RoutePath()) })

	tests := []struct{ target, want string }{
		{"/users/9", "/users/:id"},
		{"/api/items/9", "/api/items/:id"},
		{"/api/v1/ping", "/api/v1/ping"},
	}

	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			t.Parallel()
			ehttptest.Eq(t, "net/http Request.Pattern", aDo(s, "GET", tt.target).Body.String(), tt.want)
		})
	}
}

func TestRouterMethods(t *testing.T) {
	t.Parallel()

	s := NewServer()
	s.router.Handle("get", "/lower", aText("lower"))
	s.router.Handle("PROPFIND", "/custom", aText("custom"))
	s.POST("/only-post", aText("posted"))
	s.GET("/get-only", aText("got"))
	s.GET("/multi", aText("g"))
	s.POST("/multi", aText("p"))
	s.GET("/with-head", aText("g"))
	s.router.Handle("HEAD", "/with-head", aText("h"))
	s.router.Handle("OPTIONS", "/with-options", aText("custom-options"))
	s.GET("/with-options", aText("g"))
	s.GET("/res/:id", aText("g"))
	h := s

	t.Run("method name is case-insensitive at registration", func(t *testing.T) {
		t.Parallel()
		ehttptest.Eq(t, "RFC 9110 §9.1 (methods are upper-case tokens)", aDo(h, "GET", "/lower").Body.String(), "lower")
	})

	t.Run("custom method", func(t *testing.T) {
		t.Parallel()
		ehttptest.Eq(t, "RFC 9110 §9.1 (extension methods)", aDo(h, "PROPFIND", "/custom").Body.String(), "custom")
	})

	allow := []struct {
		name, method, target, want string
	}{
		{"post-only, GET", "GET", "/only-post", "OPTIONS,POST"},
		{"post-only, HEAD", "HEAD", "/only-post", "OPTIONS,POST"},
		{"get-only, POST implies HEAD", "POST", "/get-only", "GET,HEAD,OPTIONS"},
		{"get-only, DELETE", "DELETE", "/get-only", "GET,HEAD,OPTIONS"},
		{"multi, PUT", "PUT", "/multi", "GET,HEAD,OPTIONS,POST"},
		{"explicit HEAD is not repeated", "PUT", "/with-head", "GET,HEAD,OPTIONS"},
		{"explicit OPTIONS is not repeated", "PUT", "/with-options", "GET,HEAD,OPTIONS"},
		{"param route", "PUT", "/res/3", "GET,HEAD,OPTIONS"},
		{"trailing slash", "POST", "/get-only/", "GET,HEAD,OPTIONS"},
	}

	for _, tt := range allow {
		t.Run("405 Allow "+tt.name, func(t *testing.T) {
			t.Parallel()

			rec := aDo(h, tt.method, tt.target)
			ehttptest.Eq(t, "RFC 9110 §15.5.6", rec.Code, 405)
			ehttptest.Eq(t, "RFC 9110 §15.5.6 (405 MUST carry Allow)", aAllow(rec), tt.want)
		})
	}

	t.Run("405 has no duplicate Allow members", func(t *testing.T) {
		t.Parallel()

		rec := aDo(h, "PUT", "/multi")
		members := strings.Split(rec.Header().Get(HeaderAllow), ",")
		seen := map[string]bool{}

		for _, m := range members {
			m = strings.TrimSpace(m)
			ehttptest.True(t, "RFC 9110 §10.2.1", !seen[m], "duplicate %q in Allow %q", m, rec.Header().Get(HeaderAllow))
			seen[m] = true
		}
	})

	t.Run("OPTIONS on a known route is answered automatically", func(t *testing.T) {
		t.Parallel()

		rec := aDo(h, "OPTIONS", "/get-only", "Origin", "https://a.example", HeaderAccessControlRequestMethod, "GET")
		ehttptest.Eq(t, "RFC 9110 §9.3.7 / Fetch §3.2.2 preflight", rec.Code, 200)
	})

	t.Run("OPTIONS does not become 405", func(t *testing.T) {
		t.Parallel()

		rec := aDo(h, "OPTIONS", "/only-post")
		ehttptest.True(t, "RFC 9110 §9.3.7", rec.Code != 405, "OPTIONS got 405")
	})

	t.Run("explicit OPTIONS handler runs", func(t *testing.T) {
		t.Parallel()
		ehttptest.Eq(t, "route registered for the method is used", aDo(h, "OPTIONS", "/with-options").Body.String(), "custom-options")
	})

	t.Run("OPTIONS response advertises Allow", func(t *testing.T) {
		t.Parallel()

		ehttptest.KnownBug(t, "NA-1", func(t testing.TB) {
			rec := aDo(h, "OPTIONS", "/get-only")
			ehttptest.Eq(t, "RFC 9110 §9.3.7 (SHOULD send Allow)", aAllow(rec), "GET,HEAD,OPTIONS")
		})
	})

	t.Run("HEAD runs the GET handler", func(t *testing.T) {
		t.Parallel()
		ehttptest.Eq(t, "RFC 9110 §9.3.2", aDo(h, "HEAD", "/get-only").Code, 200)
	})

	t.Run("explicit HEAD route wins over GET registered first", func(t *testing.T) {
		t.Parallel()

		ehttptest.KnownBug(t, "NA-2", func(t testing.TB) {
			ehttptest.Eq(t, "net/http ServeMux: HEAD pattern is more specific than GET", aDo(h, "HEAD", "/with-head").Body.String(), "h")
		})
	})
}

// HEAD must not carry a body on the wire and must keep the GET headers.
func TestRouterHeadOverRealServer(t *testing.T) {
	t.Parallel()

	s := NewServer()
	s.GET("/x", aText("hello"))
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	getReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/x", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	get, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatal(err)
	}
	defer get.Body.Close()

	headReq, err := http.NewRequestWithContext(t.Context(), http.MethodHead, ts.URL+"/x", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	head, err := http.DefaultClient.Do(headReq)
	if err != nil {
		t.Fatal(err)
	}
	defer head.Body.Close()

	body, _ := io.ReadAll(head.Body)
	ehttptest.Eq(t, "RFC 9110 §9.3.2 (no body)", len(body), 0)
	ehttptest.Eq(t, "RFC 9110 §9.3.2 (same status)", head.StatusCode, get.StatusCode)
	ehttptest.Eq(t, "RFC 9110 §9.3.2 (same Content-Length)", head.Header.Get(HeaderContentLength), get.Header.Get(HeaderContentLength))
	ehttptest.Eq(t, "RFC 9110 §9.3.2 (same Content-Type)", head.Header.Get(HeaderContentType), get.Header.Get(HeaderContentType))
}

func TestRouterMethodOrder(t *testing.T) {
	t.Parallel()

	s := NewServer()
	s.GET("/x", aText("get"))
	s.POST("/x", aText("post"))
	s.GET("/a/:id", aText("get-param"))
	s.POST("/a/new", aText("post-static"))
	h := s

	t.Run("method registered first", func(t *testing.T) {
		t.Parallel()
		ehttptest.Eq(t, "Express routing", aDo(h, "GET", "/x").Body.String(), "get")
	})

	t.Run("method registered later on the same path", func(t *testing.T) {
		t.Parallel()

		ehttptest.KnownBug(t, "C1", func(t testing.TB) {
			rec := aDo(h, "POST", "/x")
			ehttptest.Eq(t, "C1: the route of the right method must be found", rec.Code, 200)
			ehttptest.Eq(t, "C1", rec.Body.String(), "post")
		})
	})

	t.Run("static route of another method behind a param route", func(t *testing.T) {
		t.Parallel()

		ehttptest.KnownBug(t, "C1", func(t testing.TB) {
			ehttptest.Eq(t, "C1", aDo(h, "POST", "/a/new").Body.String(), "post-static")
		})
	})

	t.Run("method that no route has still gets 405", func(t *testing.T) {
		t.Parallel()
		ehttptest.Eq(t, "RFC 9110 §15.5.6", aDo(h, "DELETE", "/x").Code, 405)
	})
}

func TestRouterPathParamsCommittedOnlyOnFullMatch(t *testing.T) {
	t.Parallel()

	s := NewServer()
	s.GET("/users/:id/posts", aText("posts"))
	s.GET("/users/:name/comments", func(res *ResponseWriter, req *Request) {
		res.String("id=" + req.Param("id") + ";name=" + req.Param("name"))
	})

	rec := aDo(s, "GET", "/users/5/comments")
	ehttptest.Eq(t, "Express routing", rec.Code, 200)

	ehttptest.Eq(t, "route's own param is set", strings.Contains(rec.Body.String(), "name=5"), true)

	ehttptest.KnownBug(t, "C12", func(t testing.TB) {
		ehttptest.Eq(t, "C12: a route that did not match must not leave its params", rec.Body.String(), "id=;name=5")
	})
}

func TestRouterParamDecoding(t *testing.T) {
	t.Parallel()

	s := NewServer()
	s.GET("/hello/:name", func(res *ResponseWriter, req *Request) { res.String(req.Param("name")) })
	h := s

	t.Run("plain value", func(t *testing.T) {
		t.Parallel()
		ehttptest.Eq(t, "Express route parameters", aDo(h, "GET", "/hello/John").Body.String(), "John")
	})

	for _, tt := range []struct{ target, want string }{
		{"/hello/John%20Doe", "John Doe"},
		{"/hello/a%2Fb", "a/b"},
		{"/hello/caf%C3%A9", "café"},
	} {
		t.Run(tt.target, func(t *testing.T) {
			t.Parallel()

			ehttptest.KnownBug(t, "C10", func(t testing.TB) {
				ehttptest.Eq(t, "C10: params are decoded (Express, net/http PathValue)", aDo(h, "GET", tt.target).Body.String(), tt.want)
			})
		})
	}
}

func TestRouterMiddlewareScopeAndOrder(t *testing.T) {
	t.Parallel()

	t.Run("global then route", func(t *testing.T) {
		t.Parallel()

		var log []string

		s := NewServer()
		s.Use(aTrace(&log, "g1"), aTrace(&log, "g2"))
		s.GET("/x", func(res *ResponseWriter, _ *Request) {
			log = append(log, "handler")
			res.SendStatus(200)
		}, aTrace(&log, "r1"), aTrace(&log, "r2"))

		aDo(s, "GET", "/x")
		ehttptest.Eq(t, "Express: middleware runs in registration order", strings.Join(log, ">"), "g1>g2>r1>r2>handler")
	})

	t.Run("global then group then route", func(t *testing.T) {
		t.Parallel()

		var log []string

		s := NewServer()
		s.Use(aTrace(&log, "global"))
		g := s.Route("/api", aTrace(&log, "group"))
		g.GET("/x", func(res *ResponseWriter, _ *Request) {
			log = append(log, "handler")
			res.SendStatus(200)
		}, aTrace(&log, "route"))

		aDo(s, "GET", "/api/x")

		ehttptest.KnownBug(t, "S1", func(t testing.TB) {
			ehttptest.Eq(t, "S1: Route(prefix, mw...) applies mw to the group", strings.Join(log, ">"), "global>group>route>handler")
		})
	})

	t.Run("middleware that does not call next stops the chain", func(t *testing.T) {
		t.Parallel()

		ran := false
		s := NewServer()
		s.Use(func(next Handler) HandlerFunc {
			return func(res *ResponseWriter, _ *Request) { res.SendStatus(401) }
		})
		s.GET("/x", func(res *ResponseWriter, _ *Request) { ran = true })

		rec := aDo(s, "GET", "/x")
		ehttptest.Eq(t, "Express: next() is explicit", rec.Code, 401)
		ehttptest.Eq(t, "Express: next() is explicit", ran, false)
	})

	t.Run("Use only wraps routes registered after it", func(t *testing.T) {
		t.Parallel()

		var log []string

		s := NewServer()
		s.GET("/early", aText("early"))
		s.Use(aTrace(&log, "mw"))
		s.GET("/late", aText("late"))

		aDo(s, "GET", "/early")
		ehttptest.Eq(t, "Express: app.use only affects routes declared after it", len(log), 0)

		aDo(s, "GET", "/late")
		ehttptest.Eq(t, "Express: app.use only affects routes declared after it", len(log), 1)
	})

	t.Run("global middleware wraps 404, 405 and OPTIONS", func(t *testing.T) {
		t.Parallel()

		var log []string

		s := NewServer()
		s.Use(aTrace(&log, "mw"))
		s.POST("/x", aText("x"))
		h := s

		aDo(h, "GET", "/missing")
		aDo(h, "GET", "/x")
		aDo(h, "OPTIONS", "/x")
		ehttptest.Eq(t, "Express: app.use runs for unmatched requests", len(log), 3)
	})
}

func TestRouterGroups(t *testing.T) {
	t.Parallel()

	t.Run("prefix composition, nested", func(t *testing.T) {
		t.Parallel()

		s := NewServer()
		api := s.Route("/api")
		v1 := api.Route("/v1")
		v1.GET("/users", aText("v1-users"))
		v1.Route("/admin").POST("/x", aText("admin-x"))
		api.GET("/ping", aText("ping"))
		h := s

		ehttptest.Eq(t, "Express Router mount path", aDo(h, "GET", "/api/ping").Body.String(), "ping")
		ehttptest.Eq(t, "Express Router mount path", aDo(h, "GET", "/api/v1/users").Body.String(), "v1-users")
		ehttptest.Eq(t, "Express Router mount path", aDo(h, "POST", "/api/v1/admin/x").Body.String(), "admin-x")
		ehttptest.Eq(t, "Express Router mount path", aDo(h, "GET", "/v1/users").Code, 404)
	})

	t.Run("S1 group middleware runs", func(t *testing.T) {
		t.Parallel()

		ran := false
		s := NewServer()
		g := s.Route("/api", func(next Handler) HandlerFunc {
			return func(res *ResponseWriter, req *Request) { ran = true; next.ServeHTTP(res, req) }
		})
		g.GET("/x", aText("x"))

		aDo(s, "GET", "/api/x")

		ehttptest.KnownBug(t, "S1", func(t testing.TB) {
			ehttptest.True(t, "S1", ran, "group middleware never ran")
		})
	})

	t.Run("S1 nested group middleware runs", func(t *testing.T) {
		t.Parallel()

		var log []string
		s := NewServer()
		outer := s.Route("/a", aTrace(&log, "outer"))
		inner := outer.Route("/b", aTrace(&log, "inner"))
		inner.GET("/x", aText("x"))

		aDo(s, "GET", "/a/b/x")

		ehttptest.KnownBug(t, "S1", func(t testing.TB) {
			ehttptest.Eq(t, "S1: nested Route keeps outer and inner mw", strings.Join(log, ">"), "outer>inner")
		})
	})

	t.Run("S1 auth middleware on a group protects its routes", func(t *testing.T) {
		t.Parallel()

		auth := func(next Handler) HandlerFunc {
			return func(res *ResponseWriter, req *Request) {
				if req.Header("Authorization") == "" {
					res.SendStatus(401)
					return
				}

				next.ServeHTTP(res, req)
			}
		}

		s := NewServer()
		s.Route("/admin", auth).GET("/secret", aText("secret"))

		rec := aDo(s, "GET", "/admin/secret")

		ehttptest.KnownBug(t, "S1", func(t testing.TB) {
			ehttptest.Eq(t, "S1: unauthenticated request must be rejected", rec.Code, 401)
		})
	})

	t.Run("group middleware does not leak to routes outside the group", func(t *testing.T) {
		t.Parallel()

		var log []string
		s := NewServer()
		s.Route("/api", aTrace(&log, "group")).GET("/x", aText("x"))
		s.GET("/outside", aText("outside"))

		aDo(s, "GET", "/outside")
		ehttptest.Eq(t, "S1", len(log), 0)
	})

	t.Run("S2 sub.Use runs for routes of the group", func(t *testing.T) {
		t.Parallel()

		var log []string
		s := NewServer()
		g := s.Route("/api")
		g.Use(aTrace(&log, "mw"))
		g.GET("/x", aText("x"))

		aDo(s, "GET", "/api/x")
		ehttptest.Eq(t, "S2", len(log), 1)
	})

	t.Run("S2 sub.Use does not run for routes outside the group", func(t *testing.T) {
		t.Parallel()

		var log []string
		s := NewServer()
		g := s.Route("/api")
		g.Use(aTrace(&log, "mw"))
		g.GET("/x", aText("x"))
		s.GET("/outside", aText("outside"))

		aDo(s, "GET", "/outside")

		ehttptest.KnownBug(t, "S2", func(t testing.TB) {
			ehttptest.Eq(t, "S2: sub.Use is scoped to the group", len(log), 0)
		})
	})

	t.Run("S2 sub.Use does not run for 404", func(t *testing.T) {
		t.Parallel()

		var log []string
		s := NewServer()
		g := s.Route("/api")
		g.Use(aTrace(&log, "mw"))

		aDo(s, "GET", "/unknown")

		ehttptest.KnownBug(t, "S2", func(t testing.TB) {
			ehttptest.Eq(t, "S2: sub.Use is scoped to the group", len(log), 0)
		})
	})

	t.Run("S2 nested sub.Use does not run outside the nested group", func(t *testing.T) {
		t.Parallel()

		var log []string
		s := NewServer()
		outer := s.Route("/a")
		inner := outer.Route("/b")
		inner.Use(aTrace(&log, "inner"))
		inner.GET("/x", aText("x"))
		outer.GET("/y", aText("y"))

		aDo(s, "GET", "/a/y")

		ehttptest.KnownBug(t, "S2", func(t testing.TB) {
			ehttptest.Eq(t, "S2: inner.Use must not affect the outer group", len(log), 0)
		})
	})
}
