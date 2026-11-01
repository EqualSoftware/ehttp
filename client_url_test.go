package ehttp

import (
	"net/url"
	"testing"
)

func TestJoinBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		base   string
		target string
		want   string
	}{
		{"mixed case dot segments", "http://h/api/v1", "/x/%2E%2e/y", "http://h/api/v1/y"},
		{"dot and encoded dot", "http://h/api/v1", "/.%2e/y", "http://h/api/v1/y"},
		{"encoded dot and dot", "http://h/api/v1", "/%2e./y", "http://h/api/v1/y"},
		{"dots clamp at the prefix", "http://h/api/v1", "/../../admin", "http://h/api/v1/admin"},
		{"encoded slash after dots is data", "http://h/api/v1", "/a/..%2Fb", "http://h/api/v1/a/..%2Fb"},
		{"encoded slash around dots is data", "http://h/api/v1", "/a%2F..%2Fb", "http://h/api/v1/a%2F..%2Fb"},
		{"double encoded dots are data", "http://h/api/v1", "/a/%252e%252e/b", "http://h/api/v1/a/%252e%252e/b"},
		{"trailing dot segment", "http://h/api/v1", "/a/b/..", "http://h/api/v1/a/"},
		{"trailing slash kept", "http://h/api/v1", "/a/b/", "http://h/api/v1/a/b/"},
		{"empty target", "http://h/api/v1", "", "http://h/api/v1"},
		{"query only target", "http://h/api/v1", "?q=1", "http://h/api/v1?q=1"},
		{"base with trailing slash", "http://h/api/v1/", "/x", "http://h/api/v1/x"},
		{"base query merges", "http://h/api/v1?a=1&a=2", "/x?a=3&b=4", "http://h/api/v1/x?a=1&a=2&a=3&b=4"},
		{"base with encoded slash", "http://h/api%2Fv1", "/x", "http://h/api%2Fv1/x"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			base, err := url.Parse(tc.base)
			if err != nil {
				t.Fatal(err)
			}

			target, err := url.Parse(tc.target)
			if err != nil {
				t.Fatal(err)
			}

			got := joinBaseURL(base, target)

			if got.String() != tc.want {
				t.Errorf("url = %q, want %q", got.String(), tc.want)
			}

			if got.RawPath != "" && got.EscapedPath() != got.RawPath {
				t.Errorf("EscapedPath() = %q, RawPath = %q", got.EscapedPath(), got.RawPath)
			}
		})
	}
}
