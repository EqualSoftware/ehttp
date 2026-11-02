package ehttp

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// CorsOptions sets the CORS policy of the Cors middleware. An empty Origin,
// Methods or Headers list means "*".
type CorsOptions struct {
	Origin        []string
	Methods       []string
	Headers       []string
	Credentials   bool
	CacheDuration time.Duration
}

// clone returns a copy that shares no slice with o.
func (o *CorsOptions) clone() *CorsOptions {
	c := *o
	c.Origin = slices.Clone(o.Origin)
	c.Methods = slices.Clone(o.Methods)
	c.Headers = slices.Clone(o.Headers)

	return &c
}

// withDefaults sets "*" for each empty list.
func (o *CorsOptions) withDefaults() {
	for _, list := range []*[]string{&o.Origin, &o.Methods, &o.Headers} {
		if len(*list) == 0 {
			*list = []string{"*"}
		}
	}
}

// Cors returns the CORS middleware (Fetch §3.2). CORS is off unless the
// developer adds it with Use, on the server or on a group.
//
// A preflight is an OPTIONS request with Origin and Access-Control-Request-Method
// (Fetch §3.2.2). The middleware answers it with 200, or 403 if the options do
// not allow it, and the chain ends there: the next middleware and the handler do
// not run, as a preflight is a standalone request (the same as go-chi/cors,
// Echo, gin-contrib/cors, Express cors and @fastify/cors). Place Cors before
// any middleware that can refuse a request without credentials, such as
// authentication, or the browser gets a refusal with no CORS headers.
// Any other request gets the CORS response headers, and the chain goes on.
//
// The options are copied. Credentials with the "*" origin, an invalid entry
// and a negative CacheDuration panic at boot.
func Cors(options CorsOptions) MiddlewareHandlerFunc {
	opts := options.clone()
	opts.withDefaults()

	if opts.Credentials && slices.Contains(opts.Origin, "*") {
		bootPanic("Cors", "Credentials with the \"*\" origin", "a browser refuses a wildcard origin on a response with credentials (Fetch §3.3.5)",
			"list the origins, for example CorsOptions{Origin: []string{\"https://app.example\"}, Credentials: true}")
	}

	validateCors(opts)

	return func(next Handler) HandlerFunc {
		return func(res *ResponseWriter, req *Request) {
			req.corsRan.Store(true)
			corsUpdateHeaders(res, req, opts)

			if !isPreflight(req) {
				next.ServeHTTP(res, req)
				return
			}

			corsPreflight(res, req, opts)
		}
	}
}

// isPreflight reports whether the request is a CORS preflight (Fetch §3.2.2).
func isPreflight(req *Request) bool {
	return req.Method() == MethodOptions && req.Origin() != "" && req.Header(HeaderAccessControlRequestMethod) != ""
}

// corsPreflight answers a preflight: 200 with the headers of Fetch §3.2.3, or
// 403 with no CORS header. The log of a refusal has the origin, the requested
// method and the names of the requested headers, and no other header.
func corsPreflight(res *ResponseWriter, req *Request, o *CorsOptions) {
	res.Vary(HeaderAccessControlRequestMethod)
	res.Vary(HeaderAccessControlRequestHeaders)

	if corsAcceptOrigin(req, o) && corsAcceptMethod(req, o) && corsAcceptHeaders(req, o) {
		res.Own(HeaderCacheControl, CacheControl(0, false))
		corsPreflightHeaders(res, req, o)

		res.SendStatus(StatusOK)

		return
	}

	res.SendStatus(StatusForbidden)

	req.newLogger().Warn("the request did not pass cors validation",
		zap.String("path", req.Path()),
		zap.String("host", req.Host()),
		zap.String("origin", res.redact.Value(HeaderOrigin, req.Origin())),
		zap.String("request_method", res.redact.Value(HeaderAccessControlRequestMethod, req.Header(HeaderAccessControlRequestMethod))),
		zap.Strings("request_headers", headerNames(req.Header(HeaderAccessControlRequestHeaders))),
	)
}

// headerNames splits a list of field names.
func headerNames(list string) []string {
	var names []string

	for name := range strings.SplitSeq(list, ",") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}

	return names
}

