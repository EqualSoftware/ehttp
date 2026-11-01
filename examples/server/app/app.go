// Package app builds the example server. main runs it and the golden and
// perf suites test it, so they all exercise the same configuration.
package app

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/middleware"
)

// New builds the example server. dir holds the templates/ and static/
// folders, so the caller picks the path that is valid from its working
// directory. modified is the Last-Modified time of "/".
func New(dir string, modified time.Time) *ehttp.Server {
	server := ehttp.NewServer(
		ehttp.WithPortInt(3000),
		ehttp.WithTemplates(filepath.Join(dir, "templates", "*.html")),
		ehttp.WithCors(ehttp.CorsOptions{
			Origin: []string{
				"http://localhost:3000",
			},
			Methods:       []string{ehttp.MethodGet, ehttp.MethodHead, ehttp.MethodOptions, ehttp.MethodPost},
			Credentials:   true,
			CacheDuration: 5 * time.Minute,
		}),
	)

	server.Use(
		middleware.Log(middleware.LogOptions{
			IgnorePaths: []string{"/healthcheck"},
		}),
		middleware.Recover(),
		middleware.ErrorCache(99*time.Second),
		middleware.RequestId(),
		middleware.AcceptEncoding(),
		middleware.Secure(middleware.DefaultSecureConfig),
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

	return server
}
