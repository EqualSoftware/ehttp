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

	"go.uber.org/zap"
)

var ErrRequestBodyTooLarge = errors.New("request body too large")

type Request struct {
	Request *http.Request
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

func (req *Request) Protocol() string {
	value := req.Request.URL.Scheme

	if h := req.Header(HeaderXForwardedProto); h != "" {
		value = h
	} else if h := req.Header(HeaderXForwardedProtocol); h != "" {
		value = h
	}

	value = strings.TrimSuffix(value, "://")
	value = strings.TrimSuffix(value, ":")

	return strings.ToLower(value)
}

func (req *Request) Host() string {
	if host := req.Header(HeaderXForwardedHost); host != "" {
		return host
	}

	return req.Request.Host
}

func (req *Request) Hostname() string {
	return req.Request.URL.Hostname()
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

func (req *Request) Accepts(value string) bool {
	accept := req.Header(HeaderAccept)

	if accept == "" || strings.Contains(accept, "*/*") {
		return true
	}

	mediatype, _, err := mime.ParseMediaType(accept)

	if err != nil {
		zap.L().Error(fmt.Sprintf("invalid header accept '%s'", accept), zap.Error(err))
		return false
	}

	vmediatype, _, err := mime.ParseMediaType(value)

	if err != nil {
		zap.L().Error(fmt.Sprintf("invalid accepts value '%s'", value), zap.Error(err))
		return false
	}

	mediatype_reg, err := regexp.Compile(mediatype)

	if err != nil {
		zap.L().Error("invalid accepts regex", zap.Error(err))
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
