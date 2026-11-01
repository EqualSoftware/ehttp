package middleware

import (
	"context"
	"time"

	"github.com/EqualSoftware/ehttp"
)

func Timeout(timeout time.Duration) ehttp.MiddlewareHandlerFunc {
	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			ctx, cancel := context.WithTimeout(req.Context(), timeout)
			defer cancel()

			done := make(chan struct{})

			go func() {
				defer close(done)
				next.ServeHTTP(res, req.WithContext(ctx))
			}()

			select {
			case <-ctx.Done():
				res.SendStatus(ehttp.StatusRequestTimeout)
			case <-done:
				return
			}
		}
	}
}
