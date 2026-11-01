package ehttp

import (
	"context"
	"html/template"
	"net/http"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"go.uber.org/zap"
)

type CorsOptions struct {
	Origin        []string
	Methods       []string
	Headers       []string
	Credentials   bool
	CacheDuration time.Duration
}

type serverOption func(*Server)

type Server struct {
	router *Router
	port   string
}

func WithPort(v string) serverOption {
	return func(s *Server) {
		s.port = v
	}
}

func WithPortInt(v int64) serverOption {
	return func(s *Server) {
		s.port = strconv.FormatInt(v, 10)
	}
}

func WithTemplates(v string) serverOption {
	return func(s *Server) {
		t, err := template.ParseGlob(v)

		if err != nil {
			zap.L().Fatal("server.WithTemplates error", zap.Error(err))
		}

		s.router.template = t
	}
}

func WithCors(v CorsOptions) serverOption {
	return func(s *Server) {
		if len(v.Methods) != 0 {
			s.router.cors.Methods = v.Methods
		}

		if len(v.Origin) != 0 {
			s.router.cors.Origin = v.Origin
		}

		if len(v.Headers) != 0 {
			s.router.cors.Headers = v.Headers
		}

		s.router.cors.Credentials = v.Credentials
	}
}

var DefaultCorsOptions = &CorsOptions{
	Origin:      []string{"*"},
	Methods:     []string{"*"},
	Headers:     []string{"*"},
	Credentials: false,
}

func NewServer(options ...serverOption) *Server {
	server := &Server{
		port: "3000",
		router: &Router{
			cors: DefaultCorsOptions,
		},
	}

	for _, apply := range options {
		apply(server)
	}

	return server
}

func (s *Server) Use(m ...MiddlewareHandlerFunc) {
	s.router.Use(m...)
}

func (s *Server) Route(prefix string, m ...MiddlewareHandlerFunc) *Router {
	return s.router.Route(prefix, m...)
}

func (s *Server) GET(pattern string, h HandlerFunc, m ...MiddlewareHandlerFunc) {
	s.router.GET(pattern, h, m...)
}

func (s *Server) POST(pattern string, h HandlerFunc, m ...MiddlewareHandlerFunc) {
	s.router.POST(pattern, h, m...)
}

func (s *Server) PUT(pattern string, h HandlerFunc, m ...MiddlewareHandlerFunc) {
	s.router.PUT(pattern, h, m...)
}

func (s *Server) PATCH(pattern string, h HandlerFunc, m ...MiddlewareHandlerFunc) {
	s.router.PATCH(pattern, h, m...)
}

func (s *Server) DELETE(pattern string, h HandlerFunc, m ...MiddlewareHandlerFunc) {
	s.router.DELETE(pattern, h, m...)
}

func (s *Server) CONNECT(pattern string, h HandlerFunc, m ...MiddlewareHandlerFunc) {
	s.router.CONNECT(pattern, h, m...)
}

func (s *Server) TRACE(pattern string, h HandlerFunc, m ...MiddlewareHandlerFunc) {
	s.router.TRACE(pattern, h, m...)
}

func (s *Server) Listen(c context.Context) error {
	server := &http.Server{
		Addr:                         ":" + s.port,
		Handler:                      s.router,
		DisableGeneralOptionsHandler: true,
	}

	zap.L().Info("server is running", zap.String("port", s.port))

	ctx, quit := signal.NotifyContext(c, syscall.SIGINT, syscall.SIGTERM)
	defer quit()

	var exit_message error

	go (func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			exit_message = err
			quit()
		}
	})()

	<-ctx.Done()
	if exit_message == nil {
		zap.L().Info("shutting down the server gracefully")
	} else {
		zap.L().Error("shutting down the server with error", zap.Error(exit_message))
	}

	server.Shutdown(ctx)

	return nil
}
