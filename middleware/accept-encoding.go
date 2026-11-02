package middleware

import "github.com/EqualSoftware/ehttp"

func AcceptEncoding() ehttp.MiddlewareHandlerFunc {
	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, r *ehttp.Request) {
			res.OwnVary("Accept-Encoding")
			next.ServeHTTP(res, r)
		}
	}
}
