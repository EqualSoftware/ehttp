package ehttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

const strictBasis = "strict errors by mode: Development panics, Production logs"

// aServe serves one GET /x request through a server in mode, with h as the
// handler and mws as route middlewares. It returns the response and the
// log entries. A panic of the handler is returned as msg.
func aServe(mode Mode, h HandlerFunc, mws ...MiddlewareHandlerFunc) (rec *httptest.ResponseRecorder, logs *observer.ObservedLogs, msg string) {
	l, logs := aObserved()
	s := NewServer(WithLogger(l), WithMode(mode))
	s.GET("/x", h, mws...)

	rec = httptest.NewRecorder()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
	msg = aPanics(func() { s.ServeHTTP(rec, r) })

	return rec, logs, msg
}

// aPart checks the four parts of a strict message.
func aPart(t *testing.T, msg, what, why string) {
	t.Helper()

	ehttptest.True(t, strictBasis, strings.HasPrefix(msg, "ehttp: "+what), "message %q does not start with the call %q", msg, what)
	ehttptest.True(t, strictBasis, strings.Contains(msg, " on GET /x: "), "message %q has no \"on GET /x\"", msg)
	ehttptest.True(t, strictBasis, strings.Contains(msg, why), "message %q has no reason %q", msg, why)
	ehttptest.True(t, strictBasis, strings.Contains(msg, ". Fix: ") && strings.HasSuffix(msg, "."), "message %q has no Fix part", msg)
}

func TestStrictErrorsByMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		rule  string
		level zapcore.Level
		what  string // start of the message
		why   string // part of the message
		do    HandlerFunc
		check func(t *testing.T, rec *httptest.ResponseRecorder) // the fallback
	}{
		{"invalid name", "invalid header name", zapcore.ErrorLevel, `res.Set("Bad Name", ...)`, `is not a token: it has ' ' (0x20) at position 3 (RFC 9110 §5.6.2)`,
			func(res *ResponseWriter, _ *Request) { res.Set("Bad Name", "v"); res.String("ok") },
			func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				ehttptest.Eq(t, strictBasis, rec.Result().Header.Get("Bad Name"), "")
			}},
		{"empty name", "invalid header name", zapcore.ErrorLevel, `res.Remove("")`, `is not a token: it is empty`,
			func(res *ResponseWriter, _ *Request) { res.Remove(""); res.String("ok") }, nil},
		{"invalid value", "invalid header value", zapcore.ErrorLevel, `res.Set("X-A", ...)`, `the value "a\nb", which has the byte '\n' (0x0a) at position 1 (RFC 9110 §5.5)`,
			func(res *ResponseWriter, _ *Request) { res.Set("X-A", "a\nb"); res.String("ok") },
			func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				ehttptest.Eq(t, strictBasis, rec.Result().Header.Get("X-A"), "")
			}},
		{"empty member", "empty list member", zapcore.WarnLevel, `res.Append("X-A", ...)`, `has an empty member (RFC 9110 §5.6.1.1)`,
			func(res *ResponseWriter, _ *Request) { res.Append("X-A", "a", "", "b"); res.String("ok") },
			func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				ehttptest.Eq(t, strictBasis, rec.Result().Header.Get("X-A"), "a, b")
			}},
		{"singleton", "Append on a single-value field", zapcore.WarnLevel, `res.Append("Location", ...)`, `Location is a single-value field (RFC 9110 §10.2.2)`,
			func(res *ResponseWriter, _ *Request) { res.Append("Location", "/a"); res.String("ok") },
			func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				ehttptest.Eq(t, strictBasis, rec.Result().Header.Get("Location"), "/a")
			}},
		{"Set Set-Cookie", "Set-Cookie has its own method", zapcore.WarnLevel, `res.Set("Set-Cookie", ...)`, `(RFC 6265 §3)`,
			func(res *ResponseWriter, _ *Request) { res.Set("Set-Cookie", "a=1"); res.String("ok") },
			func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				ehttptest.Eq(t, strictBasis, len(rec.Result().Header.Values("Set-Cookie")), 0)
			}},
		{"Append Set-Cookie", "Set-Cookie has its own method", zapcore.WarnLevel, `res.Append("set-cookie", ...)`, `(RFC 6265 §3)`,
			func(res *ResponseWriter, _ *Request) { res.Append("set-cookie", "a=1"); res.String("ok") }, nil},
		{"Set Vary", "Vary has its own method", zapcore.WarnLevel, `res.Set("Vary", ...)`, `(RFC 9110 §12.5.5)`,
			func(res *ResponseWriter, _ *Request) { res.Vary("A"); res.Set("Vary", "b"); res.String("ok") },
			func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				ehttptest.Eq(t, strictBasis, rec.Result().Header.Get("Vary"), "A")
			}},
		{"Append Vary", "Vary has its own method", zapcore.WarnLevel, `res.Append("Vary", ...)`, `(RFC 9110 §12.5.5)`,
			func(res *ResponseWriter, _ *Request) { res.Append("Vary", "b"); res.String("ok") }, nil},
		{"Remove Vary", "Vary has no Remove", zapcore.WarnLevel, `res.Remove("Vary")`, `(RFC 9110 §12.5.5)`,
			func(res *ResponseWriter, _ *Request) { res.Vary("A"); res.Remove("Vary"); res.String("ok") },
			func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				ehttptest.Eq(t, strictBasis, rec.Result().Header.Get("Vary"), "A")
			}},
		{"Set after sent", "change after the header was sent", zapcore.WarnLevel, `res.Set("X-A", ...)`, `the header was already sent`,
			func(res *ResponseWriter, _ *Request) { res.String("ok"); res.Set("X-A", "1") },
			func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				ehttptest.Eq(t, strictBasis, rec.Result().Header.Get("X-A"), "")
			}},
		{"Location after sent", "change after the header was sent", zapcore.WarnLevel, `res.Location(...)`, `the header was already sent`,
			func(res *ResponseWriter, _ *Request) { res.String("ok"); res.Location("/y") }, nil},
		{"Status after sent", "change after the header was sent", zapcore.WarnLevel, `res.Status(...)`, `the header was already sent`,
			func(res *ResponseWriter, _ *Request) { res.String("ok"); res.Status(404) },
			func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				ehttptest.Eq(t, strictBasis, rec.Code, 200)
			}},
		{"SendStatus after sent", "change after the header was sent", zapcore.WarnLevel, `res.SendStatus(...)`, `the header was already sent`,
			func(res *ResponseWriter, _ *Request) { res.String("ok"); res.SendStatus(404) },
			func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				ehttptest.Eq(t, strictBasis, rec.Code, 200)
			}},
		{"Cookie after sent", "change after the header was sent", zapcore.WarnLevel, `res.Cookie(...)`, `the header was already sent`,
			func(res *ResponseWriter, _ *Request) {
				res.String("ok")
				res.Cookie(&http.Cookie{Name: "a", Value: "1"})
			},
			func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				ehttptest.Eq(t, strictBasis, len(rec.Result().Header.Values("Set-Cookie")), 0)
			}},
		{"second body", "second body", zapcore.WarnLevel, `res.String(...)`, `a response has one body (RFC 9110 §6.4.1)`,
			func(res *ResponseWriter, _ *Request) { res.String("one"); res.String("two") },
			func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				ehttptest.Eq(t, strictBasis, rec.Body.String(), "one")
			}},
		{"nil cookie", "nil cookie", zapcore.WarnLevel, `res.Cookie(nil)`, `the cookie is nil`,
			func(res *ResponseWriter, _ *Request) { res.Cookie(nil); res.String("ok") },
			func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				ehttptest.Eq(t, strictBasis, len(rec.Result().Header.Values("Set-Cookie")), 0)
			}},
		{"invalid cookie", "invalid cookie", zapcore.WarnLevel, `res.Cookie("bad name")`, `is invalid`,
			func(res *ResponseWriter, _ *Request) {
				res.Cookie(&http.Cookie{Name: "bad name", Value: "1"})
				res.String("ok")
			},
			func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				ehttptest.Eq(t, strictBasis, len(rec.Result().Header.Values("Set-Cookie")), 0)
			}},
		{"Status 600", "status code outside 100-599", zapcore.ErrorLevel, `res.Status(600)`, `status code 600 is outside 100-599 (RFC 9110 §15)`,
			func(res *ResponseWriter, _ *Request) { res.Status(600); res.String("ok") },
			func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				ehttptest.Eq(t, strictBasis, rec.Code, 500)
			}},
		{"SendStatus 0", "status code outside 100-599", zapcore.ErrorLevel, `res.SendStatus(0)`, `status code 0 is outside 100-599`,
			func(res *ResponseWriter, _ *Request) { res.SendStatus(0) },
			func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				ehttptest.Eq(t, strictBasis, rec.Code, 500)
			}},
		{"Status 1xx", "informational status is not final", zapcore.ErrorLevel, `res.Status(103)`, `informational and not final (RFC 9110 §15.2)`,
			func(res *ResponseWriter, _ *Request) { res.Status(103); res.String("ok") },
			func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				ehttptest.Eq(t, strictBasis, rec.Code, 500)
			}},
		{"SendStatus 1xx", "informational status is not final", zapcore.ErrorLevel, `res.SendStatus(100)`, `informational and not final`,
			func(res *ResponseWriter, _ *Request) { res.SendStatus(100) },
			func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				ehttptest.Eq(t, strictBasis, rec.Code, 500)
			}},
		{"native WriteHeader 700", "status code outside 100-599", zapcore.ErrorLevel, `res.WriteHeader(700)`, `status code 700 is outside 100-599`,
			WrapHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(700) })),
			func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				ehttptest.Eq(t, strictBasis, rec.Code, 500)
			}},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/Development", func(t *testing.T) {
			t.Parallel()

			_, _, msg := aServe(Development, tt.do)

			aPart(t, msg, tt.what, tt.why)
		})

		t.Run(tt.name+"/Production", func(t *testing.T) {
			t.Parallel()

			rec, logs, msg := aServe(Production, tt.do)

			ehttptest.Eq(t, strictBasis+" (no panic in Production)", msg, "")

			if tt.check != nil {
				tt.check(t, rec)
			}

			all := logs.FilterField(zap.String("rule", tt.rule)).All()
			ehttptest.Eq(t, strictBasis+" (one log entry for the rule)", len(all), 1)

			if len(all) != 1 {
				return
			}

			e := all[0]
			ehttptest.Eq(t, strictBasis+" (level)", e.Level, tt.level)
			aPart(t, e.Message, tt.what, tt.why)
			ehttptest.True(t, strictBasis+" (fix field)", e.ContextMap()["fix"] != nil, "no fix field in %v", e.ContextMap())
			ehttptest.Eq(t, strictBasis+" (one route field)", countKey(e, "route"), 1)
			ehttptest.Eq(t, strictBasis+" (route)", e.ContextMap()["route"], any("/x"))
		})
	}
}

