package ehttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"

	"go.uber.org/zap"
)

var ErrRequestBodyTooLarge = errors.New("request body too large")

type Request struct {
	Request *http.Request

	proxies trustedProxies
	peer    atomic.Uint32 // peerUnknown, peerTrusted or peerUntrusted; the result is the same in every goroutine
	// forwardedWarned is true after the warning for an invalid forwarded header.
	forwardedWarned atomic.Bool
	res             *ResponseWriter            // for Vary; nil in a Request built by hand
	logger          *zap.Logger                // the server logger; nil in a Request built by hand
	log             atomic.Pointer[zap.Logger] // logger with the request fields, built on first use
}

var nopLogger = zap.NewNop()

// Logger returns the logger of the server with the fields request_id, method
// and route (the registered pattern). A field with an empty value is left
// out. The first call caches the logger, so a middleware that adds request
// data (for example RequestId) must run before the first call. It is safe
// to call from many goroutines.
func (req *Request) Logger() *zap.Logger {
	if l := req.log.Load(); l != nil {
		return l
	}

	req.log.CompareAndSwap(nil, req.newLogger())

	return req.log.Load()
}

// baseLogger returns the server logger, or a no-op logger.
func (req *Request) baseLogger() *zap.Logger {
	if req.logger == nil {
		return nopLogger
	}

	return req.logger
}

// newLogger builds the logger of the request and does not cache it. The
// library uses it, so its own log calls never freeze the fields before a
// middleware like RequestId has run.
func (req *Request) newLogger() *zap.Logger {
	fields := make([]zap.Field, 0, 3)

	if id := req.Header(HeaderXRequestID); id != "" {
		fields = append(fields, zap.String("request_id", id))
	}

	fields = append(fields, zap.String("method", req.Method()))

	if route := req.RoutePath(); route != "" {
		fields = append(fields, zap.String("route", route))
	}

	return req.baseLogger().With(fields...)
}

func (req *Request) WithContext(ctx context.Context) *Request {
	req.Request = req.Request.WithContext(ctx)
	return req
}

func (req *Request) Context() context.Context {
	return req.Request.Context()
}

func (req *Request) Method() string {
	return req.Request.Method
}

func (req *Request) RoutePath() string {
	return req.Request.Pattern
}

// Protocol returns "https" for a TLS connection and "http" otherwise.
// A trusted proxy can set it with X-Forwarded-Proto (see WithTrustedProxies).
func (req *Request) Protocol() string {
	if v := req.forwardedProto(); v != "" {
		return v
	}

	if req.Request.TLS != nil {
		return "https"
	}

	return "http"
}

// Cookie returns the value of the cookie name, or "" if it is absent.
func (req *Request) Cookie(name string) string {
	c, err := req.Request.Cookie(name)
	if err != nil {
		return ""
	}

	return c.Value
}

// Host returns the Host header. A trusted proxy can set it with
// X-Forwarded-Host (see WithTrustedProxies).
func (req *Request) Host() string {
	if host := req.forwardedHost(); host != "" {
		return host
	}

	return req.Request.Host
}

// Hostname returns Host without the port, in lowercase (RFC 3986 §3.2.2).
func (req *Request) Hostname() string {
	return strings.ToLower(hostOnly(req.Host()))
}

func (req *Request) Path() string {
	return req.Request.URL.EscapedPath()
}

func (req *Request) Query(name string) string {
	return req.Request.URL.Query().Get(name)
}

func (req *Request) Header(name string) string {
	return req.Request.Header.Get(name)
}

func (req *Request) Origin() string {
	return req.Header(HeaderOrigin)
}

func (req *Request) UserAgent() string {
	return req.Header(HeaderUserAgent)
}

func (req *Request) Param(name string) string {
	return req.Request.PathValue(name)
}

// Accepts reports if the Accept header allows the media type value. The answer
// depends on Accept, so it adds Accept to Vary (RFC 9110 §12.5.5).
func (req *Request) Accepts(value string) bool {
	if req.res != nil {
		req.res.varyQuiet(HeaderAccept)
	}

	accept := req.Header(HeaderAccept)

	if accept == "" || strings.Contains(accept, "*/*") {
		return true
	}

	mediatype, _, err := mime.ParseMediaType(accept)

	if err != nil {
		req.newLogger().Error(fmt.Sprintf("invalid header accept '%s'", accept), zap.Error(err))
		return false
	}

	vmediatype, _, err := mime.ParseMediaType(value)

	if err != nil {
		req.newLogger().Error(fmt.Sprintf("invalid accepts value '%s'", value), zap.Error(err))
		return false
	}

	mediatype_reg, err := regexp.Compile(mediatype)

	if err != nil {
		req.newLogger().Error("invalid accepts regex", zap.Error(err))
		return false
	}

	return mediatype_reg.Match([]byte(vmediatype))
}

func (req *Request) Body() ([]byte, error) {
	b, err := io.ReadAll(req.Request.Body)

	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return nil, ErrRequestBodyTooLarge
		}

		return nil, err
	}

	return b, nil
}

func (req *Request) BodyString() (string, error) {
	b, err := io.ReadAll(req.Request.Body)

	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return "", ErrRequestBodyTooLarge
		}

		return "", err
	}

	return string(b), nil
}

func (req *Request) BodyJSON(v interface{}) error {
	b, err := io.ReadAll(req.Request.Body)

	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return ErrRequestBodyTooLarge
		}

		return err
	}

	if err := json.Unmarshal(b, v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}

	return nil
}
