package ehttp

import (
	"context"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os/signal"
	"slices"
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

const (
	defaultReadHeaderTimeout = 10 * time.Second
	defaultShutdownTimeout   = 25 * time.Second
)

type Server struct {
	router *Router
	port   string

	readHeaderTimeout time.Duration
	readTimeout       time.Duration
	writeTimeout      time.Duration
	idleTimeout       time.Duration
	maxHeaderBytes    int
	shutdownTimeout   time.Duration
}

// Mode picks the preset of the server logger.
type Mode int

const (
	// Production is the default. The logger writes JSON at info level.
	Production Mode = iota
	// Development writes console output at debug level.
	Development
)

// WithMode picks the logger preset of the server. WithLogger wins over it,
// in any order of the options. An error from zap panics at boot.
func WithMode(m Mode) serverOption {
	return func(s *Server) { s.router.mode = m }
}

// WithLogger sets the logger of the server instead of the preset of WithMode.
// A nil logger panics. The server never changes the global zap logger.
func WithLogger(l *zap.Logger) serverOption {
	if l == nil {
		bootPanic("WithLogger", "a nil logger", "the server needs a logger for its own messages (author decision)",
			"pass a *zap.Logger, or remove WithLogger to use the preset of WithMode")
	}

	return func(s *Server) { s.router.logger = l }
}

// presetLogger builds the logger of a mode. An error panics at boot.
func presetLogger(m Mode) *zap.Logger {
	build := zap.NewProduction
	if m == Development {
		build = zap.NewDevelopment
	}

	l, err := build()
	if err != nil {
		bootPanic("WithMode", "the logger preset", "zap cannot build it: "+err.Error(),
			"pass your own logger with WithLogger")
	}

	return l
}

// mustBeNonNegative panics when a duration option is negative.
func mustBeNonNegative(option string, d time.Duration) {
	if d < 0 {
		bootPanic(option, "duration "+d.String(), "a timeout cannot be negative (net/http.Server)",
			"use zero for no limit, or a positive duration, for example "+option+"(30 * time.Second)")
	}
}

// WithTrustedProxies sets the peers that can send X-Forwarded-* headers.
// A value is a CIDR, a single IP, or loopback, linklocal or uniquelocal.
// Without it, no peer is trusted. An invalid value panics.
func WithTrustedProxies(cidrs ...string) serverOption {
	list := parseTrustedProxies(cidrs)

	return func(s *Server) {
		s.router.proxies = list
	}
}

// WithReadHeaderTimeout sets http.Server.ReadHeaderTimeout. The default is 10 s.
func WithReadHeaderTimeout(d time.Duration) serverOption {
	mustBeNonNegative("WithReadHeaderTimeout", d)

	return func(s *Server) { s.readHeaderTimeout = d }
}

// WithReadTimeout sets http.Server.ReadTimeout.
func WithReadTimeout(d time.Duration) serverOption {
	mustBeNonNegative("WithReadTimeout", d)

	return func(s *Server) { s.readTimeout = d }
}

// WithWriteTimeout sets http.Server.WriteTimeout.
func WithWriteTimeout(d time.Duration) serverOption {
	mustBeNonNegative("WithWriteTimeout", d)

	return func(s *Server) { s.writeTimeout = d }
}

// WithIdleTimeout sets http.Server.IdleTimeout.
func WithIdleTimeout(d time.Duration) serverOption {
	mustBeNonNegative("WithIdleTimeout", d)

	return func(s *Server) { s.idleTimeout = d }
}

// WithMaxHeaderBytes sets http.Server.MaxHeaderBytes.
func WithMaxHeaderBytes(n int) serverOption {
	if n < 0 {
		bootPanic("WithMaxHeaderBytes", "size "+strconv.Itoa(n), "a size cannot be negative (net/http.Server)",
			"use zero for the net/http default, or a positive size, for example WithMaxHeaderBytes(1 << 20)")
	}

	return func(s *Server) { s.maxHeaderBytes = n }
}

// WithShutdownTimeout sets how long Listen waits for in-flight requests after
// its context is done. The default is 25 s, below the 30 s grace period of
// Kubernetes. Zero means no limit.
func WithShutdownTimeout(d time.Duration) serverOption {
	mustBeNonNegative("WithShutdownTimeout", d)

	return func(s *Server) { s.shutdownTimeout = d }
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
			panic(fmt.Errorf("ehttp: pattern %s in WithTemplates: html/template cannot load it (%w). Fix: %s", quoteValue(v), err, "pass a glob that matches valid template files, for example \"views/*.html\"."))
		}

		s.router.template = t
	}
}

