// Command server is a short guide to ehttp. Read it from top to bottom, then
// try the curl commands in README.md.
package main

import (
	"context"
	"crypto/subtle"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/middleware"
)

// Demo credentials. They are for this demo only. A real service reads its
// secrets from the environment or a secret store.
const (
	demoUser     = "demo"
	demoPassword = "demo-password"
)

func main() {
	server := ehttp.NewServer(ehttp.WithPortInt(3000))

	// 2. Middlewares. They run in the order of this list, outside first.
	// Add them before the first route: a middleware added after a route
	// panics.
	server.Use(
		middleware.Recover(),                              // turns a panic into a 500
		middleware.ErrorCache(10*time.Second),             // Cache-Control of error responses; outside Timeout, so it covers the 503
		middleware.RequestId(),                            // X-Request-Id on each response
		middleware.Secure(middleware.DefaultSecureConfig), // security headers
		ehttp.Cors(corsOptions()),                         // before any authentication
		middleware.Timeout(time.Second),                   // deadline of the request context
	)

	// 1. Basic: routes, a route parameter, JSON and status codes.
	server.GET("/hello/:name", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		res.JSON(map[string]string{"message": "Hello, " + req.Param("name")})
	})

	server.POST("/items", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		res.Status(ehttp.StatusCreated)
		res.JSON(map[string]string{"status": "created"})
	})

	server.GET("/teapot", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		res.SendStatus(ehttp.StatusTeapot) // a status line and no body
	})

	server.GET("/boom", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		panic("something went wrong") // Recover answers 500
	})

	// Timeout is cooperative: the handler must watch the request context.
	server.GET("/slow", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		select {
		case <-time.After(3 * time.Second):
			res.String("done")
		case <-req.Context().Done():
		}
	})

	// 3. A group with Basic auth. The group middleware also runs on the 401,
	// 404, 405 and OPTIONS answers under /admin. So a client without
	// credentials does not see the Allow header of these routes.
	admin := server.Route("/admin", basicAuth(demoUser, demoPassword))
	admin.GET("/me", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		res.String("Hello, " + demoUser + "\n")
	})

	// 4. Files and cache. SendFile sets a strong ETag and answers Range and
	// conditional requests. Cache sets Cache-Control for a status below 400.
	server.GET("/notes.txt", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		res.SendFile("examples/server/static/notes.txt") // path from the repository root
	}, middleware.Cache(time.Minute))

	// Listen serves until the context is done, then shuts down gracefully.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := server.Listen(ctx)
	stop()

	if err != nil {
		log.Fatal(err)
	}
}

// corsOptions starts from the open default and narrows it. Use
// ehttp.DefaultCorsOptions() alone for a public API without credentials.
func corsOptions() ehttp.CorsOptions {
	o := ehttp.DefaultCorsOptions()
	o.Origin = []string{"http://localhost:5173"}
	o.Methods = []string{ehttp.MethodGet, ehttp.MethodPost}
	o.CacheDuration = 5 * time.Minute

	return o
}

// basicAuth checks HTTP Basic credentials. A missing or wrong credential gets
// 401 with WWW-Authenticate.
func basicAuth(user, password string) ehttp.MiddlewareHandlerFunc {
	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			u, p, ok := req.Request.BasicAuth()
			if ok && equal(u, user) && equal(p, password) {
				next.ServeHTTP(res, req)
				return
			}

			res.Own(ehttp.HeaderWWWAuthenticate, `Basic realm="demo"`)
			res.SendStatus(ehttp.StatusUnauthorized)
		}
	}
}

// equal compares in constant time, so the time does not leak the secret.
func equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
