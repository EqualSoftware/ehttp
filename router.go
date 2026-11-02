package ehttp

import (
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"go.uber.org/zap"

	"github.com/EqualSoftware/ehttp/internal/redact"
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
	scope       *route    // the full prefix of a group, for the answers the router makes; nil on the root
	groups      []*Router // the root keeps every group, at any depth
	routed      bool      // a route was registered through this router: Use is closed
	proxies     trustedProxies
	logger      *zap.Logger
	mode        Mode
	redact      *redact.Matcher // nil is the default list
}

type route struct {
	method      string
	pattern     string
	segments    []string
	regexes     []*regexp.Regexp
	handle      HandlerFunc
	middlewares []MiddlewareHandlerFunc
	group       *Router // the innermost group that registered the route; nil for the root
}

// Use adds middlewares that wrap each route registered after the call. It
// panics after a route was registered through this router (for a group: through
// the group or one of its subgroups), as a middleware added later would not
// wrap that route. Call Use before the routes (the rule of go-chi: "all
// middlewares must be defined before routes on a mux").
func (r *Router) Use(m ...MiddlewareHandlerFunc) {
	if r.routed {
		bootPanic("Use", "a call after a route was registered", "middleware registered after a route does not wrap that route",
			"call Use before the routes")
	}

	r.middlewares = append(r.middlewares, m...)
}

// Route makes a group. The middlewares of a group run on its routes and, for
// a path under its prefix, on the 404, 405 and OPTIONS answers of the router
// (see ServeHTTP).
func (r *Router) Route(prefix string, m ...MiddlewareHandlerFunc) *Router {
	g := &Router{
		parent:      r,
		prefix:      prefix,
		middlewares: slices.Clone(m),
	}

	full := r.fullPrefix() + prefix
	segments := splitPath(full)
	g.scope = &route{pattern: full, segments: segments, regexes: compileSegments(full, segments)}

	root := r.root()
	root.groups = append(root.groups, g)

	return g
}

func (r *Router) root() *Router {
	for r.parent != nil {
		r = r.parent
	}

	return r
}

func (r *Router) fullPrefix() string {
	if r.scope == nil {
		return ""
	}

	return r.scope.pattern
}

// splitPath splits a pattern or a path into segments. A trailing slash adds
// no segment.
func splitPath(p string) []string {
	segments := strings.Split(p, "/")

	if segments[len(segments)-1] == "" {
		segments = segments[:len(segments)-1]
	}

	return segments
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

	r.add(r, method, pattern, h, m)
}

