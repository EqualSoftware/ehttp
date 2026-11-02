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

	"github.com/EqualSoftware/ehttp/internal/redact"
)

type clientOptionFunc func(cfg *Client)

type requestOptionFunc func(cfg *clientRequestConfig)

type Client struct {
	httpClient    *http.Client
	baseUrl       *url.URL
	authorization string
	redact        *redact.Matcher // nil is the default list
	maxBytes      int64           // limit of the Response helpers; 0 is no limit
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

// defaultTimeout is the total limit of a request when the caller sets no
// timeout. It also covers the read of the body (http.Client.Timeout).
// https://pkg.go.dev/net/http#Client
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
				zap.Error(logErr(err)),
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

// WithClientRedactNames adds name patterns to the deny list of the client. It
// never replaces the default list. The syntax is the one of WithRedactNames.
// An invalid pattern panics.
func WithClientRedactNames(patterns ...string) clientOptionFunc {
	if _, err := redact.Default().With(patterns...); err != nil {
		bootPanic("WithClientRedactNames", "an invalid pattern", err.Error(),
			"use a name, or a name with one \"*\" at the start, at the end or at both, for example WithClientRedactNames(\"*pin\")")
	}

	return func(cfg *Client) {
		cfg.redact, _ = cfg.redact.With(patterns...)
	}
}

// WithMaxResponseBytes sets the largest body that Response.JSON, String and
// Bytes read. A value of 0 means no limit, which is the default. A negative
// value panics. If Content-Length is known and above the limit, the helpers
// return ErrResponseTooLarge without a read of the body. Else they stop at one
// byte over the limit. Response.Body stays unlimited, for streaming. The
// limit counts decoded bytes when the Transport decompresses gzip. If the
// caller sets Accept-Encoding by hand, the Transport does not decompress and the
// limit counts the compressed bytes. The limit is opt-in, as in resty (SetResponseBodyLimit) and req
// (SetMaxResponseSize).
func WithMaxResponseBytes(n int64) clientOptionFunc {
	if n < 0 {
		bootPanic("WithMaxResponseBytes", "a negative limit", fmt.Sprintf("the limit is %d", n),
			"use a positive number of bytes, or 0 for no limit, for example WithMaxResponseBytes(10 << 20)")
	}

	return func(cfg *Client) {
		cfg.maxBytes = n
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

// Execute sends the request. A missing method or URL is an error, not a panic.
// The returned error of a failed request is a *url.Error that holds the URL
// path. The userinfo and the sensitive query values are masked (see
// WithClientRedactNames). Only the URL of the error is masked: the text of the
// wrapped error comes from net/http and can quote a value from the server, for
// example an unparsable redirect Location. Warn and Error logs of the client
// show only the scheme, the host and the method.
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
		err = logErr(cfg.err)
		zap.L().Error("invalid source url option", zap.Error(err))

		return nil, err
	}

	if cfg.method == "" {
		return nil, misuse("Execute", "a request without a method",
			"the method is empty or WithSource was not used",
			"pass a method, for example WithSource(\"GET\", \"https://example.com\")")
	}

	if cfg.url == nil {
		return nil, misuse("Execute", "a request without a URL",
			"WithSource was not used",
			"pass a URL, for example WithSource(\"GET\", \"https://example.com\")")
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
					zap.String("target", logTarget(req_url)),
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
					zap.String("target", logTarget(req_url)),
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
				zap.String("target", logTarget(req_url)),
				zap.Duration("timeout", cfg.timeout),
			),
			zap.Error(logErr(err)),
		)

		return nil, maskErr(err, client.redact)
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
				zap.String("target", logTarget(req.URL)),
			),
			zap.Error(logErr(err)),
		)

		return nil, maskErr(err, client.redact)
	}

	zap.L().Debug("request executed",
		zap.Dict("payload",
			zap.String("method", req.Method),
			zap.String("url", client.redact.URL(req.URL)),
		),
		zap.Dict("response",
			zap.Int("code", res.StatusCode),
		),
	)

	return &Response{Response: res, maxBytes: client.maxBytes}, nil
}

// logErr removes the URL from a *url.Error, because the URL can hold secrets.
func logErr(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s: %w", urlErr.Op, urlErr.Err)
	}

	return err
}

// logTarget gives the scheme and the host for a Warn or Error log line. It
// has no path, no query and no credentials, because they can hold secrets.
func logTarget(u *url.URL) string {
	return u.Scheme + "://" + u.Host
}

// maskErr hides the secrets of the URL in a *url.Error. The URL is the one in
// the error, which after a redirect is the URL of the failing hop. The new
// error has the same Op and Err, so errors.Is, errors.As and Timeout keep
// working. If that URL does not parse, it becomes the mask. Other errors do not
// change.
func maskErr(err error, m *redact.Matcher) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}

	masked := redact.Mask

	if u, perr := url.Parse(urlErr.URL); perr == nil {
		masked = m.URL(u)
	}

	return &url.Error{Op: urlErr.Op, URL: masked, Err: urlErr.Err}
}

// misuse logs a developer mistake once at Error level and returns it.
func misuse(where, what, why, fix string) error {
	msg := bootMessage(where, what, why, fix)
	zap.L().Error(msg)

	return errors.New(msg)
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
