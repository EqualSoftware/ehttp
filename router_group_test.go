package ehttp

import (
	"net/http"
	"strings"
	"testing"

	"github.com/EqualSoftware/ehttp/internal/ehttptest"
)

// aAuth refuses a request with no Authorization header, as an authentication
// middleware does.
func aAuth(log *[]string) MiddlewareHandlerFunc {
	return func(next Handler) HandlerFunc {
		return func(res *ResponseWriter, req *Request) {
			*log = append(*log, "auth")

			if req.Header("Authorization") == "" {
				res.SendStatus(StatusUnauthorized)
				return
			}

			next.ServeHTTP(res, req)
		}
	}
}

func TestRouterAutomaticAnswersRunGroupMiddleware(t *testing.T) {
	t.Parallel()

	build := func() (*Server, *[]string) {
		log := new([]string)
		s := NewServer()
		s.Use(aTrace(log, "root"))

		api := s.Route("/api", aTrace(log, "api"))
		api.GET("/x", aText("x"))

		inner := api.Route("/v1", aTrace(log, "v1"))
		inner.GET("/y", aText("y"))

		s.GET("/top", aText("top"))
		s.GET("/mixed", aText("mixed"))
		api.POST("/mixed", aText("mixed"))

		return s, log
	}

	cases := []struct {
		name, method, path string
		code               int
		want               []string
	}{
		{"405 in a group", "POST", "/api/x", 405, []string{"root", "api"}},
		{"405 in a nested group", "POST", "/api/v1/y", 405, []string{"root", "api", "v1"}},
		{"405 at the root", "POST", "/top", 405, []string{"root"}},
		{"405 across the root and a group", "PUT", "/mixed", 405, []string{"root"}},
		{"OPTIONS in a group", "OPTIONS", "/api/x", 200, []string{"root", "api"}},
		{"OPTIONS across the root and a group", "OPTIONS", "/mixed", 200, []string{"root"}},
		{"404 under a group", "GET", "/api/none", 404, []string{"root", "api"}},
		{"404 under a nested group", "GET", "/api/v1/none", 404, []string{"root", "api", "v1"}},
		{"404 outside the groups", "GET", "/none", 404, []string{"root"}},
		{"OPTIONS of an unknown path", "OPTIONS", "/api/none", 404, []string{"root", "api"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, log := build()
			rec := aDo(s, tc.method, tc.path)

			ehttptest.Eq(t, "chi, Fiber, Express and Echo run the group middleware here", rec.Code, tc.code)
			ehttptest.Eq(t, "chi, Fiber, Express and Echo run the group middleware here", strings.Join(*log, ","), strings.Join(tc.want, ","))
		})
	}
}

func TestRouterPlainOptions(t *testing.T) {
	t.Parallel()

	var log []string

	s := NewServer()
	api := s.Route("/api", aTrace(&log, "api"))
	api.GET("/x", aText("x"))
	api.POST("/x", aText("x"))

	rec := aDo(s, "OPTIONS", "/api/x", HeaderOrigin, "https://a.example")

	ehttptest.Eq(t, "200 for old clients (RFC 9110 §9.3.7 allows 200 and 204)", rec.Code, http.StatusOK)
	ehttptest.Eq(t, "RFC 9110 §10.2.1", aAllow(rec), "GET,HEAD,OPTIONS,POST")
	ehttptest.Eq(t, "RFC 9110 §9.3.7", rec.Header().Get(HeaderContentLength), "0")
	ehttptest.Eq(t, "RFC 9110 §9.3.7", rec.Body.Len(), 0)
	ehttptest.Eq(t, "group middleware runs", strings.Join(log, ","), "api")
	ehttptest.Eq(t, "no CORS validation", rec.Header().Get(HeaderAccessControlAllowOrigin), "")
}

func TestRouterRefusalSkipsGroupMiddleware(t *testing.T) {
	t.Parallel()

	var log []string

	s := NewServer()
	api := s.Route("/api", aTrace(&log, "api"))
	api.GET("/:name", aText("x"))

	rec := aDo(s, "POST", "/api/%2e%2e")

	ehttptest.Eq(t, "RFC 9110 §15.5.4", rec.Code, 403)
	ehttptest.Eq(t, "a refused path runs no group middleware", len(log), 0)
}