func countKey(e observer.LoggedEntry, key string) (n int) {
	for _, f := range e.Context {
		if f.Key == key {
			n++
		}
	}

	return n
}

func TestStrictInvalidValueIsTruncated(t *testing.T) {
	t.Parallel()

	value := strings.Repeat("a", 100) + "\n"
	do := func(res *ResponseWriter, _ *Request) { res.Set("X-A", value); res.String("ok") }

	_, _, msg := aServe(Development, do)
	ehttptest.True(t, strictBasis+" (64 bytes)", !strings.Contains(msg, strings.Repeat("a", 65)), "message keeps more than 64 bytes: %q", msg)
	ehttptest.True(t, strictBasis+" (marks the cut)", strings.Contains(msg, `..."`) || strings.Contains(msg, `"...`), "message %q", msg)
	ehttptest.True(t, strictBasis+" (bad byte and position)", strings.Contains(msg, "at position 100"), "message %q", msg)

	_, logs, _ := aServe(Production, do)
	got, _ := logs.All()[0].ContextMap()["value"].(string)
	ehttptest.True(t, strictBasis+" (value field)", len(got) <= 1+64+1+len("..."), "value field has %d bytes", len(got))
	ehttptest.Eq(t, strictBasis+" (header field)", logs.All()[0].ContextMap()["header"], any("X-A"))
}

func TestStrictExactMessage(t *testing.T) {
	t.Parallel()

	_, _, msg := aServe(Development, func(res *ResponseWriter, _ *Request) { res.Append("Location", "/y") })

	ehttptest.Eq(t, strictBasis, msg, `ehttp: res.Append("Location", ...) on GET /x: Location is a single-value field (RFC 9110 §10.2.2). Fix: use res.Set("Location", value).`)
}

