package middleware

import (
	"fmt"
	"net/http"
	"runtime/debug"

	"github.com/EqualSoftware/ehttp"
)

func Recover() ehttp.MiddlewareHandlerFunc {
	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			defer func() {
				if r := recover(); r != nil {
					fmt.Printf("panic: %v\n%s\n", r, debug.Stack())

					res.Status(ehttp.StatusInternalServerError)
					res.JSON(map[string]interface{}{
						"code":     http.StatusInternalServerError,
						"message":  http.StatusText(ehttp.StatusInternalServerError),
						"internal": fmt.Sprintf("%v", r),
					})
				}
			}()

			next.ServeHTTP(res, req)
		}
	}
}