// add registers the route on the root. owner is the router that got the
// Handle call. Each router on the way closes its Use.
func (r *Router) add(owner *Router, method, pattern string, h HandlerFunc, m []MiddlewareHandlerFunc) {
	r.routed = true

	if r.parent != nil {
		all := make([]MiddlewareHandlerFunc, 0, len(r.middlewares)+len(m))
		all = append(all, r.middlewares...)
		all = append(all, m...)
		r.parent.add(owner, method, r.prefix+pattern, h, all)

		return
	}

	if owner == r {
		owner = nil
	}

	for _, middleware := range slices.Backward(m) {
		h = middleware(h)
	}

	for _, middleware := range slices.Backward(r.middlewares) {
		h = middleware(h)
	}

	segments := splitPath(r.prefix + pattern)

	mustNotHaveDotSegment(strings.ToUpper(method), r.prefix+pattern, segments)
	r.mustBeNew(strings.ToUpper(method), r.prefix+pattern, segments)

	r.routes = append(r.routes, &route{
		method:      strings.ToUpper(method),
		pattern:     r.prefix + pattern,
		segments:    segments,
		regexes:     compileSegments(pattern, segments),
		handle:      h,
		middlewares: m,
		group:       owner,
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

// ServeHTTP serves a request. The router answers itself when no route serves
// it: 404, 405 and the answer to OPTIONS. These answers run the middlewares
// of the root, and before them the middlewares of one group (the outer group
// first), so a middleware such as logging or CORS sees them:
//   - 404: the innermost group whose prefix matches the path.
//   - 405 and OPTIONS: the group that owns every route that matches the path.
//     If the routes belong to several groups or to the root, only the root
//     middlewares run, as the router cannot pick one group.
//
// The refusals of an unsafe path value (400, 403) run the root middlewares
// only, so a group middleware never sees a path that was refused.
//
// OPTIONS without a CORS preflight gets 200 with Allow and no body (RFC 9110
// §9.3.7), and 404 for an unknown path. A preflight is answered by the CORS
// middleware (see Cors).
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
		redact:   router.redact,
	}
	req.res = res

	method := req.Method()
	path := req.Path()
	url_segments := splitPath(path)

	if router.mode == Development && method == MethodOptions && isPreflight(req) {
		defer router.warnPreflight(res, req)
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

	if !matched {
		router.wrapIn(router.groupUnder(url_segments), router.notFoundHandler).ServeHTTP(res, req)
		return
	}

	group := router.groupOf(url_segments)

	if method == MethodOptions {
		router.setAllow(res, url_segments)
		router.wrapIn(group, router.optionsHandler).ServeHTTP(res, req)

		return
	}

	router.wrapIn(group, router.methodNotAllowedHandler(url_segments)).ServeHTTP(res, req)
}

// warnPreflight tells the developer that a middleware refused a preflight
// with 401 or 403 before the CORS middleware ran: the browser then sees a
// refusal with no CORS headers.
func (router *Router) warnPreflight(res *ResponseWriter, req *Request) {
	if req.corsRan.Load() || (res.code != StatusUnauthorized && res.code != StatusForbidden) {
		return
	}

	req.newLogger().Warn("a middleware refused a CORS preflight before the CORS middleware ran",
		zap.String("path", req.Path()),
		zap.Int("status", res.code),
		zap.String("fix", "register the CORS middleware before the authentication middleware"),
	)
}

// groupUnder returns the innermost group whose prefix matches the path, or
// nil. Among groups with the same number of segments the last one made wins,
// which is the deeper one.
func (r *Router) groupUnder(segments []string) *Router {
	var best *Router

	for _, g := range r.groups {
		if !g.scope.under(segments) {
			continue
		}

		if best == nil || len(g.scope.segments) >= len(best.scope.segments) {
			best = g
		}
	}

	return best
}

// groupOf returns the group that owns every route that matches the path. It
// returns nil when the routes belong to the root or to several groups.
func (r *Router) groupOf(segments []string) *Router {
	var group *Router

	seen := false

	for _, endpoint := range r.routes {
		if !endpoint.match(segments) {
			continue
		}

		if seen && endpoint.group != group {
			return nil
		}

		group, seen = endpoint.group, true
	}

	return group
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

		if !route.matchSegment(i, segments[i]) {
			return false
		}
	}

	return true
}

// matchSegment reports whether the path segment fits the segment i of the
// route: the same text, or a parameter that accepts the decoded value.
func (route *route) matchSegment(i int, segment string) bool {
	if !strings.HasPrefix(route.segments[i], ":") {
		return segment == route.segments[i]
	}

	value, ok := decodeSegment(segment)

	return ok && route.matchParam(i, value)
}

// under reports whether the path is the pattern of the route or is below it.
// The router uses it for the prefix of a group.
func (route *route) under(segments []string) bool {
	if len(segments) < len(route.segments) {
		return false
	}

	for i := range route.segments {
		if !route.matchSegment(i, segments[i]) {
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

// wrapIn wraps h with the middlewares of the group g and of its parents, the
// outer group first, and then with the root middlewares. A nil group adds
// nothing.
func (r *Router) wrapIn(g *Router, h HandlerFunc) HandlerFunc {
	for ; g != nil && g != r; g = g.parent {
		for _, middleware := range slices.Backward(g.middlewares) {
			h = middleware(h)
		}
	}

	return r.wrap(h)
}

// optionsHandler answers OPTIONS that is not a CORS preflight: 200 with the
// Allow header that the router set, and no body (RFC 9110 §9.3.7).
func (r *Router) optionsHandler(res *ResponseWriter, _ *Request) {
	res.Set(HeaderContentLength, "0")
	res.SendStatus(StatusOK)
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
