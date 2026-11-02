package middleware

import (
	"context"
	"errors"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/hook"
)

// Timeout sets a deadline on the request context. It is cooperative: the
// handler runs in the same goroutine and must respect the context. If the
// deadline passed and no header was sent, it sends 503. It sets no
// Cache-Control: a 503 is not heuristically cacheable (RFC 9110 §15.6.4), so
// without ErrorCache a cache does not store it. With ErrorCache, its lifetime
// for the status decides.
func Timeout(timeout time.Duration) ehttp.MiddlewareHandlerFunc {
	if timeout <= 0 {
		hook.BootPanic("Timeout", "duration "+timeout.String(), "a deadline of zero or less is already past, so each request would get 503 (context.WithTimeout)",
			"pass a positive duration, for example Timeout(30 * time.Second), or remove the middleware for no limit")
	}

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
				res.SendStatus(ehttp.StatusServiceUnavailable)
			}
		}
	}
}
