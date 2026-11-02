package ehttp

import (
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

type MiddlewareHandlerFunc func(next Handler) HandlerFunc

type Handler interface {
	ServeHTTP(*ResponseWriter, *Request)
}

type HandlerFunc func(res *ResponseWriter, req *Request)

func (f HandlerFunc) ServeHTTP(res *ResponseWriter, req *Request) {
	f(res, req)
}

func WrapHandler(h http.Handler) HandlerFunc {
	return func(res *ResponseWriter, req *Request) {
		h.ServeHTTP(res, req.Request)
	}
}

type Router struct {
	parent      *Router
	prefix      string
	template    *template.Template
	middlewares []MiddlewareHandlerFunc
	routes      []*route
	cors        *CorsOptions
	proxies     trustedProxies
	logger      *zap.Logger
	mode        Mode
}

type route struct {
	method      string
	pattern     string
	segments    []string
	regexes     []*regexp.Regexp
	handle      HandlerFunc
	middlewares []MiddlewareHandlerFunc
}

func (r *Router) Use(m ...MiddlewareHandlerFunc) {
	r.middlewares = append(r.middlewares, m...)
}

func (r *Router) Route(prefix string, m ...MiddlewareHandlerFunc) *Router {
	return &Router{
		parent:      r,
		prefix:      prefix,
		middlewares: slices.Clone(m),
	}
}

// Handle registers a route. A group passes its own middlewares to its
// parent, so the middlewares of the outer group run first.
func (r *Router) Handle(method, pattern string, h HandlerFunc, m ...MiddlewareHandlerFunc) {
	if !validFieldName(method) {
		bootPanic("Handle", "method "+quoteValue(method), "it is not a token (RFC 9110 §9.1)",
			"use a method name such as \"GET\", or a helper such as router.GET")
	}

	if !standardMethod(method) {
		bootPanic("Handle", "method "+quoteValue(method), "it is not a method that ehttp routes: GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS, CONNECT, TRACE (RFC 9110 §9.3, RFC 5789)",
			"use one of these methods, or a helper such as router.GET")
	}

	if r.parent != nil {
		all := make([]MiddlewareHandlerFunc, 0, len(r.middlewares)+len(m))
		all = append(all, r.middlewares...)
		all = append(all, m...)
		r.parent.Handle(method, r.prefix+pattern, h, all...)
		return
	}

	for _, middleware := range slices.Backward(m) {
		h = middleware(h)
	}

	for _, middleware := range slices.Backward(r.middlewares) {
		h = middleware(h)
	}

	segments := strings.Split(r.prefix+pattern, "/")

	if segments[len(segments)-1] == "" {
		segments = segments[:len(segments)-1]
	}

	mustNotHaveDotSegment(strings.ToUpper(method), r.prefix+pattern, segments)
	r.mustBeNew(strings.ToUpper(method), r.prefix+pattern, segments)

	r.routes = append(r.routes, &route{
		method:      strings.ToUpper(method),
		pattern:     r.prefix + pattern,
		segments:    segments,
		regexes:     compileSegments(pattern, segments),
		handle:      h,
		middlewares: m,
	})
}

// mustNotHaveDotSegment panics when a static segment of a pattern can never be
// reached. A client removes dot segments before it sends (RFC 3986 §5.2.4),
// and the router refuses a NUL or a backslash. A static segment compares the
// raw text, so a segment that decodes to "." or ".." (for example "%2e"), or
// holds a NUL or a backslash (for example "%00", "%5c"), is also refused.
func mustNotHaveDotSegment(method, pattern string, segments []string) {
	for _, segment := range segments {
		if strings.HasPrefix(segment, ":") || segment == "..." {
			continue
		}

		decoded, err := url.PathUnescape(segment)
		if err != nil {
			decoded = segment
		}

		if decoded != "." && decoded != ".." && !strings.ContainsAny(decoded, "\\\x00") {
			continue
		}

		bootPanic(method+" "+pattern, "the segment "+quoteValue(segment), "a client removes dot segments before it sends (RFC 3986 §5.2.4), and the router refuses a NUL or a backslash in a path value, so this route can never be reached",
			"remove the segment")
	}
}

// mustBeNew panics when a route with the same method and the same shape of
// pattern exists: the first one always wins, so the second never runs. A
// parameter name does not change the shape, a regex does.
func (r *Router) mustBeNew(method, pattern string, segments []string) {
	key := routeKey(segments)

	for _, old := range r.routes {
		if old.method != method || routeKey(old.segments) != key {
			continue
		}

		bootPanic(method+" "+pattern, "a second handler", "the route "+method+" "+old.pattern+" matches the same requests and is first, so this one never runs",
			"remove one of them, or give them different paths")
	}
}

// routeKey writes the segments with the name of each parameter removed.
func routeKey(segments []string) string {
	var b strings.Builder

	for _, segment := range segments {
		b.WriteByte('/')

		if !strings.HasPrefix(segment, ":") {
			b.WriteString(segment)
			continue
		}

		_, expr, _ := strings.Cut(segment, "|")
		b.WriteString(":|" + expr)
	}

	return b.String()
}

// standardMethod reports whether method is one that ehttp routes. The name
// is case-insensitive, as Handle upper-cases it.
func standardMethod(method string) bool {
	switch strings.ToUpper(method) {
	case MethodGet, MethodHead, MethodPost, MethodPut, MethodPatch, MethodDelete, MethodOptions, MethodConnect, MethodTrace:
		return true
	}

	return false
}

// compileSegments compiles the regex of each ":name|regex" segment one time.
// The regex is anchored. An invalid regex panics at registration.
func compileSegments(pattern string, segments []string) []*regexp.Regexp {
	regexes := make([]*regexp.Regexp, len(segments))

	for i, segment := range segments {
		if !strings.HasPrefix(segment, ":") {
			continue
		}

		_, expr, ok := strings.Cut(segment[1:], "|")
		if !ok {
			continue
		}

		// Compile expr alone first: an unbalanced ")" must not escape the group.
		if _, err := regexp.Compile(expr); err != nil {
			bootPanic("route "+quoteValue(pattern), "regex "+quoteValue(expr), "it does not compile: "+err.Error()+" (RE2 syntax)",
				"write a valid RE2 expression, for example \":id|[0-9]+\"")
		}

		re := regexp.MustCompile("^(?:" + expr + ")$")

		regexes[i] = re
	}

	return regexes
}

func (r *Router) GET(pattern string, h HandlerFunc, m ...MiddlewareHandlerFunc) {
	r.Handle(MethodGet, pattern, h, m...)
}

func (r *Router) POST(pattern string, h HandlerFunc, m ...MiddlewareHandlerFunc) {
	r.Handle(MethodPost, pattern, h, m...)
}

func (r *Router) PUT(pattern string, h HandlerFunc, m ...MiddlewareHandlerFunc) {
	r.Handle(MethodPut, pattern, h, m...)
}

func (r *Router) PATCH(pattern string, h HandlerFunc, m ...MiddlewareHandlerFunc) {
	r.Handle(MethodPatch, pattern, h, m...)
}

func (r *Router) DELETE(pattern string, h HandlerFunc, m ...MiddlewareHandlerFunc) {
	r.Handle(MethodDelete, pattern, h, m...)
}

func (r *Router) CONNECT(pattern string, h HandlerFunc, m ...MiddlewareHandlerFunc) {
	r.Handle(MethodConnect, pattern, h, m...)
}

func (r *Router) TRACE(pattern string, h HandlerFunc, m ...MiddlewareHandlerFunc) {
	r.Handle(MethodTrace, pattern, h, m...)
}

func (router *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req := &Request{
		Request: r,
		proxies: router.proxies,
		logger:  router.logger,
	}
	res := &ResponseWriter{
		w:        w,
		request:  r,
		req:      req,
		template: router.template,
		mode:     router.mode,
	}
	req.res = res

	corsUpdateHeaders(res, req, router.cors)

	method := req.Method()
	path := req.Path()
	url_segments := strings.Split(path, "/")

	if url_segments[len(url_segments)-1] == "" {
		url_segments = url_segments[:len(url_segments)-1]
	}

	endpoint, first := router.find(method, url_segments)
	matched := first != nil

	if endpoint != nil {
		if status := endpoint.bind(req, url_segments); status != 0 {
			router.wrap(router.statusHandler(status)).ServeHTTP(res, req)
			return
		}

		req.Request.Pattern = endpoint.pattern
		endpoint.handle(res, req)

		return
	}

	if matched {
		if status := router.refusal(url_segments); status != 0 {
			router.wrap(router.statusHandler(status)).ServeHTTP(res, req)
			return
		}
	}

	if matched && method != MethodOptions {
		router.wrap(router.methodNotAllowedHandler(url_segments)).ServeHTTP(res, req)
		return
	}

	if method == MethodOptions {
		if matched {
			router.setAllow(res, url_segments)
		}

		router.wrap(router.optionsHandler).ServeHTTP(res, req)

		return
	}

	router.wrap(router.notFoundHandler).ServeHTTP(res, req)
}

// find returns the route that serves the method, and the first route that
// matched the path (nil if none matched). A route of the exact method wins. HEAD falls back to the
// first GET route when no HEAD route matches (RFC 9110 §9.3.2).
func (r *Router) find(method string, segments []string) (serving, first *route) {
	var fallback *route

	for _, endpoint := range r.routes {
		if !endpoint.match(segments) {
			continue
		}

		if first == nil {
			first = endpoint
		}

		if endpoint.method == method {
			return endpoint, first
		}

		if method == MethodHead && endpoint.method == MethodGet && fallback == nil {
			fallback = endpoint
		}
	}

	return fallback, first
}

// refusal checks the param values of every route that matches the path. It
// returns the first 400 (a NUL or a backslash), or else a 403 (a ".." part),
// or 0. The router answers it before a 405 or an OPTIONS response, with no
// Allow header.
func (r *Router) refusal(segments []string) int {
	status := 0

	for _, endpoint := range r.routes {
		if !endpoint.match(segments) {
			continue
		}

		switch endpoint.checkValues(segments) {
		case StatusBadRequest:
			return StatusBadRequest
		case StatusForbidden:
			status = StatusForbidden
		}
	}

	return status
}

// setAllow sets the Allow header (RFC 9110 §10.2.1) from the routes that
// match the path. GET implies HEAD, and OPTIONS is always served. Call it only
// when a route matched.
func (r *Router) setAllow(res *ResponseWriter, segments []string) {
	var buf [9]string

	methods := buf[:0]

	for _, endpoint := range r.routes {
		if endpoint.match(segments) {
			methods = append(methods, endpoint.method)
		}
	}

	if slices.Contains(methods, MethodGet) && !slices.Contains(methods, MethodHead) {
		methods = append(methods, MethodHead)
	}

	if !slices.Contains(methods, MethodOptions) {
		methods = append(methods, MethodOptions)
	}

	res.Append(HeaderAllow, methods...)
}

// match reports whether the route matches the path segments. It does not
// change the request.
func (route *route) match(segments []string) bool {
	if !strings.HasSuffix(route.pattern, "/...") && len(segments) != len(route.segments) {
		return false
	}

	for i, route_segment := range route.segments {
		if i > len(segments)-1 {
			return false
		}

		if route_segment == "..." {
			return validEscapes(segments[i:])
		}

		if !strings.HasPrefix(route_segment, ":") {
			if segments[i] != route_segment {
				return false
			}

			continue
		}

		value, ok := decodeSegment(segments[i])
		if !ok || !route.matchParam(i, value) {
			return false
		}
	}

	return true
}

// matchParam checks one decoded value against the parameter at index i.
func (route *route) matchParam(i int, value string) bool {
	if route.regexes[i] != nil {
		return route.regexes[i].MatchString(value)
	}

	return value != ""
}

// bind writes the parameters of a fully matched route to the request. It
// returns 0, or the status to send instead of calling the handler: 400 for a
// NUL or a backslash, 403 for a ".." part (RFC 9110 §15.5.1, §15.5.4).
func (route *route) bind(req *Request, segments []string) int {
	if status := route.checkValues(segments); status != 0 {
		return status
	}

	for i, route_segment := range route.segments {
		if route_segment == "..." {
			rest, _ := decodeRest(segments[i:])
			req.Request.SetPathValue("...", rest)

			return 0
		}

		if !strings.HasPrefix(route_segment, ":") {
			continue
		}

		key, _, _ := strings.Cut(route_segment[1:], "|")
		value, _ := decodeSegment(segments[i])
		req.Request.SetPathValue(key, value)
	}

	return 0
}

// checkValues returns the status for the first refused param value, or 0. It
// runs before any value is written, so a refused request keeps no params.
func (route *route) checkValues(segments []string) int {
	for i, route_segment := range route.segments {
		if route_segment == "..." {
			return restStatus(segments[i:])
		}

		if !strings.HasPrefix(route_segment, ":") {
			continue
		}

		value, _ := decodeSegment(segments[i])
		if status := unsafeStatus(value); status != 0 {
			return status
		}
	}

	return 0
}

// restStatus checks each wildcard segment alone. The parts split on "/" of the
// joined value are the parts of the segments, so this needs no join.
func restStatus(segments []string) int {
	for _, segment := range segments {
		value, _ := decodeSegment(segment) // match checked the escapes
		if status := unsafeStatus(value); status != 0 {
			return status
		}
	}

	return 0
}

// unsafeStatus checks a decoded param value, as the send module does: a NUL or
// a backslash gives 400, and a ".." part between "/" gives 403. A decoded "/"
// alone is data, and so is a single "." part. A value without ".." costs one
// scan.
func unsafeStatus(value string) int {
	if strings.ContainsAny(value, "\\\x00") {
		return StatusBadRequest
	}

	if !strings.Contains(value, "..") {
		return 0
	}

	for part := range strings.SplitSeq(value, "/") {
		if part == ".." {
			return StatusForbidden
		}
	}

	return 0
}

// validEscapes reports whether every segment has valid percent escapes. It
// does not allocate for a segment without "%".
func validEscapes(segments []string) bool {
	for _, segment := range segments {
		if _, ok := decodeSegment(segment); !ok {
			return false
		}
	}

	return true
}

// decodeRest joins the segments of a wildcard and decodes each one, once. The
// value can hold "/" and ".". A ".." part gives 403 and a NUL or backslash
// gives 400 (see unsafeStatus). The caller must still confine the value before
// file use (for example with os.Root or filepath.IsLocal). An invalid escape
// gives no match, as for a single param. Over HTTP this cannot run: Request.Path is
// URL.EscapedPath and net/http answers 400 for a bad escape.
func decodeRest(segments []string) (string, bool) {
	rest := strings.Join(segments, "/")

	if !strings.Contains(rest, "%") {
		return rest, true
	}

	decoded := make([]string, len(segments))

	for i, segment := range segments {
		value, ok := decodeSegment(segment)
		if !ok {
			return "", false
		}

		decoded[i] = value
	}

	return strings.Join(decoded, "/"), true
}

// decodeSegment decodes the percent escapes of one path segment (RFC 3986
// §2.1). A decoded %2F stays inside the value, as the path is split first.
func decodeSegment(segment string) (string, bool) {
	if !strings.Contains(segment, "%") {
		return segment, true
	}

	value, err := url.PathUnescape(segment)

	return value, err == nil
}

func (r *Router) wrap(h HandlerFunc) HandlerFunc {
	for _, middleware := range slices.Backward(r.middlewares) {
		h = middleware(h)
	}

	return h
}

func (r *Router) optionsHandler(res *ResponseWriter, req *Request) {
	res.Vary(HeaderAccessControlRequestMethod)
	res.Vary(HeaderAccessControlRequestHeaders)

	if corsAcceptOrigin(req, r.cors) && corsAcceptMethod(req, r.cors) && corsAcceptHeaders(req, r.cors) {
		res.Own(HeaderCacheControl, CacheControl(0, false))
		corsPreflightHeaders(res, req, r.cors)

		res.SendStatus(StatusOK)
		return
	}

	res.SendStatus(StatusForbidden)

	req.newLogger().Warn("the request did not pass cors validation",
		zap.String("path", req.Path()),
		zap.String("host", req.Host()),
		zap.String("origin", req.Origin()),
		zap.Any("headers", req.Request.Header),
	)
}

func (r *Router) methodNotAllowedHandler(segments []string) HandlerFunc {
	return func(res *ResponseWriter, req *Request) {
		r.setAllow(res, segments)
		res.SendStatus(StatusMethodNotAllowed)
	}
}

// statusHandler sends a status that the router chose before the handler ran.
func (r *Router) statusHandler(status int) HandlerFunc {
	return func(res *ResponseWriter, _ *Request) {
		res.SendStatus(status)
	}
}

func (r *Router) notFoundHandler(res *ResponseWriter, req *Request) {
	res.SendStatus(StatusNotFound)
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
			bootPanic("WithCors", "Origin entry "+quoteValue(origin), "it is empty or not a valid field value (RFC 9110 §5.5)",
				"use an origin such as \"https://app.example\", or \"*\"")
		}
	}

	for _, method := range o.Methods {
		if method != "*" && !validFieldName(method) {
			bootPanic("WithCors", "Methods entry "+quoteValue(method), "it is not a token (RFC 9110 §9.1)",
				"use a method such as \"GET\", or \"*\"")
		}
	}

	if o.CacheDuration < 0 {
		bootPanic("WithCors", "CacheDuration "+o.CacheDuration.String(), "Access-Control-Max-Age is delta-seconds, a non-negative integer (Fetch §3.2.3)",
			"use zero for no Max-Age, or a positive duration, for example CacheDuration: time.Hour")
	}

	for _, header := range o.Headers {
		if header != "*" && !validFieldName(header) {
			bootPanic("WithCors", "Headers entry "+quoteValue(header), "it is not a token (RFC 9110 §5.6.2)",
				"use a header name such as \"Content-Type\", or \"*\"")
		}
	}
}