func TestStrictNoRouteUsesPath(t *testing.T) {
	t.Parallel()

	l, logs := aObserved()
	s := NewServer(WithLogger(l))
	s.Use(func(next Handler) HandlerFunc {
		return func(res *ResponseWriter, req *Request) { res.Set("Bad Name", "v"); next.ServeHTTP(res, req) }
	})

	s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/nope", http.NoBody))

	e := logs.All()[0]
	ehttptest.Eq(t, strictBasis+" (a 404 has no pattern: the path is the route)", e.ContextMap()["route"], any("/nope"))
	ehttptest.Eq(t, strictBasis+" (one route field)", countKey(e, "route"), 1)
	ehttptest.True(t, strictBasis, strings.Contains(e.Message, " on GET /nope: "), "message %q", e.Message)
}

func TestStrictAppendSingletonKeepsLastValue(t *testing.T) {
	t.Parallel()

	rec, _, _ := aServe(Production, func(res *ResponseWriter, _ *Request) { res.Append("Location", "/a", "/b"); res.String("ok") })

	ehttptest.Eq(t, strictBasis+" (same as two Set calls)", rec.Header().Get("Location"), "/b")
}

func TestBootMessagesHaveTheFormat(t *testing.T) {
	t.Parallel()

	tests := map[string]func(){
		"cors origin":      func() { Cors(CorsOptions{Origin: []string{"a\nb"}}) },
		"cors credentials": func() { Cors(CorsOptions{Origin: []string{"*"}, Credentials: true}) },
		"unknown method":   func() { NewServer().router.Handle("PROPFIND", "/", func(*ResponseWriter, *Request) {}) },
		"route method":     func() { NewServer().router.Handle("GE T", "/", func(*ResponseWriter, *Request) {}) },
		"route regex":      func() { NewServer().router.GET("/:id|(", func(*ResponseWriter, *Request) {}) },
		"trusted proxy":    func() { WithTrustedProxies("nope") },
		"timeout":          func() { WithReadTimeout(-1) },
		"max header bytes": func() { WithMaxHeaderBytes(-1) },
		"nil logger":       func() { WithLogger(nil) },
		"cors max age":     func() { Cors(CorsOptions{Origin: []string{"*"}, CacheDuration: -time.Second}) },
		"duplicate route": func() {
			s := NewServer()
			s.GET("/a", aText("1"))
			s.GET("/a", aText("2"))
		},
	}

	for name, do := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			msg := aPanics(do)

			ehttptest.True(t, strictBasis+" (boot panics)", strings.HasPrefix(msg, "ehttp: "), "message %q", msg)
			ehttptest.True(t, strictBasis+" (where: the option)", strings.Contains(msg, " in "), "message %q has no \"in <option>\"", msg)
			ehttptest.True(t, strictBasis+" (why and fix)", strings.Contains(msg, ": ") && strings.Contains(msg, ". Fix: ") && strings.HasSuffix(msg, "."), "message %q", msg)
		})
	}
}

func TestBootPanicsInProduction(t *testing.T) {
	t.Parallel()

	msg := aPanics(func() { Cors(CorsOptions{Origin: []string{""}}) })

	ehttptest.True(t, strictBasis, strings.Contains(msg, "in Cors"), "message %q", msg)
}

func TestAcceptsAddsVaryAccept(t *testing.T) {
	t.Parallel()

	rec, _, _ := aServe(Production, func(res *ResponseWriter, req *Request) {
		res.Vary("Origin")
		req.Accepts("text/html")
		req.Accepts("application/json")
		res.String("ok")
	})

	ehttptest.Eq(t, "RFC 9110 §12.5.5", strings.Join(rec.Header().Values("Vary"), "|"), "Origin, Accept")

	rec, _, _ = aServe(Production, func(res *ResponseWriter, req *Request) {
		req.Accepts("text/html") // Accept is absent: the answer still depends on it
		res.String("ok")
	})

	ehttptest.Eq(t, "RFC 9110 §12.5.5", rec.Header().Get("Vary"), "Accept")
}

func TestAcceptsAfterSendIsQuiet(t *testing.T) {
	t.Parallel()

	_, logs, msg := aServe(Development, func(res *ResponseWriter, req *Request) {
		res.String("ok")
		req.Accepts("text/html")
	})

	ehttptest.Eq(t, "Accepts reads the request; it is not a mistake", msg, "")
	ehttptest.Eq(t, "no log", logs.Len(), 0)
}

func TestAcceptsOnHandBuiltRequest(t *testing.T) {
	t.Parallel()

	ehttptest.True(t, "no panic without a response writer", aReq("GET", "/", nil, "Accept", "text/html").Accepts("text/html"), "Accepts is false")
}

