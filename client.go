package ehttp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"
)

type clientOptionFunc func(cfg *Client)

type requestOptionFunc func(cfg *clientRequestConfig)

type Client struct {
	httpClient    *http.Client
	baseUrl       *url.URL
	authorization string
}

type clientRequestConfig struct {
	ctx           context.Context
	method        string
	url           *url.URL
	timeout       time.Duration
	headers       http.Header
	jsonPayload   interface{}
	stringPayload *string
}

func WithTimeout(value time.Duration) clientOptionFunc {
	return func(cfg *Client) {
		cfg.httpClient.Timeout = value
	}
}

func WithBaseUrl(value string) clientOptionFunc {
	return func(cfg *Client) {
		base, err := url.Parse(value)

		if err != nil {
			zap.L().Panic("invalid fetch base url option",
				zap.String("url", value),
				zap.Error(err),
			)
		}

		cfg.baseUrl = base
	}
}

func WithAuthorization(value string) clientOptionFunc {
	return func(cfg *Client) {
		cfg.authorization = value
	}
}

func WithTransport(value *http.Transport) clientOptionFunc {
	return func(cfg *Client) {
		cfg.httpClient.Transport = value
	}
}

func WithContext(value context.Context) requestOptionFunc {
	return func(cfg *clientRequestConfig) {
		cfg.ctx = value
	}
}

func WithSource(m, u string) requestOptionFunc {
	return func(cfg *clientRequestConfig) {
		cfg.method = m

		if cfg.method == "" {
			zap.L().Panic("invalid source method option",
				zap.String("method", m),
			)
		}

		parsed_url, err := url.Parse(u)

		if err != nil {
			zap.L().Panic("invalid source url option",
				zap.String("url", u),
				zap.Error(err),
			)
		}

		cfg.url = parsed_url
	}
}

func WithHeader(key, value string) requestOptionFunc {
	return func(cfg *clientRequestConfig) {
		cfg.headers.Set(key, value)
	}
}

func WithJsonPayload(value interface{}) requestOptionFunc {
	return func(cfg *clientRequestConfig) {
		cfg.jsonPayload = value
	}
}

func WithStringPayload(value string) requestOptionFunc {
	return func(cfg *clientRequestConfig) {
		cfg.stringPayload = &value
	}
}

func NewClient(options ...clientOptionFunc) *Client {
	http_client := &http.Client{}

	c := &Client{
		httpClient: http_client,
	}

	for _, apply := range options {
		apply(c)
	}

	return c
}

func (client *Client) Execute(options ...requestOptionFunc) (*Response, error) {
	var err error
	var req *http.Request
	var res *http.Response
	var req_url *url.URL

	cfg := &clientRequestConfig{
		timeout: 0,
		headers: http.Header{},
	}

	for _, apply := range options {
		apply(cfg)
	}

	if cfg.method == "" {
		zap.L().Panic("you are trying to execute a request without define a method.")
	}

	if cfg.url == nil {
		zap.L().Panic("you are trying to execute a request without define a url.")
	}

	if client.baseUrl != nil {
		req_url = joinBaseURL(client.baseUrl, cfg.url)
	} else {
		req_url = cfg.url
	}

	body := &bytes.Buffer{}

	if cfg.jsonPayload != nil {
		err = json.NewEncoder(body).Encode(&cfg.jsonPayload)

		if err != nil {
			zap.L().Error("failure to create the payload",
				zap.Dict("request",
					zap.Bool("context", cfg.ctx != nil),
					zap.String("method", cfg.method),
					zap.String("url", req_url.String()),
					zap.Duration("timeout", cfg.timeout),
				),
				zap.Error(err),
			)

			return nil, err
		}
	} else if cfg.stringPayload != nil {
		_, err := body.WriteString(*cfg.stringPayload)

		if err != nil {
			zap.L().Error("failure to create the payload",
				zap.Dict("request",
					zap.Bool("context", cfg.ctx != nil),
					zap.String("method", cfg.method),
					zap.String("url", req_url.String()),
					zap.Duration("timeout", cfg.timeout),
				),
				zap.Error(err),
			)

			return nil, err
		}
	}

	if cfg.ctx == nil {
		req, err = http.NewRequest(cfg.method, req_url.String(), body) //nolint:noctx // no context is set: the caller did not ask for one
	} else {
		req, err = http.NewRequestWithContext(cfg.ctx, cfg.method, req_url.String(), body)
	}

	if err != nil {
		zap.L().Error("failure to create the request",
			zap.Dict("request",
				zap.Bool("context", cfg.ctx != nil),
				zap.String("method", cfg.method),
				zap.String("url", req_url.String()),
				zap.Duration("timeout", cfg.timeout),
			),
			zap.Error(err),
		)

		return nil, err
	}

	if client.authorization != "" {
		req.Header.Set("Authorization", client.authorization)
	}

	for key, header := range cfg.headers {
		req.Header.Set(key, strings.Join(header, ", "))
	}

	res, err = client.httpClient.Do(req) //nolint:bodyclose // the caller closes the body through Response

	result := &Response{res}

	if err != nil {
		zap.L().Error("failure to execute the request",
			zap.Dict("request",
				zap.String("method", req.Method),
				zap.String("url", req.URL.String()),
			),
			zap.Error(err),
		)

		return result, err
	}

	zap.L().Debug("request executed",
		zap.Dict("payload",
			zap.String("method", req.Method),
			zap.String("url", req.URL.String()),
		),
		zap.Dict("response",
			zap.Int("code", res.StatusCode),
		),
	)

	return result, nil
}

// joinBaseURL puts the path of target under the path of base. The path stays
// escaped, so %2F is data and not a separator. Dot segments are resolved
// inside target and cannot leave the base prefix (RFC 3986 §5.2.4): a path
// that tries to leave it is clamped to the prefix. The query keeps every value
// of the base and of target.
func joinBaseURL(base, target *url.URL) *url.URL {
	escaped := base.EscapedPath()
	if target.EscapedPath() != "" {
		escaped = strings.TrimRight(escaped, "/") + cleanEscapedPath(target.EscapedPath())
	}

	path, err := url.PathUnescape(escaped)
	if err != nil {
		return base
	}

	joined := *base
	joined.Path = path
	joined.RawPath = escaped

	query := base.Query()
	for key, values := range target.Query() {
		for _, value := range values {
			query.Add(key, value)
		}
	}

	joined.RawQuery = query.Encode()

	return &joined
}

// cleanEscapedPath removes the dot segments of an escaped path and gives it a
// leading slash. A ".." at the root is dropped. A trailing slash is kept.
func cleanEscapedPath(p string) string {
	segments := strings.Split(p, "/")
	out := make([]string, 0, len(segments))

	for i, segment := range segments {
		last := i == len(segments)-1

		switch strings.ToLower(segment) {
		case "", ".", "%2e":
			if last {
				out = append(out, "")
			}
		case "..", ".%2e", "%2e.", "%2e%2e":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}

			if last {
				out = append(out, "")
			}
		default:
			out = append(out, segment)
		}
	}

	return "/" + strings.Join(out, "/")
}
