package middleware

import (
	"context"
	"errors"
	"time"

	"github.com/EqualSoftware/ehttp"
)

// Timeout sets a deadline on the request context. It is cooperative: the
// handler runs in the same goroutine and must respect the context. If the
// deadline passed and no header was sent, it sends 503.
func Timeout(timeout time.Duration) ehttp.MiddlewareHandlerFunc {
	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			orig := req.Request
			ctx, cancel := context.WithTimeout(req.Context(), timeout)

			// WithContext changes the request in place: restore it for the outer middleware.
			defer func() {
				cancel()

				req.Request = orig
			}()

			next.ServeHTTP(res, req.WithContext(ctx))

			if errors.Is(ctx.Err(), context.DeadlineExceeded) && !res.HeadersSent() {
				// A 503 is temporary (RFC 9110 §15.6.4): it replaces any cache header set before.
				res.Set(ehttp.HeaderCacheControl, "no-store")
				res.SendStatus(ehttp.StatusServiceUnavailable)
			}
		}
	}
}