func TestRouterUseAfterRoute(t *testing.T) {
	t.Parallel()

	const want = "ehttp: a call after a route was registered in Use: middleware registered after a route does not wrap that route. Fix: call Use before the routes."

	noop := aTrace(new([]string), "x")

	t.Run("on the root after a root route", func(t *testing.T) {
		t.Parallel()

		s := NewServer()
		s.GET("/x", aText("x"))

		ehttptest.Eq(t, "go-chi mux.go: all middlewares must be defined before routes on a mux", aPanics(func() { s.Use(noop) }), want)
	})

	t.Run("on the root after a group route", func(t *testing.T) {
		t.Parallel()

		s := NewServer()
		s.Route("/api").GET("/x", aText("x"))

		ehttptest.Eq(t, "go-chi mux.go: all middlewares must be defined before routes on a mux", aPanics(func() { s.Use(noop) }), want)
	})

	t.Run("on a group after a route of the group", func(t *testing.T) {
		t.Parallel()

		g := NewServer().Route("/api")
		g.GET("/x", aText("x"))

		ehttptest.Eq(t, "go-chi mux.go: all middlewares must be defined before routes on a mux", aPanics(func() { g.Use(noop) }), want)
	})

	t.Run("on a group after a route of a subgroup", func(t *testing.T) {
		t.Parallel()

		g := NewServer().Route("/api")
		g.Route("/v1").GET("/x", aText("x"))

		ehttptest.Eq(t, "the route of the subgroup would not get the middleware", aPanics(func() { g.Use(noop) }), want)
	})

	t.Run("a group or the root without a route accepts Use", func(t *testing.T) {
		t.Parallel()

		s := NewServer()
		g := s.Route("/api")
		g.Use(noop)
		s.Use(noop)
		g.GET("/x", aText("x"))

		ehttptest.Eq(t, "no panic", aPanics(func() { s.Route("/other").Use(noop) }), "")
	})
}

func TestRouterPreflightAndAuth(t *testing.T) {
	t.Parallel()

	opts := CorsOptions{Origin: []string{"https://a.example"}}

	t.Run("Cors on the root ends a preflight before the auth of a group", func(t *testing.T) {
		t.Parallel()

		var log []string

		s := NewServer()
		s.Use(Cors(opts))
		api := s.Route("/api", aAuth(&log))
		api.GET("/x", aText("x"))

		code, header := aPreflight(s, "/api/x")

		ehttptest.Eq(t, "go-chi/cors: a preflight stops the chain", code, 200)
		ehttptest.Eq(t, "go-chi/cors: a preflight stops the chain", len(log), 0)
		ehttptest.Eq(t, "Fetch §3.2.3", header(HeaderAccessControlAllowOrigin), "https://a.example")
	})

	t.Run("auth before Cors refuses the preflight and Development warns", func(t *testing.T) {
		t.Parallel()

		var log []string

		l, logs := aObserved()
		s := NewServer(WithLogger(l), WithMode(Development))
		s.Use(aAuth(&log), Cors(opts))
		s.GET("/x", aText("x"))

		code, _ := aPreflight(s, "/x")

		ehttptest.Eq(t, "the auth middleware answered", code, 401)
		ehttptest.Eq(t, "one warning", logs.Len(), 1)

		e := logs.All()[0]
		ehttptest.Eq(t, "the warning has the status", e.ContextMap()["status"], any(int64(401)))
		ehttptest.Eq(t, "the warning has the fix", e.ContextMap()["fix"], any("register the CORS middleware before the authentication middleware"))
	})

	t.Run("Production does not warn", func(t *testing.T) {
		t.Parallel()

		var log []string

		l, logs := aObserved()
		s := NewServer(WithLogger(l))
		s.Use(aAuth(&log), Cors(opts))
		s.GET("/x", aText("x"))

		code, _ := aPreflight(s, "/x")

		ehttptest.Eq(t, "the auth middleware answered", code, 401)
		ehttptest.Eq(t, "no warning", logs.Len(), 0)
	})

	t.Run("a request that is not a preflight does not warn", func(t *testing.T) {
		t.Parallel()

		var log []string

		l, logs := aObserved()
		s := NewServer(WithLogger(l), WithMode(Development))
		s.Use(aAuth(&log), Cors(opts))
		s.GET("/x", aText("x"))

		aDo(s, "GET", "/x")
		aDo(s, "OPTIONS", "/x")

		ehttptest.Eq(t, "no warning", logs.Len(), 0)
	})
}

func TestRouterAllowNeedsTheChain(t *testing.T) {
	t.Parallel()

	t.Run("a 401 of the group auth sends no Allow", func(t *testing.T) {
		t.Parallel()

		var log []string

		s := NewServer()
		admin := s.Route("/admin", aAuth(&log))
		admin.GET("/x", aText("x"))
		admin.GET("/u/:id", aText("x"))

		for _, path := range []string{"/admin/x", "/admin/u/5"} {
			rec := aDo(s, "OPTIONS", path)

			ehttptest.Eq(t, "the group auth answers", rec.Code, 401)
			ehttptest.Eq(t, "RFC 9110 §10.2.1: no method list for a refused request", rec.Header().Get(HeaderAllow), "")
		}
	})

	t.Run("a refused preflight sends no Allow", func(t *testing.T) {
		t.Parallel()

		s := NewServer()
		s.Use(Cors(CorsOptions{Origin: []string{"https://b.example"}}))
		s.GET("/x", aText("x"))

		rec := aDo(s, "OPTIONS", "/x", HeaderOrigin, "https://a.example", HeaderAccessControlRequestMethod, "GET")

		ehttptest.Eq(t, "Fetch §3.2.2", rec.Code, 403)
		ehttptest.Eq(t, "RFC 9110 §10.2.1: no method list for a refused request", rec.Header().Get(HeaderAllow), "")
	})
}

