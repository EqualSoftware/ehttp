package ehttp

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func aObserved() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)

	return zap.New(core), logs
}

func TestServerLoggerPrecedence(t *testing.T) {
	t.Parallel()

	l, _ := aObserved()

	ehttptest.True(t, "default mode is Production", NewServer().router.mode == Production, "mode changed")
	ehttptest.True(t, "default logger is built", NewServer().router.logger != nil, "nil logger")
	ehttptest.True(t, "Development builds the debug preset", NewServer(WithMode(Development)).router.logger.Core().Enabled(zap.DebugLevel), "debug is off")
	ehttptest.True(t, "Production builds the info preset", !NewServer(WithMode(Production)).router.logger.Core().Enabled(zap.DebugLevel), "debug is on")

	for name, s := range map[string]*Server{
		"logger then mode": NewServer(WithLogger(l), WithMode(Development)),
		"mode then logger": NewServer(WithMode(Development), WithLogger(l)),
	} {
		ehttptest.True(t, name+": WithLogger wins", s.router.logger == l, "preset replaced the logger")
		ehttptest.True(t, name+": the mode is kept", s.router.mode == Development, "mode lost")
	}
}

func TestWithLoggerNilPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		ehttptest.True(t, "nil logger panics at boot", recover() != nil, "no panic")
	}()

	WithLogger(nil)
}

func TestServersKeepSeparateLoggers(t *testing.T) {
	t.Parallel()

	la, logsA := aObserved()
	lb, logsB := aObserved()
	before := zap.L()

	handler := func(res *ResponseWriter, req *Request) { req.Logger().Info("hit") }
	a := NewServer(WithLogger(la))
	b := NewServer(WithLogger(lb))
	a.GET("/a", handler)
	b.GET("/b", handler)

	a.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), "GET", "/a", nil))

	ehttptest.Eq(t, "server a logs to its logger", logsA.Len(), 1)
	ehttptest.Eq(t, "server b logs nothing", logsB.Len(), 0)
	ehttptest.True(t, "the global logger is untouched", zap.L() == before, "global replaced")
}

func TestRequestLoggerFields(t *testing.T) {
	t.Parallel()

	l, logs := aObserved()
	s := NewServer(WithLogger(l))
	s.GET("/users/:id", func(res *ResponseWriter, req *Request) {
		req.Logger().Info("hit")
		first, second := req.Logger(), req.Logger()
		ehttptest.True(t, "the logger is cached", first == second, "built twice")
	})

	rec := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(t.Context(), "GET", "/users/42", nil)
	r.Header.Set(HeaderXRequestID, "rid-1")
	s.ServeHTTP(rec, r)

	got := logs.FilterMessage("hit").All()[0].ContextMap()
	ehttptest.Eq(t, "request_id", got["request_id"], any("rid-1"))
	ehttptest.Eq(t, "method", got["method"], any("GET"))
	ehttptest.Eq(t, "route is the pattern, not the path", got["route"], any("/users/:id"))
}

func TestRequestLoggerOmitsEmptyFields(t *testing.T) {
	t.Parallel()

	l, logs := aObserved()
	s := NewServer(WithLogger(l))
	s.Use(func(next Handler) HandlerFunc {
		return func(res *ResponseWriter, req *Request) {
			req.Logger().Info("mw")
			next.ServeHTTP(res, req)
		}
	})

	s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), "GET", "/nope", nil))

	got := logs.FilterMessage("mw").All()[0].ContextMap()
	_, hasRoute := got["route"]
	_, hasID := got["request_id"]
	ehttptest.True(t, "404 has no route field", !hasRoute, "route = %v", got["route"])
	ehttptest.True(t, "no request id, no field", !hasID, "request_id = %v", got["request_id"])
	ehttptest.Eq(t, "method stays", got["method"], any("GET"))
}

func TestRequestLoggerHandBuiltRequest(t *testing.T) {
	t.Parallel()

	req := &Request{Request: httptest.NewRequestWithContext(t.Context(), "GET", "/", nil)}

	req.Logger().Info("no panic without a server logger")
}

func TestResponseWriterLogsThroughServerLogger(t *testing.T) {
	t.Parallel()

	l, logs := aObserved()
	s := NewServer(WithLogger(l))
	s.GET("/f", func(res *ResponseWriter, req *Request) { res.SendFile("../etc/passwd") })

	s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), "GET", "/f", nil))

	ehttptest.Eq(t, "path traversal log", logs.FilterMessage("path traversal refused").Len(), 1)
}