func TestRedirectInvalidLocation(t *testing.T) {
	t.Parallel()

	do := func(res *ResponseWriter, _ *Request) { res.Redirect("/a\r\nSet-Cookie: x=1") }

	_, _, msg := aServe(Development, do)
	aPart(t, msg, "res.Redirect(...)", "has the byte '\\r' (0x0d) at position 2")

	rec, logs, msg := aServe(Production, do)
	ehttptest.Eq(t, strictBasis+" (no panic)", msg, "")
	ehttptest.Eq(t, strictBasis+" (500, not a redirect without Location)", rec.Code, 500)
	ehttptest.Eq(t, strictBasis, rec.Result().Header.Get("Location"), "")
	ehttptest.Eq(t, strictBasis+" (one error)", logs.FilterField(zap.String("rule", "invalid header value")).Len(), 1)
	ehttptest.Eq(t, strictBasis, logs.All()[0].Level, zapcore.ErrorLevel)
}

func TestDuplicateRoutePanics(t *testing.T) {
	t.Parallel()

	noop := func(*ResponseWriter, *Request) {}

	tests := map[string]func(s *Server){
		"same method and path": func(s *Server) { s.GET("/a", noop); s.GET("/a", noop) },
		"lower case method":    func(s *Server) { s.GET("/a", noop); s.router.Handle("get", "/a", noop) },
		"trailing slash":       func(s *Server) { s.GET("/a", noop); s.GET("/a/", noop) },
		"parameter name":       func(s *Server) { s.GET("/u/:id", noop); s.GET("/u/:name", noop) },
		"same regex":           func(s *Server) { s.GET("/u/:id|[0-9]+", noop); s.GET("/u/:n|[0-9]+", noop) },
		"in a group":           func(s *Server) { s.GET("/g/a", noop); s.Route("/g").GET("/a", noop) },
		"in a group twice":     func(s *Server) { g := s.Route("/g"); g.GET("/a", noop); g.GET("/a", noop) },
		"nested group":         func(s *Server) { s.Route("/g").Route("/h").GET("/a", noop); s.GET("/g/h/a", noop) },
		"explicit HEAD":        func(s *Server) { s.router.Handle("HEAD", "/a", noop); s.router.Handle("head", "/a", noop) },
	}

	for name, do := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			msg := aPanics(func() { do(NewServer()) })

			ehttptest.True(t, "the first route wins, so a second one never runs", strings.HasPrefix(msg, "ehttp: a second handler in "), "message %q", msg)
			ehttptest.True(t, "DX message: fix part", strings.Contains(msg, ". Fix: "), "message %q has no Fix part", msg)
		})
	}
}

func TestDifferentRoutesDoNotPanic(t *testing.T) {
	t.Parallel()

	noop := func(*ResponseWriter, *Request) {}

	s := NewServer()
	s.GET("/a", noop)
	s.POST("/a", noop)
	s.router.Handle("HEAD", "/a", noop)
	s.GET("/u/:id|[0-9]+", noop)
	s.GET("/u/:id|[a-z]+", noop)
	s.GET("/u/:id", noop)
	s.GET("/u/:id/x", noop)
	s.GET("/s/...", noop)
	s.Route("/g").GET("/a", noop)
}

// A port that is not a decimal number from 0 to 65535 is a mistake of the
// developer and panics at boot. Port 0 is valid: the OS picks a free port.
func TestWithPortInvalidPanics(t *testing.T) {
	t.Parallel()

	for _, port := range []string{"", "65536", "99999999999", "-1", "8080:9", "http", " 80", "+80"} {
		t.Run("port "+port, func(t *testing.T) {
			t.Parallel()

			msg := aPanics(func() { WithPort(port) })

			ehttptest.True(t, "a TCP port is 0 to 65535", strings.HasPrefix(msg, "ehttp: port "+strconv.Quote(port)+" in WithPort: "), "message %q", msg)
			ehttptest.True(t, "DX message: fix part", strings.Contains(msg, ". Fix: "), "message %q has no Fix part", msg)
		})
	}

	for _, n := range []int64{-1, 65536} {
		msg := aPanics(func() { WithPortInt(n) })

		ehttptest.True(t, "a TCP port is 0 to 65535", strings.Contains(msg, " in WithPortInt: "), "message %q", msg)
	}
}

