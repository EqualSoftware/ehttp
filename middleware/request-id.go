package middleware

import (
	"github.com/EqualSoftware/ehttp"
	"github.com/google/uuid"
)

func RequestId() ehttp.MiddlewareHandlerFunc {
	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			rid := req.Header(ehttp.HeaderXRequestID)

			if rid == "" {
				rid = uuid.New().String()
			}

			res.Append(ehttp.HeaderXRequestID, rid)

			next.ServeHTTP(res, req)
		}
	}
}
