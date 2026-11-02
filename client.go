package ehttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	err           error
}

// defaultTimeout bounds a request when the caller sets no timeout. It matches
// the 30 s dial timeout of http.DefaultTransport.
// https://pkg.go.dev/net/http#DefaultTransport
const defaultTimeout = 30 * time.Second

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
			cfg.err = err

			return
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
	http_client := &http.Client{Timeout: defaultTimeout}

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

	if cfg.err != nil {
		zap.L().Error("invalid source url option", zap.Error(logErr(cfg.err)))

		return nil, cfg.err
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
					zap.String("url", logURL(req_url)),
					zap.Duration("timeout", cfg.timeout),
				),
				zap.Error(logErr(err)),
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
					zap.String("url", logURL(req_url)),
					zap.Duration("timeout", cfg.timeout),
				),
				zap.Error(logErr(err)),
			)

			return nil, err
		}
	}

	ctx := cfg.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	req, err = http.NewRequestWithContext(ctx, cfg.method, req_url.String(), body)

	if err != nil {
		zap.L().Error("failure to create the request",
			zap.Dict("request",
				zap.Bool("context", cfg.ctx != nil),
				zap.String("method", cfg.method),
				zap.String("url", logURL(req_url)),
				zap.Duration("timeout", cfg.timeout),
			),
			zap.Error(logErr(err)),
		)

		return nil, err
	}

	if cfg.jsonPayload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if client.authorization != "" {
		req.Header.Set("Authorization", client.authorization)
	}

	for key, header := range cfg.headers {
		req.Header.Set(key, strings.Join(header, ", "))
	}

	res, err = client.httpClient.Do(req) //nolint:bodyclose // the caller closes the body through Response

	if err != nil {
		zap.L().Error("failure to execute the request",
			zap.Dict("request",
				zap.String("method", req.Method),
				zap.String("url", logURL(req.URL)),
			),
			zap.Error(logErr(err)),
		)

		return nil, err
	}

	zap.L().Debug("request executed",
		zap.Dict("payload",
			zap.String("method", req.Method),
			zap.String("url", logURL(req.URL)),
		),
		zap.Dict("response",
			zap.Int("code", res.StatusCode),
		),
	)

	return &Response{res}, nil
}

// logErr removes the URL from a *url.Error, because the URL can hold secrets.
func logErr(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s: %w", urlErr.Op, urlErr.Err)
	}

	return err
}

// logURL gives the URL for a log line. It drops the credentials, the query
// and the fragment, because they can hold secrets.
func logURL(u *url.URL) string {
	safe := *u
	safe.User = nil
	safe.RawQuery = ""
	safe.ForceQuery = false
	safe.Fragment = ""
	safe.RawFragment = ""

	return safe.String()
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
