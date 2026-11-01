package middleware

import (
	"time"

	"github.com/EqualSoftware/ehttp"
)

func ErrorCache(duration time.Duration) ehttp.MiddlewareHandlerFunc {
	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			ires := ehttp.WithResponseWriter(res)
			next.ServeHTTP(ires, req)

			i, _ := ires.Writer.(*ehttp.ResponseWriterInterceptor)

			if i.Code >= 400 {
				ehttp.SetCacheHeader(res, duration)
			}

			i.SendResponse(res)
		}
	}
}
