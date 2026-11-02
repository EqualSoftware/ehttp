package ehttp

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/EqualSoftware/ehttp/internal/ehttptest"
)

func aPreflight(h *Server, path string, kv ...string) (code int, header func(string) string) {
	kv = append([]string{HeaderOrigin, "https://a.example", HeaderAccessControlRequestMethod, "GET"}, kv...)
	rec := aDo(h, "OPTIONS", path, kv...)

	return rec.Code, rec.Header().Get
}

func TestCors(t *testing.T) {
	t.Parallel()

	t.Run("empty lists mean the wildcard", func(t *testing.T) {
		t.Parallel()

		s := NewServer()
		s.Use(Cors(CorsOptions{Origin: []string{"https://a.example"}}))
		s.GET("/x", aText("x"))

		code, header := aPreflight(s, "/x")

		ehttptest.Eq(t, "Fetch §3.2.3 (preflight response)", code, 200)
		ehttptest.Eq(t, "Fetch §3.2.3 (preflight response)", header(HeaderAccessControlAllowMethods), "*")
		ehttptest.Eq(t, "Fetch §3.2.3 (preflight response)", header(HeaderAccessControlAllowHeaders), "*")
	})

	t.Run("Credentials with the wildcard origin fails at boot", func(t *testing.T) {
		t.Parallel()

		msg := aPanics(func() { Cors(CorsOptions{Origin: []string{"*"}, Credentials: true}) })

		ehttptest.True(t, "Fetch §3.3.5: credentials never go with the * origin", msg != "", "Credentials:true with the * origin did not panic at boot")
	})

	t.Run("the options are copied", func(t *testing.T) {
		t.Parallel()

		origins := []string{"https://a.example"}
		s := NewServer()
		s.Use(Cors(CorsOptions{Origin: origins}))
		s.GET("/x", aText("x"))
		origins[0] = "https://changed.example"

		code, _ := aPreflight(s, "/x")

		ehttptest.Eq(t, "the middleware must not share the slices of the caller", code, 200)
	})

	// An OPTIONS response is not cacheable (RFC 9110 §9.3.7), so the preflight
	// cache is Access-Control-Max-Age only and never Cache-Control.
	t.Run("CacheDuration becomes Access-Control-Max-Age", func(t *testing.T) {
		t.Parallel()

		s := NewServer()
		s.Use(Cors(CorsOptions{Origin: []string{"*"}, CacheDuration: time.Hour}))

		_, header := aPreflight(s, "/x")

		ehttptest.Eq(t, "Fetch §3.2.2 (preflight cache)", header(HeaderAccessControlMaxAge), "3600")
	})

	t.Run("a preflight ends the chain", func(t *testing.T) {
		t.Parallel()

		var log []string

		s := NewServer()
		s.Use(Cors(CorsOptions{Origin: []string{"https://a.example"}}), aTrace(&log, "after"))
		s.GET("/x", aText("x"))

		code, _ := aPreflight(s, "/x")

		ehttptest.Eq(t, "go-chi/cors: a preflight is standalone and stops the chain", code, 200)
		ehttptest.Eq(t, "go-chi/cors: a preflight is standalone and stops the chain", len(log), 0)
	})

	t.Run("a refused preflight is 403 and ends the chain", func(t *testing.T) {
		t.Parallel()

		var log []string

		s := NewServer()
		s.Use(Cors(CorsOptions{Origin: []string{"https://b.example"}}), aTrace(&log, "after"))
		s.GET("/x", aText("x"))

		code, header := aPreflight(s, "/x")

		ehttptest.Eq(t, "Fetch §3.2.2 (a refused preflight)", code, 403)
		ehttptest.Eq(t, "Fetch §3.2.3 (no CORS header on a refusal)", header(HeaderAccessControlAllowOrigin), "")
		ehttptest.Eq(t, "go-chi/cors: a preflight is standalone and stops the chain", len(log), 0)
	})

	t.Run("a request that is not a preflight gets the headers and goes on", func(t *testing.T) {
		t.Parallel()

		s := NewServer()
		s.Use(Cors(CorsOptions{Origin: []string{"https://a.example"}, Credentials: true}))
		s.GET("/x", aText("x"))

		rec := aDo(s, "GET", "/x", HeaderOrigin, "https://a.example")

		ehttptest.Eq(t, "Fetch §3.2", rec.Body.String(), "x")
		ehttptest.Eq(t, "Fetch §3.2", rec.Header().Get(HeaderAccessControlAllowOrigin), "https://a.example")
		ehttptest.Eq(t, "Fetch §3.2", rec.Header().Get(HeaderAccessControlAllowCredentials), "true")
		ehttptest.Eq(t, "Fetch §3.2 (allow-methods only on a preflight)", rec.Header().Get(HeaderAccessControlAllowMethods), "")
	})

	t.Run("CORS is off without the middleware", func(t *testing.T) {
		t.Parallel()

		s := NewServer()
		s.GET("/x", aText("x"))

		rec := aDo(s, "GET", "/x", HeaderOrigin, "https://a.example")
		ehttptest.Eq(t, "Fetch §3.2", rec.Header().Get(HeaderAccessControlAllowOrigin), "")
		ehttptest.Eq(t, "Fetch §3.2", rec.Header().Get(HeaderVary), "")

		code, header := aPreflight(s, "/x")
		ehttptest.Eq(t, "RFC 9110 §9.3.7 (a plain OPTIONS answer)", code, 200)
		ehttptest.Eq(t, "Fetch §3.2", header(HeaderAccessControlAllowOrigin), "")
		ehttptest.Eq(t, "Fetch §3.2", header(HeaderAccessControlAllowMethods), "")
	})

	t.Run("it works in a group", func(t *testing.T) {
		t.Parallel()

		g := NewServer()
		api := g.Route("/api")
		api.Use(Cors(CorsOptions{Origin: []string{"https://a.example"}}))
		api.GET("/x", aText("x"))
		g.GET("/y", aText("y"))

		_, header := aPreflight(g, "/api/x")
		ehttptest.Eq(t, "Fetch §3.2.3", header(HeaderAccessControlAllowOrigin), "https://a.example")

		_, header = aPreflight(g, "/y")
		ehttptest.Eq(t, "the group middleware does not reach a route outside it", header(HeaderAccessControlAllowOrigin), "")
	})
}