func corsAcceptHeaders(req *Request, options *CorsOptions) bool {
	if slices.Contains(options.Headers, "*") {
		return true
	}

	headers_raw := req.Header(HeaderAccessControlRequestHeaders)

	if headers_raw == "" {
		return true
	}

	headers_raw = strings.ReplaceAll(headers_raw, ", ", ",")
	headers := strings.Split(headers_raw, ",")

	for _, header := range headers {
		if !slices.Contains(options.Headers, http.CanonicalHeaderKey(header)) {
			return false
		}
	}

	return true
}

func corsAcceptMethod(req *Request, options *CorsOptions) bool {
	if slices.Contains(options.Methods, "*") {
		return true
	}

	method := req.Header(HeaderAccessControlRequestMethod)

	if method == "" {
		return false
	}

	return slices.Contains(options.Methods, strings.ToUpper(method))
}

func corsAcceptOrigin(req *Request, options *CorsOptions) bool {
	if slices.Contains(options.Origin, "*") {
		return true
	}

	origin := req.Origin()

	return origin != "" && slices.Contains(options.Origin, origin)
}

// corsUpdateHeaders sets the CORS headers of a response (Fetch §3.2).
// Vary: Origin is set when the allowlist is specific, so a cache keeps one
// response for each origin, also for requests without Origin.
func corsUpdateHeaders(res *ResponseWriter, req *Request, options *CorsOptions) {
	wildcard := slices.Contains(options.Origin, "*")

	if !wildcard {
		res.OwnVary(HeaderOrigin)
	}

	origin := req.Origin()

	if origin == "" || !corsAcceptOrigin(req, options) {
		return
	}

	if wildcard {
		origin = "*"
	}

	res.Own(HeaderAccessControlAllowOrigin, origin)

	if options.Credentials {
		res.Own(HeaderAccessControlAllowCredentials, "true")
	}
}

// corsPreflightHeaders sets the headers that a browser reads only from the
// preflight response (Fetch §3.2.3). With credentials, a literal * has no
// meaning, so the request values are echoed.
func corsPreflightHeaders(res *ResponseWriter, req *Request, options *CorsOptions) {
	methods := options.Methods
	headers := options.Headers

	if options.Credentials && slices.Contains(options.Methods, "*") {
		methods = corsEcho(req, HeaderAccessControlRequestMethod)
	}

	if options.Credentials && slices.Contains(options.Headers, "*") {
		headers = corsEcho(req, HeaderAccessControlRequestHeaders)
	}

	if len(methods) > 0 {
		res.Append(HeaderAccessControlAllowMethods, methods...)
	}

	if len(headers) > 0 {
		res.Append(HeaderAccessControlAllowHeaders, headers...)
	}

	if d := options.CacheDuration; d > 0 {
		res.Set(HeaderAccessControlMaxAge, strconv.Itoa(int(d.Seconds())))
	}
}

// corsEcho returns the request header name as one list member, or none if the
// header is empty.
func corsEcho(req *Request, name string) []string {
	if v := req.Header(name); v != "" {
		return []string{v}
	}

	return nil
}

// validateCors panics at boot for an entry that would fail at each request:
// an empty entry, an invalid origin value, or a method or header name that is
// not a token (RFC 9110 §5.6.2, §9.1). "*" is allowed.
func validateCors(o *CorsOptions) {
	for _, origin := range o.Origin {
		if origin == "" || !validFieldValue(origin) {
			bootPanic("Cors", "Origin entry "+quoteValue(origin), "it is empty or not a valid field value (RFC 9110 §5.5)",
				"use an origin such as \"https://app.example\", or \"*\"")
		}
	}

	for _, method := range o.Methods {
		if method != "*" && !validFieldName(method) {
			bootPanic("Cors", "Methods entry "+quoteValue(method), "it is not a token (RFC 9110 §9.1)",
				"use a method such as \"GET\", or \"*\"")
		}
	}

	if o.CacheDuration < 0 {
		bootPanic("Cors", "CacheDuration "+o.CacheDuration.String(), "Access-Control-Max-Age is delta-seconds, a non-negative integer (Fetch §3.2.3)",
			"use zero for no Max-Age, or a positive duration, for example CacheDuration: time.Hour")
	}

	for _, header := range o.Headers {
		if header != "*" && !validFieldName(header) {
			bootPanic("Cors", "Headers entry "+quoteValue(header), "it is not a token (RFC 9110 §5.6.2)",
				"use a header name such as \"Content-Type\", or \"*\"")
		}
	}
}