func TestRouterGroupSelection(t *testing.T) {
	t.Parallel()

	build := func() (*Server, *[]string) {
		log := new([]string)
		s := NewServer()
		s.Use(aTrace(log, "root"))

		admin := s.Route("/admin", aTrace(log, "admin"))
		admin.GET("/x", aText("x"))

		user := s.Route("/u/:id", aTrace(log, "user"))
		user.GET("/x", aText("x"))

		admin.Route("/deep", aTrace(log, "deep")).GET("/y", aText("y"))

		s.GET("/administrator", aText("a"))

		return s, log
	}

	cases := []struct{ name, method, path, want string }{
		{"a longer first segment is not the prefix", "POST", "/administrator", "root"},
		{"404 of a longer first segment", "GET", "/administrators/x", "root"},
		{"a parameter prefix", "GET", "/u/5/none", "root,user"},
		{"a parameter prefix, 405", "POST", "/u/5/x", "root,user"},
		{"the prefix alone", "GET", "/admin", "root,admin"},
		{"a trailing slash", "GET", "/admin/none/", "root,admin"},
		{"an encoded prefix is another segment", "GET", "/%61dmin/zz", "root"},
		{"a nested group", "GET", "/admin/deep/none", "root,admin,deep"},
		{"the outer group beside the nested one", "GET", "/admin/other", "root,admin"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, log := build()
			aDo(s, tc.method, tc.path)

			ehttptest.Eq(t, "the innermost group whose prefix matches runs after the root", strings.Join(*log, ","), tc.want)
		})
	}
}

func TestRouterGroupWithRootRouteOnSamePath(t *testing.T) {
	t.Parallel()

	var log []string

	s := NewServer()
	s.GET("/shared", aText("root"))
	s.Route("/shared", aTrace(&log, "group")).POST("", aText("group"))

	rec := aDo(s, "OPTIONS", "/shared")

	ehttptest.Eq(t, "the routes of the root and of a group: the root chain only", len(log), 0)
	ehttptest.Eq(t, "Allow lists both routes", aAllow(rec), "GET,HEAD,OPTIONS,POST")
}

func TestRouterSecondGroupWithSamePrefixPanics(t *testing.T) {
	t.Parallel()

	t.Run("siblings", func(t *testing.T) {
		t.Parallel()

		s := NewServer()
		s.Route("/api/:id")

		msg := aPanics(func() { s.Route("/api/:name") })

		ehttptest.True(t, "what, where, why and fix", strings.HasPrefix(msg, "ehttp: a second group in Route \"/api/:name\": ") &&
			strings.Contains(msg, "cannot pick one group middleware chain") &&
			strings.Contains(msg, "router.GET(pattern, handler, middleware)"), "message %q", msg)
	})

	t.Run("the same full prefix by another path", func(t *testing.T) {
		t.Parallel()

		s := NewServer()
		s.Route("/a/b")

		ehttptest.True(t, "the full prefix counts", aPanics(func() { s.Route("/a").Route("/b") }) != "", "no panic")
	})

	t.Run("a different regex is another prefix", func(t *testing.T) {
		t.Parallel()

		s := NewServer()
		s.Route("/api/:id|[0-9]+")

		ehttptest.Eq(t, "no panic", aPanics(func() { s.Route("/api/:id|[a-z]+") }), "")
	})

	t.Run("the empty prefix and a slash are the same prefix", func(t *testing.T) {
		t.Parallel()

		s := NewServer()
		s.Route("")

		ehttptest.True(t, "both match every path", aPanics(func() { s.Route("/") }) != "", "no panic")
	})

	t.Run("a nested group is not a sibling", func(t *testing.T) {
		t.Parallel()

		s := NewServer()
		api := s.Route("/api")

		ehttptest.Eq(t, "no panic", aPanics(func() { api.Route("") }), "")
	})
}

func TestRouterRefusalGivesNoPreflightWarning(t *testing.T) {
	t.Parallel()

	l, logs := aObserved()
	s := NewServer(WithLogger(l), WithMode(Development))
	s.GET("/p/:v", aText("x"))

	rec := aDo(s, "OPTIONS", "/p/%2e%2e", HeaderOrigin, "https://a.example", HeaderAccessControlRequestMethod, "GET")

	ehttptest.Eq(t, "RFC 9110 §15.5.4", rec.Code, 403)
	ehttptest.Eq(t, "the router refused, not a middleware", logs.Len(), 0)
}