func TestRequestLoggerIsLazy(t *testing.T) {
	l, _ := aObserved()
	s := NewServer(WithLogger(l))
	s.GET("/x", func(res *ResponseWriter, req *Request) {})

	r := httptest.NewRequestWithContext(t.Context(), "GET", "/x", nil)
	w := httptest.NewRecorder()
	base := testing.AllocsPerRun(100, func() { s.ServeHTTP(w, r) })

	s2 := NewServer(WithLogger(l))
	s2.GET("/x", func(res *ResponseWriter, req *Request) { _ = req.Logger() })

	used := testing.AllocsPerRun(100, func() { s2.ServeHTTP(w, r) })

	ehttptest.True(t, "a route that calls Logger pays for it", used > base, "base %v, used %v", base, used)
}

func TestWithTemplatesBadPatternPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		err, ok := r.(error)
		ehttptest.True(t, "a bad template pattern panics with an error", ok, "recovered %v", r)
		ehttptest.True(t, "the message names the option", ok && strings.Contains(err.Error(), "WithTemplates"), "got %v", r)
	}()

	NewServer(WithTemplates("[bad"))
}

// The observer ContextMap hides duplicate keys, so count the fields.
func TestRequestLoggerHasNoDuplicateKeys(t *testing.T) {
	t.Parallel()

	l, logs := aObserved()
	s := NewServer(WithLogger(l))
	s.Use(Cors(CorsOptions{Origin: []string{"https://a.example"}}))
	s.GET("/x", func(res *ResponseWriter, req *Request) { req.Logger().Info("hit") })

	s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), "GET", "/x", nil))

	r := httptest.NewRequestWithContext(t.Context(), "OPTIONS", "/x", nil)
	r.Header.Set(HeaderOrigin, "https://evil.example")
	r.Header.Set(HeaderAccessControlRequestMethod, "GET")
	s.ServeHTTP(httptest.NewRecorder(), r)

	ehttptest.True(t, "entries were logged", logs.Len() == 2, "got %d", logs.Len())

	for _, e := range logs.All() {
		seen := map[string]bool{}

		for _, f := range e.Context {
			ehttptest.True(t, e.Message+": key once: "+f.Key, !seen[f.Key], "duplicate key %q", f.Key)

			seen[f.Key] = true
		}
	}
}

func TestRequestLoggerSafeForGoroutines(t *testing.T) {
	t.Parallel()

	l, _ := aObserved()
	req := &Request{Request: httptest.NewRequestWithContext(t.Context(), "GET", "/", nil), logger: l}
	got := make([]*zap.Logger, 4)

	var wg sync.WaitGroup

	for i := range got {
		wg.Add(1)

		go func() {
			defer wg.Done()

			got[i] = req.Logger()
		}()
	}

	wg.Wait()

	for _, g := range got {
		ehttptest.True(t, "all goroutines get the cached logger", g == got[0], "different loggers")
	}
}

// A library log call must not freeze the cached logger before RequestId runs.
func TestLibraryLogDoesNotFillTheLoggerCache(t *testing.T) {
	t.Parallel()

	l, logs := aObserved()
	s := NewServer(WithLogger(l), WithTrustedProxies("10.0.0.0/8"))
	s.Use(
		func(next Handler) HandlerFunc {
			return func(res *ResponseWriter, req *Request) {
				_ = req.Protocol() // untrusted peer: the library logs and ignores the header
				next.ServeHTTP(res, req)
			}
		},
		func(next Handler) HandlerFunc { // like RequestId
			return func(res *ResponseWriter, req *Request) {
				req.Request.Header.Set(HeaderXRequestID, "rid-7")
				next.ServeHTTP(res, req)
			}
		},
	)
	s.GET("/x", func(res *ResponseWriter, req *Request) { req.Logger().Info("hit") })

	r := httptest.NewRequestWithContext(t.Context(), "GET", "/x", nil)
	r.Header.Set(HeaderXForwardedProto, "https")
	r.RemoteAddr = "192.0.2.1:1234"
	s.ServeHTTP(httptest.NewRecorder(), r)

	ehttptest.Eq(t, "the library logged the ignored header", logs.FilterMessage("ehttp: ignored forwarded header").Len(), 1)
	ehttptest.Eq(t, "request_id reaches the handler log", logs.FilterMessage("hit").All()[0].ContextMap()["request_id"], any("rid-7"))
}
