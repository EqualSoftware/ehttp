package middleware_test

import (
	"strings"
	"testing"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"github.com/EqualSoftware/ehttp/internal/fixtures"
	"github.com/EqualSoftware/ehttp/middleware"
)

func TestAcceptEncoding(t *testing.T) {
	t.Parallel()

	const rfc = "RFC 9110 §12.5.5"

	tests := []struct {
		name     string
		existing string
		want     string
	}{
		{"adds Vary when absent", "", "Accept-Encoding"},
		{"appends to existing Vary", "Origin", "Origin, Accept-Encoding"},
		{"does not duplicate", "Accept-Encoding", "Accept-Encoding"},
		{"does not duplicate case-insensitively", "accept-encoding", "accept-encoding"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// the middleware runs before the handler, so seed Vary through an outer one
			outer := func(next ehttp.Handler) ehttp.HandlerFunc {
				return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
					if tt.existing != "" {
						res.Vary(tt.existing)
					}

					next.ServeHTTP(res, req)
				}
			}

			h := fixtures.Handler(fixtures.OK, outer, middleware.AcceptEncoding())
			rec := ehttptest.Do(h, fixtures.Req("GET", "/x"))

			ehttptest.Eq(t, rfc, rec.Code, 200)
			ehttptest.Eq(t, rfc, rec.Header().Get("Vary"), tt.want)
		})
	}
}

func TestAcceptEncodingCallsNext(t *testing.T) {
	t.Parallel()

	called := false
	h := fixtures.Handler(func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		called = true
		res.String("body")
	}, middleware.AcceptEncoding())

	rec := ehttptest.Do(h, fixtures.Req("GET", "/x"))

	ehttptest.True(t, "middleware contract", called, "next handler was not called")
	ehttptest.Eq(t, "middleware contract", rec.Body.String(), "body")
	ehttptest.True(t, "RFC 9110 §12.5.5", strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding"), "Vary lacks Accept-Encoding: %q", rec.Header().Get("Vary"))
}

// Vary is a list of field names (RFC 9110 §12.5.5). A name that only
// contains "Accept-Encoding" as a substring must not count as present.
func TestAcceptEncodingVarySubstring(t *testing.T) {
	t.Parallel()

	outer := func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			res.Vary("X-Accept-Encoding-Hint")
			next.ServeHTTP(res, req)
		}
	}

	rec := ehttptest.Do(fixtures.Handler(fixtures.OK, outer, middleware.AcceptEncoding()), fixtures.Req("GET", "/x"))
	vary := "," + strings.ReplaceAll(strings.ToLower(rec.Header().Get("Vary")), " ", "") + ","

	ehttptest.True(t, "RFC 9110 §12.5.5", strings.Contains(vary, ",accept-encoding,"),
		"Vary %q has no exact Accept-Encoding member", rec.Header().Get("Vary"))
}
