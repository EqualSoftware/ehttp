package ehttp

import (
	"html/template"
	"net/http"
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

	r.routes = append(r.routes, &route{
		method:      strings.ToUpper(method),
		pattern:     r.prefix + pattern,
		segments:    segments,
		regexes:     compileSegments(pattern, segments),
		handle:      h,
		middlewares: m,
	})
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
			panic("ehttp: invalid route regex " + strconv.Quote(expr) + " in pattern " + strconv.Quote(pattern) + ": " + err.Error())
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
	res := &ResponseWriter{
		w:        w,
		request:  r,
		template: router.template,
	}
	req := &Request{
		Request: r,
	}

	corsUpdateHeaders(res, req, router.cors)

	method := req.Method()
	url := req.Path()
	url_segments := strings.Split(url, "/")

	if url_segments[len(url_segments)-1] == "" {
		url_segments = url_segments[:len(url_segments)-1]
	}

	for _, endpoint := range router.routes {
		if match := endpoint.match(req, url_segments); !match {
			continue
		}

		if method == endpoint.method || method == MethodHead && endpoint.method == MethodGet {
			req.Request.Pattern = endpoint.pattern
			endpoint.handle(res, req)
			return
		}

		if method != MethodOptions {
			var selected_route *route
			methods := []string{}

			for _, route := range router.routes {
				if match := route.match(req, url_segments); match {
					methods = append(methods, route.method)

					if selected_route == nil && method == route.method {
						selected_route = route
					}
				}
			}

			if slices.Contains(methods, MethodGet) && !slices.Contains(methods, MethodHead) {
				methods = append(methods, MethodHead)
			}

			if !slices.Contains(methods, MethodOptions) {
				methods = append(methods, MethodOptions)
			}

			router.wrap(router.methodNotAllowedHandler(methods)).ServeHTTP(res, req)

			return
		}
	}

	if method == MethodOptions {
		router.wrap(router.optionsHandler).ServeHTTP(res, req)
		return
	}

	router.wrap(router.notFoundHandler).ServeHTTP(res, req)
}

func (route *route) match(req *Request, segments []string) bool {
	if !strings.HasSuffix(route.pattern, "/...") && len(segments) != len(route.segments) {
		return false
	}

	for i, route_segment := range route.segments {
		if i > len(segments)-1 {
			return false
		}

		if route_segment == "..." {
			req.Request.SetPathValue("...", strings.Join(segments[i:], "/"))
			return true
		}

		if strings.HasPrefix(route_segment, ":") {
			key, _, contains_regex := strings.Cut(strings.TrimPrefix(route_segment, ":"), "|")

			if contains_regex {
				if route.regexes[i].MatchString(segments[i]) {
					req.Request.SetPathValue(key, segments[i])
					continue
				}
			}

			if !contains_regex && segments[i] != "" {
				req.Request.SetPathValue(key, segments[i])
				continue
			}

			return false
		}

		if segments[i] != route_segment {
			return false
		}
	}

	return true
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
		SetCacheHeader(res, 0)
		corsPreflightHeaders(res, req, r.cors)

		res.SendStatus(StatusOK)
		return
	}

	res.SendStatus(StatusForbidden)

	zap.L().Warn("the request did not pass cors validation",
		zap.String("method", req.Method()),
		zap.String("path", req.Path()),
		zap.String("host", req.Host()),
		zap.String("origin", req.Origin()),
		zap.Any("headers", req.Request.Header),
	)
}

func (r *Router) methodNotAllowedHandler(route_methods []string) HandlerFunc {
	return func(res *ResponseWriter, req *Request) {
		res.Append(HeaderAllow, strings.Join(route_methods, ", "))
		res.SendStatus(StatusMethodNotAllowed)
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
		res.Vary(HeaderOrigin)
	}

	origin := req.Origin()

	if origin == "" || !corsAcceptOrigin(req, options) {
		return
	}

	if wildcard {
		origin = "*"
	}

	res.Append(HeaderAccessControlAllowOrigin, origin)
	res.Append(HeaderAccessControlAllowCredentials, strconv.FormatBool(options.Credentials))
}

// corsPreflightHeaders sets the headers that a browser reads only from the
// preflight response (Fetch §3.2.3). With credentials, a literal * has no
// meaning, so the request values are echoed.
func corsPreflightHeaders(res *ResponseWriter, req *Request, options *CorsOptions) {
	methods := corsJoin(options.Methods)
	headers := corsJoin(options.Headers)

	if options.Credentials && slices.Contains(options.Methods, "*") {
		methods = req.Header(HeaderAccessControlRequestMethod)
	}

	if options.Credentials && slices.Contains(options.Headers, "*") {
		headers = req.Header(HeaderAccessControlRequestHeaders)
	}

	if methods != "" {
		res.Append(HeaderAccessControlAllowMethods, methods)
	}

	if headers != "" {
		res.Append(HeaderAccessControlAllowHeaders, headers)
	}

	if d := options.CacheDuration; d > 0 {
		res.Append(HeaderAccessControlMaxAge, strconv.Itoa(int(d.Seconds())))
	}
}

// corsJoin does not allocate for a list of one item.
func corsJoin(list []string) string {
	if len(list) == 1 {
		return list[0]
	}

	return strings.Join(list, ", ")
}