func TestWithPortAccepts(t *testing.T) {
	t.Parallel()

	ehttptest.Eq(t, "port 0 lets the OS pick a port", NewServer(WithPort("0")).port, "0")
	ehttptest.Eq(t, "port 65535 is the last port", NewServer(WithPort("65535")).port, "65535")
	ehttptest.Eq(t, "port 0 lets the OS pick a port", NewServer(WithPortInt(0)).port, "0")
}

// RFC 9111 §1.2.2: max-age takes whole seconds. A duration above 0 and under 1s
// follows the rule of a negative one: Development panics, Production logs and
// sends no-store. From 1s up, the seconds are cut to a whole number.
func TestSetCacheHeaderSubSecondDuration(t *testing.T) {
	t.Parallel()

	const noStoreLong = "no-store, no-cache, must-revalidate, proxy-revalidate"

	tests := []struct {
		d    time.Duration
		fail bool
		want string // Cache-Control, public
	}{
		{time.Nanosecond, true, noStoreLong},
		{500 * time.Millisecond, true, noStoreLong},
		{999 * time.Millisecond, true, noStoreLong},
		{time.Second, false, "max-age=1, stale-while-revalidate=1, stale-if-error=1"},
		{1500 * time.Millisecond, false, "max-age=1, stale-while-revalidate=1, stale-if-error=1"},
	}

	for _, tt := range tests {
		for _, private := range []bool{false, true} {
			t.Run(tt.d.String()+map[bool]string{false: "", true: " private"}[private], func(t *testing.T) {
				t.Parallel()

				do := func(res *ResponseWriter, _ *Request) { SetCacheHeaderPrivate(res, tt.d, private); res.String("ok") }

				_, _, msg := aServe(Development, do)
				rec, logs, prodMsg := aServe(Production, do)
				rule := logs.FilterField(zap.String("rule", "sub-second cache duration"))
				want := tt.want

				if private && !tt.fail {
					want = "private, " + want
				}

				ehttptest.Eq(t, strictBasis+" (Production: no panic)", prodMsg, "")
				ehttptest.Eq(t, strictBasis, rec.Code, 200)
				ehttptest.Eq(t, strictBasis+" (header)", rec.Header().Get("Cache-Control"), want)

				if !tt.fail {
					ehttptest.Eq(t, strictBasis+" (no panic in Development)", msg, "")
					ehttptest.Eq(t, strictBasis+" (no log)", logs.Len(), 0)

					return
				}

				aPart(t, msg, "ehttp.SetCacheHeader("+tt.d.String()+")", "whole seconds")
				ehttptest.True(t, "the message names the route", strings.Contains(msg, "GET /x"), "message %q", msg)
				ehttptest.True(t, "the message names the fix", strings.Contains(msg, "Fix: use 1s or more, or 0"), "message %q", msg)
				ehttptest.Eq(t, strictBasis+" (one log)", rule.Len(), 1)
				ehttptest.Eq(t, strictBasis+" (error level)", rule.All()[0].Level, zap.ErrorLevel)
			})
		}
	}
}

// RFC 9111 §1.2.2: max-age is a non-negative integer. A negative duration in a
// handler is a mistake: Development panics, Production logs and sends no-store.
func TestSetCacheHeaderNegativeDuration(t *testing.T) {
	t.Parallel()

	do := func(res *ResponseWriter, _ *Request) { SetCacheHeader(res, -time.Minute); res.String("ok") }

	t.Run("development panics", func(t *testing.T) {
		t.Parallel()

		_, _, msg := aServe(Development, do)

		aPart(t, msg, "ehttp.SetCacheHeader(-1m0s)", "non-negative integer (RFC 9111 §1.2.2)")
		ehttptest.True(t, "the message names the fallback", strings.Contains(msg, "no-store"), "message %q", msg)
	})

	t.Run("production logs and sends no-store", func(t *testing.T) {
		t.Parallel()

		rec, logs, msg := aServe(Production, do)

		ehttptest.Eq(t, strictBasis, msg, "")
		ehttptest.Eq(t, strictBasis, rec.Code, 200)
		ehttptest.Eq(t, "safe fallback is no-store", strings.HasPrefix(rec.Header().Get("Cache-Control"), "no-store"), true)
		ehttptest.Eq(t, strictBasis, logs.FilterField(zap.String("rule", "negative cache duration")).Len(), 1)
	})

	t.Run("the pure function gives no-store", func(t *testing.T) {
		t.Parallel()

		ehttptest.Eq(t, "RFC 9111 §1.2.2", strings.HasPrefix(CacheControl(-time.Second, false), "no-store"), true)
	})
}
