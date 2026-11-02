package middleware

import (
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/hook"
)

func Cache(duration time.Duration) ehttp.MiddlewareHandlerFunc {
	if duration < 0 {
		hook.BootPanic("Cache", "duration "+duration.String(), "max-age is delta-seconds, a non-negative integer (RFC 9111 §1.2.2)",
			"use zero for no-store, or a positive duration, for example Cache(time.Minute)")
	}

	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			setCacheControl(res, req, duration)
			next.ServeHTTP(res, req)
		}
	}
}

// setCacheControl sets the cache headers. A response to a request with
// Authorization is private (RFC 9111 §3.5): a shared cache must not store it.
func setCacheControl(res *ehttp.ResponseWriter, req *ehttp.Request, d time.Duration) {
	res.Own(ehttp.HeaderCacheControl, ehttp.CacheControl(d, req.Header(ehttp.HeaderAuthorization) != ""))
}
