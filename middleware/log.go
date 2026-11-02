package middleware

import (
	"slices"
	"time"

	"github.com/EqualSoftware/ehttp"
	"go.uber.org/zap"
)

type LogOptions struct {
	IgnoreStatus []int
	IgnorePaths  []string
}

func Log(option LogOptions) ehttp.MiddlewareHandlerFunc {
	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			start := time.Now()
			next.ServeHTTP(res, req)
			duration := time.Since(start)

			url := req.RoutePath()

			if url == "" {
				url = req.Path()
			}

			if slices.Contains(option.IgnoreStatus, res.StatusCode()) {
				return
			}

			if slices.Contains(option.IgnorePaths, url) {
				return
			}

			req.Logger().Info("server sent response",
				zap.Int("code", res.StatusCode()),
				zap.String("url", url),
				zap.Duration("duration", duration),
			)
		}
	}
}
