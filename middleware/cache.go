package middleware

import (
	"time"

	"github.com/EqualSoftware/ehttp"
)

func Cache(duration time.Duration) ehttp.MiddlewareHandlerFunc {
	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			ehttp.SetCacheHeader(res, duration)
			next.ServeHTTP(res, req)
		}
	}
}
