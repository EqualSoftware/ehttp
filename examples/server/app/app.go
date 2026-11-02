// Package app builds the example server. main runs it and the golden and
// perf suites test it, so they all exercise the same configuration.
package app

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// nginxIP is the address of the nginx container in test/docker/compose.yaml.
// Only this peer may send X-Forwarded-* headers. Do not trust the whole
// network: Docker puts the gateway, and so a direct client, in it.
const nginxIP = "172.28.0.10"

// Demo credentials for the /admin group. They are for the demo only. A real
// service reads its secrets from the environment or a secret store.
const (
	demoUser     = "demo"
	demoPassword = "demo-password"
)

// New builds the example server. dir holds the templates/ and static/
// folders, so the caller picks the path that is valid from its working
// directory. modified is the Last-Modified time of "/".
func New(dir string, modified time.Time) *ehttp.Server {
	return build(dir, modified, nil, nil)
}

// NewQuiet is New with a logger that writes nothing. A benchmark uses it, so
// the log lines do not mix with the result rows.
func NewQuiet(dir string, modified time.Time) *ehttp.Server {
	return build(dir, modified, nil, zap.NewNop())
}

// NewObserved is New plus Prometheus metrics at /metrics (see metrics.go).
func NewObserved(dir string, modified time.Time) *ehttp.Server {
	return build(dir, modified, prometheus.NewRegistry(), nil)
}

func build(dir string, modified time.Time, reg *prometheus.Registry, logger *zap.Logger) *ehttp.Server {
	logging := ehttp.WithMode(ehttp.Production) // the default preset
	if logger != nil {
		logging = ehttp.WithLogger(logger)
	}

	server := ehttp.NewServer(
		logging,
		ehttp.WithPortInt(3000),
		ehttp.WithTrustedProxies(nginxIP),
		ehttp.WithTemplates(filepath.Join(dir, "templates", "*.html")),
	)

	var observer []ehttp.MiddlewareHandlerFunc
	var observerRoutes func(*ehttp.Server)
	if reg != nil {
		var mw ehttp.MiddlewareHandlerFunc
		mw, observerRoutes = observe(reg)
		observer = append(observer, mw)
	}

	server.Use(observer...)
	server.Use(
		middleware.Log(middleware.LogOptions{
			IgnorePaths: []string{"/healthcheck"},
		}),
		middleware.Recover(),
		middleware.ErrorCache(99*time.Second),
		middleware.RequestId(),
		middleware.AcceptEncoding(),
		middleware.Secure(middleware.DefaultSecureConfig),
		// Cors comes after the middlewares whose headers a preflight keeps, and
		// before BodyLimit and Timeout, so their refusals get the CORS headers.
		// Put it before any authentication.
		ehttp.Cors(ehttp.CorsOptions{
			Origin: []string{
				"http://localhost:3000",
			},
			Methods:       []string{ehttp.MethodGet, ehttp.MethodHead, ehttp.MethodOptions, ehttp.MethodPost},
			Credentials:   true,
			CacheDuration: 5 * time.Minute,
		}),
		middleware.BodyLimit("4KB"),
		middleware.Timeout(10*time.Second),
	)

	server.POST("/body", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		str, err := req.BodyString()

		if err != nil {
			if errors.Is(err, ehttp.ErrRequestBodyTooLarge) {
				res.SendStatus(ehttp.StatusRequestEntityTooLarge)
				return
			}

			res.SendStatus(ehttp.StatusBadRequest)
			return
		}

		res.Status(ehttp.StatusCreated)
		res.String(str)
	})

	server.GET("/stats", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		res.SendStatus(200)
		res.String(fmt.Sprintf("Protocol: %s\nHost: %s\nHostname: %s\n", req.Protocol(), req.Host(), req.Hostname()))
	})

	server.GET("/", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		res.Vary("User-Agent")

		if req.Accepts(ehttp.MIMETextHTML) {
			res.Render("index.html", map[string]interface{}{
				"Message": "Hello, World!",
			})
			return
		}

		if req.Accepts(ehttp.MIMEApplicationJSON) {
			res.JSON(map[string]interface{}{
				"message": "Hello, World!",
			})
			return
		}

		res.String("Hello, World!")
	}, middleware.Cache(5*time.Minute), middleware.LastModified(modified), middleware.WeakEtag())

	server.GET("/healthcheck", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		res.SendStatus(200)
	}, middleware.Cache(0))

	server.GET("/timeout", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		time.Sleep(1 * time.Second)
		res.SendStatus(200)
	})

	server.GET("/native", ehttp.WrapHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("Hello!"))
	})), middleware.Cache(5*time.Minute))

	server.GET("/file", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		res.SendFile(filepath.Join(dir, "static", "pudim.png"))
	}, middleware.Cache(5*time.Minute))

	api_v1 := server.Route("/api/v1")
	api_v1.GET("/hello/:name", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		data := map[string]interface{}{
			"name": "Hello, " + req.Param("name"),
		}

		res.Status(ehttp.StatusOK)
		res.JSON(data)
	}, middleware.Cache(5*time.Minute))

	admin := server.Route("/admin", basicAuth(demoUser, demoPassword))
	admin.GET("/me", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		res.String("Hello, " + demoUser + "\n")
	})

	if observerRoutes != nil {
		observerRoutes(server)
	}

	return server
}

// basicAuth checks HTTP Basic credentials (RFC 7617). A missing or wrong
// credential gets 401 with WWW-Authenticate (RFC 9110 section 11.6.1).
func basicAuth(user, password string) ehttp.MiddlewareHandlerFunc {
	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			u, p, ok := req.Request.BasicAuth()
			if ok && secureEqual(u, user) && secureEqual(p, password) {
				next.ServeHTTP(res, req)
				return
			}

			res.Own(ehttp.HeaderWWWAuthenticate, `Basic realm="demo", charset="UTF-8"`)
			res.SendStatus(ehttp.StatusUnauthorized)
		}
	}
}

func secureEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
