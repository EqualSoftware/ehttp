package ehttp

import (
	"strconv"
	"unicode/utf8"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Every strict error has one message format (project rule: DX message):
//
//	ehttp: <what> on|in <where>: <why>. Fix: <fix>.
//
// what is the call that failed, where is "METHOD route" at request time or the
// option name at boot, why is the rule with its basis (RFC section or "author
// decision") and fix is the exact call to use.

// maxQuoted is the most bytes of a user value that a message or a log keeps.
const maxQuoted = 64

// quoteValue quotes v with %q rules and keeps at most maxQuoted bytes of it.
func quoteValue(v string) string {
	if len(v) <= maxQuoted {
		return strconv.Quote(v)
	}

	n := maxQuoted
	for n > 0 && !utf8.RuneStart(v[n]) {
		n-- // cut on a rune boundary
	}

	return strconv.Quote(v[:n]) + "..."
}

// fieldName writes a header name for a log field: as is if it is a short
// token, else quoted.
func fieldName(name string) string {
	if len(name) <= maxQuoted && validFieldName(name) {
		return name
	}

	return quoteValue(name)
}

// bootPanic panics at boot (both modes) with the strict message format.
func bootPanic(where, what, why, fix string) {
	panic(bootMessage(where, what, why, fix))
}

func bootMessage(where, what, why, fix string) string {
	return "ehttp: " + what + " in " + where + ": " + why + ". Fix: " + fix + "."
}

// strictError describes one developer mistake at request time.
type strictError struct {
	level  zapcore.Level
	rule   string // short name of the rule, for the log field "rule"
	what   string // the call that failed
	why    string // the rule and its basis
	fix    string // the exact call to use
	header string // field name, if the error is about one
	value  string // user value, if the error is about one (logged truncated)
	extra  []zap.Field
}

func (e strictError) message(where string) string {
	return "ehttp: " + e.what + " on " + where + ": " + e.why + ". Fix: " + e.fix + "."
}

// where returns "METHOD route" of the response. The route is the registered
// pattern, or the path if the request has no route.
func (res *ResponseWriter) where() string {
	if res.request == nil {
		return "an unknown request"
	}

	return res.request.Method + " " + res.route()
}

func (res *ResponseWriter) route() string {
	if res.request.Pattern != "" {
		return res.request.Pattern
	}

	return res.request.URL.Path
}

// fail handles a developer mistake at request time. In Development it panics
// with the message. In Production it logs the same message with the fields
// rule, fix, header, value and route, and returns: the caller then applies the
// safe fallback. The logger is the uncached request logger.
func (res *ResponseWriter) fail(e strictError) {
	msg := e.message(res.where())

	if res.mode == Development {
		panic(msg)
	}

	ce := res.logger().Check(e.level, msg)
	if ce == nil {
		return
	}

	fields := make([]zap.Field, 0, 5+len(e.extra))
	fields = append(fields, zap.String("rule", e.rule), zap.String("fix", e.fix))

	if e.header != "" {
		fields = append(fields, zap.String("header", fieldName(e.header)))
	}

	if e.value != "" {
		fields = append(fields, zap.String("value", quoteValue(e.value)))
	}

	// The request logger has the route field when the request has a pattern.
	if res.request != nil && res.request.Pattern == "" {
		fields = append(fields, zap.String("route", res.route()))
	}

	ce.Write(append(fields, e.extra...)...)
}

// call writes a method call for a message: res.Set("X-A", ...).
func call(method, name string) string {
	switch method {
	case "Remove":
		return "res.Remove(" + quoteValue(name) + ")"
	case "Set", "Append", "Own":
		return "res." + method + "(" + quoteValue(name) + ", ...)"
	}

	return "res." + method + "(...)"
}
