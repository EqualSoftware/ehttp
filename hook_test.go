package ehttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"github.com/EqualSoftware/ehttp/internal/hook"
)

type aTag struct{ http.ResponseWriter }

func (w aTag) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// The hook puts a writer under the response, and restore puts the old one back.
func TestHookWrapWriter(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	res := &ResponseWriter{
		w:       rec,
		request: httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody),
	}

	var seen http.ResponseWriter

	restore := hook.WrapWriter(res, func(w http.ResponseWriter) http.ResponseWriter {
		seen = w
		return aTag{w}
	})

	ehttptest.True(t, "wrap gets the current writer", seen == http.ResponseWriter(rec), "wrap got %T", seen)
	ehttptest.True(t, "the new writer is in place", res.Unwrap() != http.ResponseWriter(rec), "the writer was not replaced")

	_, _ = res.Write([]byte("x"))

	restore()

	ehttptest.True(t, "restore puts the old writer back", res.Unwrap() == http.ResponseWriter(rec), "the writer was not restored")
}

func TestHookBootPanic(t *testing.T) {
	t.Parallel()

	msg := aPanics(func() { hook.BootPanic("Opt", "a thing", "a reason", "a fix") })

	ehttptest.Eq(t, "the boot format of strict.go", msg, bootMessage("Opt", "a thing", "a reason", "a fix"))
}

// A wrong value for the hook is a mistake of the developer: it panics with the
// boot message, not with a nil dereference.
func TestHookRejectsWrongWriter(t *testing.T) {
	t.Parallel()

	wrap := func(w http.ResponseWriter) http.ResponseWriter { return w }

	tests := map[string]any{
		"nil":                 nil,
		"typed nil":           (*ResponseWriter)(nil),
		"interceptor":         &ResponseWriterInterceptor{},
		"not a writer at all": "res",
	}

	for name, v := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for hookName, do := range map[string]func(){
				"hook.WrapWriter": func() { hook.WrapWriter(v, wrap) },
				"hook.SetStatus":  func() { hook.SetStatus(v, 304) },
			} {
				msg := aPanics(do)

				ehttptest.True(t, "boot message format", strings.HasPrefix(msg, "ehttp: value of type ") && strings.Contains(msg, " in "+hookName+": "), "message %q", msg)
				ehttptest.True(t, "DX message: fix part", strings.Contains(msg, ". Fix: "), "message %q has no Fix part", msg)
			}
		})
	}
}
