package middleware

import (
	"time"

	"github.com/EqualSoftware/ehttp"
)

func Cache(duration time.Duration) ehttp.MiddlewareHandlerFunc {
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
	ehttp.SetCacheHeaderPrivate(res, d, req.Header(ehttp.HeaderAuthorization) != "")
}
