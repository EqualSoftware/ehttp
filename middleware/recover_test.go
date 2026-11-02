package middleware_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"github.com/EqualSoftware/ehttp/internal/fixtures"
	"github.com/EqualSoftware/ehttp/middleware"
)

func bPanic(v any) ehttp.HandlerFunc {
	return func(*ehttp.ResponseWriter, *ehttp.Request) { panic(v) }
}

func TestRecover(t *testing.T) {
	t.Parallel()

	t.Run("no panic passes through untouched", func(t *testing.T) {
		t.Parallel()

		rec := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.Recover()), fixtures.Req("GET", "/x"))

		ehttptest.Eq(t, "middleware contract", rec.Code, 200)
		ehttptest.Eq(t, "middleware contract", rec.Body.String(), "ok")
	})

	for name, v := range map[string]any{
		"string panic": "boom",
		"error panic":  errors.New("boom"),
		"int panic":    42,
	} {
		t.Run(name+" becomes a JSON 500", func(t *testing.T) {
			t.Parallel()

			rec := ehttptest.Do(fixtures.Handler(bPanic(v), middleware.Recover()), fixtures.Req("GET", "/x"))

			var body struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}
			err := json.Unmarshal(rec.Body.Bytes(), &body)

			ehttptest.Eq(t, "RFC 9110 §15.6.1", rec.Code, 500)
			ehttptest.Eq(t, "middleware contract", rec.Header().Get("Content-Type"), ehttp.MIMEApplicationJSONCharsetUTF8) //nolint:staticcheck // the test asserts this exact (deprecated) value the framework emits
			ehttptest.True(t, "middleware contract", err == nil, "body is not JSON: %q (%v)", rec.Body.String(), err)
			ehttptest.Eq(t, "middleware contract", body.Code, 500)
			ehttptest.Eq(t, "middleware contract", body.Message, "Internal Server Error")
		})
	}

	t.Run("headers set by outer middlewares survive the 500", func(t *testing.T) {
		t.Parallel()

		h := fixtures.Handler(bPanic("boom"), middleware.RequestId(), middleware.Recover())
		rec := ehttptest.Do(h, fixtures.Req("GET", "/x", "X-Request-Id", "abc"))

		ehttptest.Eq(t, "composition", rec.Code, 500)
		ehttptest.Eq(t, "composition", rec.Header().Get("X-Request-Id"), "abc")
	})

	t.Run("a panic in a later middleware is also recovered", func(t *testing.T) {
		t.Parallel()

		bad := func(next ehttp.Handler) ehttp.HandlerFunc { return bPanic("mw") }
		rec := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.Recover(), bad), fixtures.Req("GET", "/x"))

		ehttptest.Eq(t, "middleware contract", rec.Code, 500)
	})
}

// S6: the panic value is internal and must not reach the client.
func TestRecoverDoesNotLeakPanicValue(t *testing.T) {
	t.Parallel()

	rec := ehttptest.Do(fixtures.Handler(bPanic("db password=hunter2"), middleware.Recover()), fixtures.Req("GET", "/x"))

	ehttptest.True(t, "S6", !strings.Contains(rec.Body.String(), "hunter2"), "body leaks the panic value: %q", rec.Body.String())
}

// S6: http.ErrAbortHandler is net/http's way to abort a response; swallowing
// it hides the abort from the server.
func TestRecoverRethrowsAbortHandler(t *testing.T) {
	t.Parallel()

	var got any

	func() {
		defer func() { got = recover() }()
		ehttptest.Do(fixtures.Handler(bPanic(http.ErrAbortHandler), middleware.Recover()), fixtures.Req("GET", "/x"))
	}()

	ehttptest.True(t, "net/http ErrAbortHandler doc", got == http.ErrAbortHandler, "recovered value = %v, want http.ErrAbortHandler re-panicked", got) //nolint:errorlint // got is a recover() value (any); net/http compares it by identity
}

// The "response already started" case (a 500 JSON glued to a chunked body,
// RFC 9112 §8: the client must see an incomplete message) needs a real
// server. It lives in test/security (TestCSecurityS6PanicAfterHeadersSent).

// A recovered panic goes to the logger of the server, with the request fields.
func TestRecoverLogsThroughServerLogger(t *testing.T) {
	t.Parallel()

	logger, logs := fixtures.ObservedLogger()
	h := fixtures.HandlerLogged(logger, bPanic("boom"), middleware.RequestId(), middleware.Recover())

	ehttptest.Do(h, fixtures.Req("GET", "/x", "X-Request-Id", "rid-9"))

	got := logs.FilterMessage("panic recovered").All()
	ehttptest.Eq(t, "one log line", len(got), 1)
	ehttptest.Eq(t, "request_id comes from RequestId", got[0].ContextMap()["request_id"], any("rid-9"))
	ehttptest.Eq(t, "route", got[0].ContextMap()["route"], any("/x"))
}

func TestRequestIdFeedsTheRequestLogger(t *testing.T) {
	t.Parallel()

	logger, logs := fixtures.ObservedLogger()
	h := fixtures.HandlerLogged(logger, func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		req.Logger().Info("hit")
	}, middleware.RequestId())

	rec := ehttptest.Do(h, fixtures.Req("GET", "/x"))

	id := rec.Header().Get("X-Request-Id")
	ehttptest.Eq(t, "generated id is in the log", logs.All()[0].ContextMap()["request_id"], any(id))
}