func TestCorsRefusalLogHasNoSecrets(t *testing.T) {
	t.Parallel()

	l, logs := aObserved()
	s := NewServer(WithLogger(l))
	s.Use(Cors(CorsOptions{Origin: []string{"https://b.example"}}))

	aDo(s, "OPTIONS", "/x",
		HeaderOrigin, "https://a.example",
		HeaderAccessControlRequestMethod, "GET",
		HeaderAccessControlRequestHeaders, "Authorization, X-Token",
		"Authorization", "Bearer secret-token",
		"Cookie", "sid=secret-cookie")

	ehttptest.Eq(t, "one refusal is logged", logs.Len(), 1)

	e := logs.All()[0]
	ehttptest.Eq(t, "the origin is logged", e.ContextMap()["origin"], any("https://a.example"))
	ehttptest.Eq(t, "the requested method is logged", e.ContextMap()["request_method"], any("GET"))
	ehttptest.Eq(t, "the requested header names are logged", fmt.Sprint(e.ContextMap()["request_headers"]), "[Authorization X-Token]")

	ehttptest.True(t, "the log has no secret value", !strings.Contains(fmt.Sprint(e.ContextMap()), "secret"), "log: %v", e.ContextMap())
}

func TestCorsOptionsChecks(t *testing.T) {
	t.Parallel()

	t.Run("an empty Origin list panics", func(t *testing.T) {
		t.Parallel()

		msg := aPanics(func() { Cors(CorsOptions{Methods: []string{"GET"}}) })

		ehttptest.True(t, "the message says what, where and the fix", strings.HasPrefix(msg, "ehttp: an empty Origin list in Cors: ") &&
			strings.Contains(msg, "ehttp.Cors(ehttp.DefaultCorsOptions())"), "message %q", msg)
	})

	t.Run("DefaultCorsOptions allows all and is a new copy", func(t *testing.T) {
		t.Parallel()

		d := DefaultCorsOptions()
		d.Origin[0] = "https://changed.example"

		ehttptest.Eq(t, "a call returns a fresh copy", DefaultCorsOptions().Origin[0], "*")

		s := NewServer()
		s.Use(Cors(DefaultCorsOptions()))

		code, header := aPreflight(s, "/x", HeaderAccessControlRequestHeaders, "X-Any")
		ehttptest.Eq(t, "Fetch §3.2.3", code, 200)
		ehttptest.Eq(t, "Fetch §3.2.3", header(HeaderAccessControlAllowOrigin), "*")
	})

	t.Run("header names are compared in canonical form", func(t *testing.T) {
		t.Parallel()

		s := NewServer()
		s.Use(Cors(CorsOptions{Origin: []string{"https://a.example"}, Headers: []string{"content-type"}}))

		code, header := aPreflight(s, "/x", HeaderAccessControlRequestHeaders, "Content-Type")
		ehttptest.Eq(t, "RFC 9110 §5.1 (names are case-insensitive)", code, 200)
		ehttptest.Eq(t, "RFC 9110 §5.1 (names are case-insensitive)", header(HeaderAccessControlAllowHeaders), "Content-Type")
	})
}
