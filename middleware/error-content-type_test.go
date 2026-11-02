package middleware_test

import (
	"testing"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"github.com/EqualSoftware/ehttp/internal/fixtures"
	"github.com/EqualSoftware/ehttp/middleware"
)

const ctBasis = "an error the library writes sets its own Content-Type"

func TestRecoverSetsOwnContentType(t *testing.T) {
	t.Parallel()

	h := fixtures.Handler(func(res *ehttp.ResponseWriter, _ *ehttp.Request) {
		res.Set(ehttp.HeaderContentType, "text/html")
		panic("boom")
	}, middleware.Recover())

	rec := ehttptest.Do(h, fixtures.Req("GET", "/x"))

	ehttptest.Eq(t, ctBasis, rec.Code, 500)
	ehttptest.Eq(t, ctBasis, rec.Header().Get(ehttp.HeaderContentType), "application/json; charset=UTF-8")
}

// The other library errors send a status and no body, so a Content-Type of the
// handler describes nothing: check that no body is written.
func TestLibraryErrorsHaveNoBody(t *testing.T) {
	t.Parallel()

	set := func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.Set(ehttp.HeaderContentType, "text/html") }

	tests := []struct {
		name string
		h    func() *ehttp.Server
		req  []string
		code int
	}{
		{"BodyLimit 413", func() *ehttp.Server {
			s := ehttp.NewServer()
			s.POST("/x", set, middleware.BodyLimit("1B"))
			return s
		}, []string{"POST", "/x"}, 413},
		{"router 404", func() *ehttp.Server { return ehttp.NewServer() }, []string{"GET", "/none"}, 404},
		{"router 405", func() *ehttp.Server {
			s := ehttp.NewServer()
			s.POST("/x", set)
			return s
		}, []string{"GET", "/x"}, 405},
		{"CORS 403", func() *ehttp.Server {
			s := ehttp.NewServer()
			s.Use(ehttp.Cors(ehttp.CorsOptions{Origin: []string{"http://ok"}}))
			s.POST("/x", set)
			return s
		}, []string{"OPTIONS", "/x"}, 403},
		{"Timeout 503", func() *ehttp.Server {
			s := ehttp.NewServer()
			s.GET("/x", set, middleware.Timeout(time.Nanosecond))
			return s
		}, []string{"GET", "/x"}, 503},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := fixtures.Req(tt.req[0], tt.req[1], "Origin", "http://bad", "Access-Control-Request-Method", "POST")
			if tt.req[0] == "POST" {
				req.ContentLength = 100
			}

			rec := ehttptest.Do(tt.h(), req)

			ehttptest.Eq(t, ctBasis, rec.Code, tt.code)
			ehttptest.Eq(t, ctBasis, rec.Body.Len(), 0)
		})
	}
}
