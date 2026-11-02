package middleware_test

import (
	"testing"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"github.com/EqualSoftware/ehttp/internal/fixtures"
	"github.com/EqualSoftware/ehttp/middleware"
	"go.uber.org/zap/zaptest/observer"
)

func bAccessLogs(logs *observer.ObservedLogs) []map[string]any {
	var out []map[string]any

	for _, e := range logs.FilterMessage("server sent response").All() {
		out = append(out, e.ContextMap())
	}

	return out
}

func TestLog(t *testing.T) {
	t.Run("logs method, route pattern and status", func(t *testing.T) {
		logger, logs := fixtures.ObservedLogger()
		s := ehttp.NewServer(ehttp.WithLogger(logger))
		s.Use(middleware.Log(middleware.LogOptions{}))
		s.GET("/users/:id", fixtures.OK)

		rec := ehttptest.Do(s, fixtures.Req("GET", "/users/42?token=abc"))
		got := bAccessLogs(logs)

		ehttptest.Eq(t, "middleware contract", rec.Code, 200)
		ehttptest.Eq(t, "one line per request", len(got), 1)
		ehttptest.Eq(t, "route pattern, not raw path (low log cardinality)", got[0]["url"], any("/users/:id"))
		ehttptest.Eq(t, "middleware contract", got[0]["method"], any("GET"))
		ehttptest.Eq(t, "middleware contract", got[0]["code"], any(int64(200)))
		_, hasDuration := got[0]["duration"]
		ehttptest.True(t, "middleware contract", hasDuration, "no duration field")
	})

	t.Run("unmatched route logs the raw path and 404", func(t *testing.T) {
		logger, logs := fixtures.ObservedLogger()
		s := ehttp.NewServer(ehttp.WithLogger(logger))
		s.Use(middleware.Log(middleware.LogOptions{}))
		s.GET("/a", fixtures.OK)

		ehttptest.Do(s, fixtures.Req("GET", "/nope"))
		got := bAccessLogs(logs)

		ehttptest.Eq(t, "one line per request", len(got), 1)
		ehttptest.Eq(t, "middleware contract", got[0]["url"], any("/nope"))
		ehttptest.Eq(t, "middleware contract", got[0]["code"], any(int64(404)))
	})

	t.Run("IgnoreStatus and IgnorePaths suppress the line", func(t *testing.T) {
		logger, logs := fixtures.ObservedLogger()
		s := ehttp.NewServer(ehttp.WithLogger(logger))
		s.Use(middleware.Log(middleware.LogOptions{IgnoreStatus: []int{404}, IgnorePaths: []string{"/health"}}))
		s.GET("/health", fixtures.OK)
		s.GET("/real", fixtures.OK)

		ehttptest.Do(s, fixtures.Req("GET", "/health"))
		ehttptest.Do(s, fixtures.Req("GET", "/nope"))
		ehttptest.Do(s, fixtures.Req("GET", "/real"))
		got := bAccessLogs(logs)

		ehttptest.Eq(t, "LogOptions contract", len(got), 1)
		ehttptest.Eq(t, "LogOptions contract", got[0]["url"], any("/real"))
	})

	t.Run("calls next and keeps the response", func(t *testing.T) {
		rec := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.Log(middleware.LogOptions{})), fixtures.Req("GET", "/x"))

		ehttptest.Eq(t, "middleware contract", rec.Code, 200)
		ehttptest.Eq(t, "middleware contract", rec.Body.String(), "ok")
	})

	t.Run("outside Recover it records the recovered 500", func(t *testing.T) {
		logger, logs := fixtures.ObservedLogger()
		h := fixtures.HandlerLogged(logger, func(*ehttp.ResponseWriter, *ehttp.Request) { panic("boom") },
			middleware.Log(middleware.LogOptions{}), middleware.Recover())

		rec := ehttptest.Do(h, fixtures.Req("GET", "/x"))
		got := bAccessLogs(logs)

		ehttptest.Eq(t, "composition", rec.Code, 500)
		ehttptest.Eq(t, "composition", len(got), 1)
		ehttptest.Eq(t, "composition", got[0]["code"], any(int64(500)))
	})

	t.Run("query string stays out of the log", func(t *testing.T) {
		logger, logs := fixtures.ObservedLogger()
		s := ehttp.NewServer(ehttp.WithLogger(logger))
		s.Use(middleware.Log(middleware.LogOptions{}))
		s.GET("/nope-not-registered", fixtures.OK)

		ehttptest.Do(s, fixtures.Req("GET", "/other?token=secret-123"))
		got := bAccessLogs(logs)

		ehttptest.Eq(t, "one line per request", len(got), 1)
		ehttptest.Eq(t, "middleware contract: path only, no query", got[0]["url"], any("/other"))
	})
}

// A handler that answers through net/http's implicit 200 (nothing written
// via ehttp) is logged with the sent code.
func TestLogImplicitStatus(t *testing.T) {
	logger, logs := fixtures.ObservedLogger()

	rec := ehttptest.Do(fixtures.HandlerLogged(logger, func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		_, _ = res.Write([]byte("direct"))
	}, middleware.Log(middleware.LogOptions{})), fixtures.Req("GET", "/x"))
	got := bAccessLogs(logs)

	ehttptest.Eq(t, "net/http implicit 200", rec.Code, 200)
	ehttptest.Eq(t, "one line per request", len(got), 1)

	ehttptest.Eq(t, "logged status must match the sent status", got[0]["code"], any(int64(200)))
}
