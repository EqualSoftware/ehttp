// Package hook lets the middleware package reach two functions of the ehttp
// package that are not public API. ehttp imports this package and sets the
// variables in init, so there is no import cycle and no exported symbol in
// ehttp.
package hook

import "net/http"

// WrapWriter puts a writer between a *ehttp.ResponseWriter (res) and the real
// writer, without a buffer. wrap gets the current writer and returns the new
// one. The returned function puts the old writer back.
var WrapWriter func(res any, wrap func(http.ResponseWriter) http.ResponseWriter) (restore func())

// BootPanic panics at boot (both modes) with the ehttp message format.
var BootPanic func(where, what, why, fix string)

// SetStatus sets the status code that a *ehttp.ResponseWriter reports with
// StatusCode, when a wrapped writer changes the status on the wire.
var SetStatus func(res any, code int)