func WithCors(v CorsOptions) serverOption {
	return func(s *Server) {
		if len(v.Methods) != 0 {
			s.router.cors.Methods = slices.Clone(v.Methods)
		}

		if len(v.Origin) != 0 {
			s.router.cors.Origin = slices.Clone(v.Origin)
		}

		if len(v.Headers) != 0 {
			s.router.cors.Headers = slices.Clone(v.Headers)
		}

		s.router.cors.Credentials = v.Credentials
		s.router.cors.CacheDuration = v.CacheDuration
	}
}

var DefaultCorsOptions = &CorsOptions{
	Origin:      []string{"*"},
	Methods:     []string{"*"},
	Headers:     []string{"*"},
	Credentials: false,
}

// clone returns a copy that shares no slice with o.
func (o *CorsOptions) clone() *CorsOptions {
	c := *o
	c.Origin = slices.Clone(o.Origin)
	c.Methods = slices.Clone(o.Methods)
	c.Headers = slices.Clone(o.Headers)

	return &c
}

func NewServer(options ...serverOption) *Server {
	server := &Server{
		port:              "3000",
		readHeaderTimeout: defaultReadHeaderTimeout,
		shutdownTimeout:   defaultShutdownTimeout,
		router: &Router{
			cors: DefaultCorsOptions.clone(),
		},
	}

	for _, apply := range options {
		apply(server)
	}

	if server.router.cors.Credentials && slices.Contains(server.router.cors.Origin, "*") {
		bootPanic("WithCors", "Credentials with the \"*\" origin", "a browser refuses a wildcard origin on a response with credentials (Fetch §3.3.5)",
			"list the origins, for example CorsOptions{Origin: []string{\"https://app.example\"}, Credentials: true}")
	}

	validateCors(server.router.cors)

	if server.router.logger == nil {
		server.router.logger = presetLogger(server.router.mode)
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

// ServeHTTP makes Server an http.Handler that serves requests through its router.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

// Listen serves until c is done, then shuts down gracefully.
// It returns a bind or serve error. A clean shutdown returns nil.
func (s *Server) Listen(c context.Context) error {
	server := s.httpServer()

	ctx, quit := signal.NotifyContext(c, syscall.SIGINT, syscall.SIGTERM)
	defer quit()

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", server.Addr)
	if err != nil {
		return err
	}

	s.router.logger.Info("server is running", zap.String("port", s.port))

	serveErr := make(chan error, 1)

	go func() { serveErr <- server.Serve(ln) }()

	select {
	case err := <-serveErr:
		_ = server.Close()

		s.router.logger.Error("shutting down the server with error", zap.Error(err))

		return err
	case <-ctx.Done():
	}

	// Release the signal handler: a second signal takes the Go default and
	// ends the process while the shutdown waits.
	quit()

	s.router.logger.Info("shutting down the server gracefully")

	return s.shutdown(context.WithoutCancel(ctx), server, serveErr)
}

// httpServer builds the http.Server from the options of s.
func (s *Server) httpServer() *http.Server {
	return &http.Server{
		Addr:                         ":" + s.port,
		Handler:                      s.router,
		DisableGeneralOptionsHandler: true,
		ReadHeaderTimeout:            s.readHeaderTimeout,
		ReadTimeout:                  s.readTimeout,
		WriteTimeout:                 s.writeTimeout,
		IdleTimeout:                  s.idleTimeout,
		MaxHeaderBytes:               s.maxHeaderBytes,
	}
}

// shutdown gets a context without the cancel of the parent, which is done.
// Its deadline is the shutdown timeout.
func (s *Server) shutdown(ctx context.Context, server *http.Server, serveErr <-chan error) error {
	if s.shutdownTimeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, s.shutdownTimeout)
		defer cancel()
	}

	err := server.Shutdown(ctx)
	if err != nil {
		_ = server.Close()
	}

	<-serveErr //nolint:errcheck // Serve returns ErrServerClosed after Shutdown

	return err
}
