package middleware

import (
	"net/http"
	"runtime/debug"

	"github.com/EqualSoftware/ehttp"
	"go.uber.org/zap"
)

// Recover turns a panic into a 500 response. The panic value goes to the log,
// never to the client. If a header was already sent, the response cannot be
// fixed: it aborts the connection with http.ErrAbortHandler.
func Recover() ehttp.MiddlewareHandlerFunc {
	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			defer func() {
				r := recover()
				if r == nil {
					return
				}

				if r == http.ErrAbortHandler { //nolint:errorlint // recover() value; net/http compares it by identity
					panic(r)
				}

				req.Logger().Error("panic recovered",
					zap.Any("panic", r),
					zap.ByteString("stack", debug.Stack()),
				)

				if res.HeadersSent() {
					res.Flush() // what was already written reaches the client; a sized body stays complete

					panic(http.ErrAbortHandler)
				}

				res.Status(ehttp.StatusInternalServerError)
				res.JSON(map[string]interface{}{
					"code":    http.StatusInternalServerError,
					"message": http.StatusText(ehttp.StatusInternalServerError),
				})
			}()

			next.ServeHTTP(res, req)
		}
	}
}
