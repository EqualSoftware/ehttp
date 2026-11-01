// C6 - golden snapshots of end-to-end responses.
//
// IMPORTANT: a golden file is a SNAPSHOT of the CURRENT behaviour, bugs
// included. It is not an oracle of correct behaviour. When a fix changes a
// response, the diff of testdata/golden/*.txt shows exactly what changed;
// review it, then regenerate with `go test -run TestGolden -update ./test/golden`
// (or `make golden-update`).
//
// The server is the real examples/server/app. Deliberate differences from
// running the example:
//   - LastModified uses a fixed time instead of time.Now();
//   - templates and pudim.png are copied to t.TempDir() and pudim.png gets a
//     fixed mtime (os.Chtimes), so Last-Modified and the file ETag are stable;
//   - Listen is replaced by httptest.NewServer(server).
package golden_test

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/examples/server/app"
)

func init() {
	// Another test file may already define -update.
	if flag.Lookup("update") == nil {
		flag.Bool("update", false, "rewrite golden files")
	}
}

func goldenUpdating() bool {
	f := flag.Lookup("update")
	return f != nil && f.Value.String() == "true"
}

var goldenFixedTime = time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)

// goldenCopy copies the example's templates and static files into dir.
func goldenCopy(t *testing.T, dir string) {
	t.Helper()

	src := filepath.Join("..", "..", "examples", "server")

	for _, rel := range []string{"templates/index.html", "static/pudim.png"} {
		b, err := os.ReadFile(filepath.Join(src, rel))
		if err != nil {
			t.Fatal(err)
		}

		dst := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(dst, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	png := filepath.Join(dir, "static", "pudim.png")
	if err := os.Chtimes(png, goldenFixedTime, goldenFixedTime); err != nil {
		t.Fatal(err)
	}
}

// goldenServer serves the example app. See the file comment.
func goldenServer(t *testing.T) *httptest.Server {
	t.Helper()

	// WithCors mutates the global DefaultCorsOptions (S3): restore it so other
	// tests of this package are not affected.
	saved := *ehttp.DefaultCorsOptions
	t.Cleanup(func() { *ehttp.DefaultCorsOptions = saved })

	dir := t.TempDir()
	goldenCopy(t, dir)

	ts := httptest.NewServer(app.New(dir, goldenFixedTime))
	t.Cleanup(ts.Close)

	return ts
}

type goldenScenario struct {
	name    string
	method  string
	path    string
	headers map[string]string
	body    string
	// before may derive headers from an earlier response (e.g. the ETag).
	before func(t *testing.T, base string) map[string]string
}

// goldenRender flattens a response to a stable text form: status, sorted
// headers, body length and sha256. Date is dropped; X-Request-Id is masked
// when the request did not send one.
func goldenRender(resp *http.Response, body []byte, sentRequestID bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "status: %d\n", resp.StatusCode)

	names := make([]string, 0, len(resp.Header))
	for k := range resp.Header {
		names = append(names, k)
	}
	sort.Strings(names)

	for _, k := range names {
		v := strings.Join(resp.Header[k], " | ")
		switch {
		case k == "Date":
			v = "<volatile>"
		case k == "X-Request-Id" && !sentRequestID:
			v = "<uuid>"
		}
		fmt.Fprintf(&b, "header %s: %s\n", k, v)
	}

	sum := sha256.Sum256(body)
	fmt.Fprintf(&b, "body-len: %d\nbody-sha256: %s\n", len(body), hex.EncodeToString(sum[:]))

	return b.String()
}

func goldenDo(t *testing.T, base string, sc goldenScenario) string {
	t.Helper()

	hdr := map[string]string{}
	for k, v := range sc.headers {
		hdr[k] = v
	}
	if sc.before != nil {
		for k, v := range sc.before(t, base) {
			hdr[k] = v
		}
	}

	req, err := http.NewRequestWithContext(t.Context(), sc.method, base+sc.path, strings.NewReader(sc.body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{DisableCompression: true},
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	_, sent := hdr["X-Request-Id"]

	return goldenRender(resp, data, sent)
}

// goldenETag fetches GET / and returns its ETag as If-None-Match.
func goldenETag(t *testing.T, base string) map[string]string {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, resp.Body)

	return map[string]string{"If-None-Match": resp.Header.Get("Etag")}
}

func TestGolden(t *testing.T) { //nolint:paralleltest // WithCors mutates a global (S3)
	ts := goldenServer(t)

	htmlAccept := map[string]string{"Accept": "text/html"}
	origin := map[string]string{"Origin": "http://localhost:3000"}

	scenarios := []goldenScenario{
		{name: "get_root_html", method: "GET", path: "/", headers: htmlAccept},
		{name: "get_root_json", method: "GET", path: "/", headers: map[string]string{"Accept": "application/json"}},
		{name: "get_root_plain_default", method: "GET", path: "/"},
		{name: "head_root", method: "HEAD", path: "/", headers: htmlAccept},
		{name: "cors_simple_allowed_origin", method: "GET", path: "/healthcheck", headers: origin},
		{name: "cors_simple_foreign_origin", method: "GET", path: "/healthcheck", headers: map[string]string{"Origin": "http://evil.example"}},
		{name: "cors_preflight_allowed", method: "OPTIONS", path: "/body", headers: map[string]string{
			"Origin": "http://localhost:3000", "Access-Control-Request-Method": "POST",
		}},
		{name: "cors_preflight_allowed_with_sec_fetch_site", method: "OPTIONS", path: "/body", headers: map[string]string{
			"Origin": "http://localhost:3000", "Access-Control-Request-Method": "POST", "Sec-Fetch-Site": "cross-site",
		}},
		{name: "cors_preflight_foreign", method: "OPTIONS", path: "/body", headers: map[string]string{
			"Origin": "http://evil.example", "Access-Control-Request-Method": "POST", "Sec-Fetch-Site": "cross-site",
		}},
		{name: "conditional_etag_304", method: "GET", path: "/", headers: htmlAccept, before: goldenETag},
		{name: "conditional_if_modified_since_root", method: "GET", path: "/", headers: map[string]string{
			"If-Modified-Since": goldenFixedTime.Format(http.TimeFormat),
		}},
		{name: "file_full", method: "GET", path: "/file"},
		{name: "file_head", method: "HEAD", path: "/file"},
		{name: "file_range_0_9", method: "GET", path: "/file", headers: map[string]string{"Range": "bytes=0-9"}},
		{name: "file_range_0_0_snapshot_of_C2", method: "GET", path: "/file", headers: map[string]string{"Range": "bytes=0-0"}},
		{name: "file_range_suffix_snapshot_of_C2", method: "GET", path: "/file", headers: map[string]string{"Range": "bytes=-5"}},
		{name: "file_range_unsatisfiable", method: "GET", path: "/file", headers: map[string]string{"Range": "bytes=99999999-"}},
		{name: "file_if_modified_since", method: "GET", path: "/file", headers: map[string]string{
			"If-Modified-Since": goldenFixedTime.Format(http.TimeFormat),
		}},
		{name: "not_found", method: "GET", path: "/nope"},
		{name: "method_not_allowed", method: "POST", path: "/healthcheck"},
		{name: "body_created", method: "POST", path: "/body", body: "hello"},
		{name: "body_over_limit_declared", method: "POST", path: "/body", body: strings.Repeat("a", 5000)},
		{name: "param_route_encoded", method: "GET", path: "/api/v1/hello/John%20Doe"},
		{name: "native_handler", method: "GET", path: "/native"},
		{name: "request_id_echo", method: "GET", path: "/healthcheck", headers: map[string]string{"X-Request-Id": "fixed-id-1"}},
	}

	for _, sc := range scenarios { //nolint:paralleltest // shares one server
		t.Run(sc.name, func(t *testing.T) {
			got := goldenDo(t, ts.URL, sc)
			path := filepath.Join("testdata", "golden", sc.name+".txt")

			if goldenUpdating() {
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("[C6] missing golden %s (run with -update): %v", path, err)
			}
			if string(want) != got {
				t.Errorf("[C6] %s differs from golden.\n--- want\n%s--- got\n%s", sc.name, want, got)
			}
		})
	}
}
