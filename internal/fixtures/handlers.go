// Package fixtures holds the servers, handlers and requests that the test
// suites share. It imports ehttp, so it lives apart from ehttptest, which
// the tests inside package ehttp import.
package fixtures

import (
	"context"
	"net/http"
	"net/http/httptest"

	"github.com/EqualSoftware/ehttp"
	"go.uber.org/zap"
)

// Handler builds a server that serves h on GET /x (and HEAD) with the
// given route middlewares, in order (first = outermost). It registers one
// method only: GET plus POST on the same path answers 405 for POST.
func Handler(h ehttp.HandlerFunc, mws ...ehttp.MiddlewareHandlerFunc) http.Handler {
	s := ehttp.NewServer()
	s.GET("/x", h, mws...)

	return s
}

// HandlerLogged is Handler with the server logger set.
func HandlerLogged(l *zap.Logger, h ehttp.HandlerFunc, mws ...ehttp.MiddlewareHandlerFunc) http.Handler {
	s := ehttp.NewServer(ehttp.WithLogger(l))
	s.GET("/x", h, mws...)

	return s
}

// PostHandler is Handler for POST /x.
func PostHandler(h ehttp.HandlerFunc, mws ...ehttp.MiddlewareHandlerFunc) http.Handler {
	s := ehttp.NewServer()
	s.POST("/x", h, mws...)

	return s
}

// Req builds an in-memory request with headers given as key, value pairs.
func Req(method, target string, kv ...string) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), method, target, http.NoBody)

	for i := 0; i+1 < len(kv); i += 2 {
		req.Header.Set(kv[i], kv[i+1])
	}

	return req
}

// OK answers 200 with the body "ok".
func OK(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.String("ok") }

// Status answers with code and body.
func Status(code int, body string) ehttp.HandlerFunc {
	return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		res.Status(code)
		res.String(body)
	}
}
