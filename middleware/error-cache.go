package middleware

import (
	"time"

	"github.com/EqualSoftware/ehttp"
)

func ErrorCache(duration time.Duration) ehttp.MiddlewareHandlerFunc {
	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			ires, i := ehttp.WithResponseWriter(res)
			next.ServeHTTP(ires, req)

			if i.Code >= 400 {
				ehttp.SetCacheHeader(res, duration)
			}

			i.SendResponse(res)
		}
	}
}
