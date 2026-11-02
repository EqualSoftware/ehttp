package ehttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"go.uber.org/zap"

	"github.com/EqualSoftware/ehttp/internal/redact"
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

// quoteField is quoteValue for the value of the header name, with the deny
// list of the server (the default one if the request has no response).
func (req *Request) quoteField(name, value string) string {
	var m *redact.Matcher
	if req.res != nil {
		m = req.res.redact
	}

	if m.Sensitive(name) {
		return redact.Mask
	}

	return quoteValue(value)
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

// Param returns a route param. The value is percent-decoded once, per segment
// (RFC 3986 §2.1). When a value has a ".." part (split on "/", literal or
// decoded), the router sends 403 and does not call the handler. A decoded NUL
// or backslash gives 400. A decoded "/" stays as data, and so does a "." part.
// Still confine a wildcard ("...") value before file use, for example with
// os.Root or filepath.IsLocal. Static segments compare the raw, still-escaped
// path, so "/us%65rs" does not match "/users".
func (req *Request) Param(name string) string {
	return req.Request.PathValue(name)
}

// Accepts reports if the Accept header allows the media type value. The answer
// depends on Accept, so it adds Accept to Vary (RFC 9110 §12.5.5).
//
// A missing Accept header allows every type. The most specific media range that
// matches value sets its q value, and q=0 means not acceptable (RFC 9110
// §12.4.2). Media type parameters in the header are ignored. An invalid header
// or an invalid value is never acceptable, so the caller answers with 406.
func (req *Request) Accepts(value string) bool {
	if req.res != nil {
		req.res.varyQuiet(HeaderAccept)
	}

	typ, sub, ok := parseMediaType(value)

	if !ok {
		req.newLogger().Error(fmt.Sprintf("invalid accepts value '%s'", value))
		return false
	}

	best, seen, ok := req.acceptBest(typ, sub)

	if !ok {
		return false
	}

	return !seen || best.q > 0
}

// acceptBest finds the best match of the media type in all Accept lines. seen
// is false if the header has no member. ok is false if the header is invalid.
func (req *Request) acceptBest(typ, sub string) (best acceptMatch, seen, ok bool) {
	for _, line := range req.Request.Header.Values(HeaderAccept) {
		for rest, more := line, true; more; {
			var member string

			member, rest, more, ok = cutUnquoted(rest, ',')
			member = strings.Trim(member, " \t")

			if ok && member == "" {
				continue
			}

			rng, valid := parseAcceptRange(member)

			if !ok || !valid {
				req.newLogger().Warn("ehttp: invalid Accept header", zap.String("value", req.quoteField(HeaderAccept, line)))
				return best, seen, false
			}

			seen = true
			best.add(rng, typ, sub)
		}
	}

	return best, seen, true
}

// cutUnquoted cuts s at the first sep that is not inside a quoted string
// (RFC 9110 §5.6.4). found is true if it cut. ok is false if a quote does not
// end.
func cutUnquoted(s string, sep byte) (before, after string, found, ok bool) {
	quoted := false

	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quoted && c == '\\':
			i++
		case c == '"':
			quoted = !quoted
		case c == sep && !quoted:
			return s[:i], s[i+1:], true, true
		}
	}

	return s, "", false, !quoted
}

// validQuoted reports if s is a quoted-string (RFC 9110 §5.6.4).
func validQuoted(s string) bool {
	if len(s) < 2 || s[0] != '"' {
		return false
	}

	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '"':
			return i == len(s)-1
		case '\\':
			i++
		}
	}

	return false
}

// acceptMatch holds the best match found so far: the specificity of the range
// (0 none, 1 "*/*", 2 "type/*", 3 "type/subtype") and its q value.
type acceptMatch struct {
	specificity int
	q           int
}

func (m *acceptMatch) add(r acceptRange, typ, sub string) {
	spec := r.specificity(typ, sub)

	switch {
	case spec == 0 || spec < m.specificity:
	case spec > m.specificity:
		m.specificity, m.q = spec, r.q
	default:
		m.q = max(m.q, r.q)
	}
}

// acceptRange is one media range of an Accept header. q is in thousandths.
type acceptRange struct {
	typ, sub string
	q        int
}

func (r acceptRange) specificity(typ, sub string) int {
	switch {
	case r.typ == "*":
		return 1
	case !strings.EqualFold(r.typ, typ):
		return 0
	case r.sub == "*":
		return 2
	case strings.EqualFold(r.sub, sub):
		return 3
	}

	return 0
}

// parseAcceptRange parses "type/subtype", "type/*" or "*/*" with parameters
// (RFC 9110 §12.5.1). Parameters other than q are checked and ignored. They
// do not count for specificity.
func parseAcceptRange(member string) (acceptRange, bool) {
	rng := acceptRange{q: 1000}
	head, params, more, ok := cutUnquoted(member, ';')

	if !ok {
		return rng, false
	}

	typ, sub, ok := strings.Cut(strings.TrimRight(head, " \t"), "/")

	if !ok || !isToken(typ) && typ != "*" || !isToken(sub) && sub != "*" || typ == "*" && sub != "*" {
		return rng, false
	}

	rng.typ, rng.sub = typ, sub

	for more {
		var p string

		p, params, more, ok = cutUnquoted(params, ';')
		p = strings.Trim(p, " \t")

		if !ok {
			return rng, false
		}

		if p == "" {
			continue
		}

		name, val, ok := strings.Cut(p, "=")

		if !ok || !isToken(name) || !isToken(val) && !validQuoted(val) {
			return rng, false
		}

		if name != "q" && name != "Q" {
			continue
		}

		if rng.q, ok = parseQValue(val); !ok {
			return rng, false
		}
	}

	return rng, true
}

// parseQValue parses a weight (RFC 9110 §12.4.2) into thousandths.
func parseQValue(s string) (int, bool) {
	whole, frac, _ := strings.Cut(s, ".")

	if len(frac) > 3 || whole != "0" && whole != "1" {
		return 0, false
	}

	q := int(whole[0]-'0') * 1000
	scale := 100

	for i := range len(frac) {
		d := int(frac[i] - '0')

		if d < 0 || d > 9 || whole == "1" && d != 0 {
			return 0, false
		}

		q += d * scale
		scale /= 10
	}

	return q, true
}

// parseMediaType parses "type/subtype" with optional parameters, which it drops.
func parseMediaType(value string) (typ, sub string, ok bool) {
	head, _, _ := strings.Cut(value, ";")
	typ, sub, ok = strings.Cut(strings.Trim(head, " \t"), "/")

	return typ, sub, ok && isToken(typ) && isToken(sub)
}

// isToken reports if s is a non-empty token (RFC 9110 §5.6.2).
func isToken(s string) bool {
	if s == "" {
		return false
	}

	for i := range len(s) {
		c := s[i]

		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}

		if strings.IndexByte("!#$%&'*+-.^_`|~", c) < 0 {
			return false
		}
	}

	return true
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

	return json.Unmarshal(b, v)
}
