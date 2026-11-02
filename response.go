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

// Read the response.Body as JSON
func (res *Response) JSON(value interface{}) error {
	defer res.Body.Close()

	err := json.NewDecoder(res.Body).Decode(value)

	if err != nil {
		zap.L().Error("failure to parse the json response", zap.Error(err))

		return err
	}

	// Read to EOF so the connection can be reused.
	_, _ = io.Copy(io.Discard, res.Body)

	return nil
}

// Read the response.Body as String
func (res *Response) String() (string, error) {
	defer res.Body.Close()

	bytes, err := io.ReadAll(res.Body)

	if err != nil {
		zap.L().Error("failure to parse the json response", zap.Error(err))

		return "", err
	}

	return string(bytes), nil
}

// Read the response.Body as Bytes
func (res *Response) Bytes() ([]byte, error) {
	defer res.Body.Close()

	bytes, err := io.ReadAll(res.Body)

	if err != nil {
		zap.L().Error("failure to parse the json response", zap.Error(err))

		return nil, err
	}

	return bytes, nil
}
