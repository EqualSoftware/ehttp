package ehttp

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

type Response struct {
	*http.Response
}

// Retrieve the response.Header Cache-Control max-age as time.Duration
func (res *Response) GetCacheControlMaxAge() time.Duration {
	header := res.Header.Get("Cache-Control")
	duration := time.Duration(0)

	if header == "" {
		return duration
	}

	directives := strings.Split(header, ",")

	for _, directive := range directives {
		directive = strings.TrimSpace(directive)

		if strings.HasPrefix(directive, "max-age=") {
			parts := strings.SplitN(directive, "=", 2)

			if len(parts) == 2 {
				age_string := strings.TrimSpace(parts[1])
				age, err := strconv.Atoi(age_string)

				duration = time.Duration(age) * time.Second

				if err == nil {
					break
				}
			}
		}
	}

	return duration
}

// Read the response.Body as JSON. It reads the JSON value into memory.
func (res *Response) JSON(value interface{}) error {
	defer res.Body.Close()

	err := json.NewDecoder(res.Body).Decode(value)

	if err != nil {
		zap.L().Error("failure to parse the json response", zap.Error(err))

		return err
	}

	// Read the rest of a small body so the connection can be reused. A bigger
	// body closes the connection (same limit as net/http maxPostHandlerReadBytes).
	_, _ = io.CopyN(io.Discard, res.Body, 256<<10)

	return nil
}

// Read the response.Body as String. It reads the full body into memory.
func (res *Response) String() (string, error) {
	defer res.Body.Close()

	bytes, err := io.ReadAll(res.Body)

	if err != nil {
		zap.L().Error("failure to parse the json response", zap.Error(err))

		return "", err
	}

	return string(bytes), nil
}

// Read the response.Body as Bytes. It reads the full body into memory.
func (res *Response) Bytes() ([]byte, error) {
	defer res.Body.Close()

	bytes, err := io.ReadAll(res.Body)

	if err != nil {
		zap.L().Error("failure to parse the json response", zap.Error(err))

		return nil, err
	}

	return bytes, nil
}
