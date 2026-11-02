package fixtures

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/EqualSoftware/ehttp"
)

// Origins used by the CORS suites.
const (
	OriginA    = "https://app.example"
	OriginB    = "https://admin.example"
	OriginEvil = "https://evil.example"
)

// Start serves srv through a real net/http server.
func Start(t testing.TB, srv *ehttp.Server) *httptest.Server {
	t.Helper()

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	return ts
}

// Client returns a client that keeps raw responses: no redirects, no
// transparent gzip, short overall deadline.
func Client() *http.Client {
	return &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{DisableCompression: true, DisableKeepAlives: true},
	}
}

// Fetch sends one request and returns the response and its whole body.
func Fetch(t testing.TB, method, url string, hdr http.Header) (resp *http.Response, body string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, url, http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	for k, v := range hdr {
		req.Header[k] = v
	}

	resp, err = Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	return resp, string(b)
}

// Raw writes raw bytes to addr and returns everything the server sends
// until it closes the connection or 2s pass.
func Raw(t testing.TB, addr, raw string) string {
	t.Helper()

	conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	if _, err := conn.Write([]byte(raw)); err != nil {
		t.Fatalf("write: %v", err)
	}

	b, _ := io.ReadAll(bufio.NewReader(conn))

	return string(b)
}

// FreePort reserves and releases a loopback port.
func FreePort(t testing.TB) string {
	t.Helper()

	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	return fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
}

// Listening is a server started by Listen.
type Listening struct {
	Port   string
	Cancel context.CancelFunc
	Result chan error
}

// Listen starts srv.Listen on a free port and waits until it accepts.
func Listen(t testing.TB, mk func(port string) *ehttp.Server) *Listening {
	t.Helper()

	port := FreePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	l := &Listening{Port: port, Cancel: cancel, Result: make(chan error, 1)}
	srv := mk(port)

	go func() { l.Result <- srv.Listen(ctx) }()

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		c, err := (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext(t.Context(), "tcp", "127.0.0.1:"+port)
		if err == nil {
			_ = c.Close()
			t.Cleanup(cancel)

			return l
		}

		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	t.Fatal("server never accepted connections")

	return nil
}

// CorsServer builds a server with WithCors. WithCors mutates the global
// DefaultCorsOptions, so the global is restored when the test ends.
func CorsServer(t testing.TB, opts *ehttp.CorsOptions) *ehttp.Server {
	orig := *ehttp.DefaultCorsOptions
	*ehttp.DefaultCorsOptions = orig

	t.Cleanup(func() { *ehttp.DefaultCorsOptions = orig })

	var srv *ehttp.Server
	if opts == nil {
		srv = ehttp.NewServer()
	} else {
		srv = ehttp.NewServer(ehttp.WithCors(*opts))
	}

	srv.GET("/api", func(res *ehttp.ResponseWriter, req *ehttp.Request) { res.String("data") })

	return srv
}
