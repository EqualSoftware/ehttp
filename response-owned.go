package ehttp

import (
	"net/http"
	"slices"

	"go.uber.org/zap"
)

// ownedField is a field that a middleware owns. Only Development tracks it.
type ownedField struct {
	name  string // canonical field name
	value string // the value that the middleware set
	stamp uint32 // seq when the entry was last set
}

// ownedDev is the owner list of Development. Production keeps no list: the
// pointer of the writer stays nil, so the list costs nothing there.
type ownedDev struct {
	list []ownedField
	seq  uint32
}

// Own sets the field name to value. In Development it also records that a
// middleware set the field. A handler can still change the field (the last
// writer wins), but Development then logs a warning: the handler overrides a
// middleware. Production records nothing and logs nothing.
//
// Own is for middleware only. A middleware can call Own again to replace the
// value that another middleware set. Own has the checks of Set, and Vary and
// Set-Cookie have their own methods (OwnVary, Cookie). Pass a canonical name
// (for example "X-Xss-Protection") to avoid an allocation for each request.
func (res *ResponseWriter) Own(name, value string) {
	if !res.checkName("Own", name) || !res.checkValue("Own", name, value) || !res.checkSpecial("Own", name) {
		return
	}

	if res.finished() || !res.unlessSent("Own", name) {
		return
	}

	res.w.Header().Set(name, value)

	if res.mode == Development {
		res.markOwned(http.CanonicalHeaderKey(name), value)
	}
}

// OwnVary is Vary, and it also keeps each name as a member that the handler
// cannot remove: Vary only grows (RFC 9110 §12.5.5). Both modes keep the
// members, and the header re-gets them before it goes out. Like Own, it is
// for middleware only.
func (res *ResponseWriter) OwnVary(names ...string) {
	if !res.addVary("OwnVary", names) {
		return
	}

	for _, value := range names {
		res.ownedVary = varyAdd(res.ownedVary, value)
	}
}

// markOwned adds the entry, or replaces the entry that has the same name.
func (res *ResponseWriter) markOwned(name, value string) {
	if res.dev == nil {
		res.dev = &ownedDev{}
	}

	res.dev.seq++

	if i := res.findOwned(name); i >= 0 {
		res.dev.list[i].value, res.dev.list[i].stamp = value, res.dev.seq

		return
	}

	res.dev.list = append(res.dev.list, ownedField{name: name, value: value, stamp: res.dev.seq})
}

// findOwned returns the index of the entry of the field name, or -1.
func (res *ResponseWriter) findOwned(name string) int {
	if res.dev == nil {
		return -1
	}

	for i := range res.dev.list {
		if equalASCIIFold(res.dev.list[i].name, name) {
			return i
		}
	}

	return -1
}

// dropOwned removes the entry i: the handler took the field.
func (res *ResponseWriter) dropOwned(i int) {
	res.dev.list = slices.Delete(res.dev.list, i, i+1)
}

// inheritOwned copies the owner state of from. An interceptor writer starts
// with the state of the writer it wraps.
func (res *ResponseWriter) inheritOwned(from *ResponseWriter) {
	res.ownedVary = from.ownedVary

	if from.dev == nil {
		return
	}

	res.dev = &ownedDev{list: append([]ownedField(nil), from.dev.list...), seq: from.dev.seq}
}

// mergeOwned takes the owner state of the inner writer of an interceptor.
// base is the sequence when the interceptor was made. An entry that this
// writer changed after that point is newer (a middleware after the handler),
// so it stays. Else the inner entry wins (a middleware inside the interceptor).
func (res *ResponseWriter) mergeOwned(from *ResponseWriter, base uint32) {
	res.ownedVary = varyAdd(res.ownedVary, from.ownedVary)

	if from.dev == nil {
		return
	}

	for _, o := range from.dev.list {
		i := res.findOwned(o.name)

		switch {
		case i < 0:
			res.markOwned(o.name, o.value)
		case res.dev.list[i].stamp <= base:
			res.markOwned(o.name, o.value)
		}
	}

	res.dropTaken(from, base)
}

// dropTaken drops the old entries that the inner writer dropped: the handler
// took those fields, and the warning is already out.
func (res *ResponseWriter) dropTaken(from *ResponseWriter, base uint32) {
	if res.dev == nil {
		return
	}

	for i := len(res.dev.list) - 1; i >= 0; i-- {
		o := res.dev.list[i]
		if o.stamp > base {
			continue
		}

		if from.dev == nil || from.findOwned(o.name) < 0 {
			res.dropOwned(i)
		}
	}
}

// checkOwned runs before the header goes out. In Development it warns about an
// owned field that a raw write through Header, or a native handler, changed.
// The value stays: the last writer wins. Then it makes sure that the Vary
// members of the middleware are still there.
func (res *ResponseWriter) checkOwned() {
	if res.ownedVary != "" {
		header := res.w.Header()
		line := joinLines(header[HeaderVary])

		if merged := varyAdd(line, res.ownedVary); merged != line {
			header.Set(HeaderVary, merged)
		}
	}

	if res.dev == nil {
		return
	}

	header := res.w.Header()

	for i := len(res.dev.list) - 1; i >= 0; i-- {
		o := res.dev.list[i]
		if got := header[o.name]; len(got) != 1 || got[0] != o.value {
			res.warnOverride("a raw write through res.Header()", o, joinLines(got))
			res.dropOwned(i)
		}
	}
}

// warnOverride logs that the handler changed a field that a middleware set.
// handler is the value that the handler wants (empty for a removal).
func (res *ResponseWriter) warnOverride(what string, o ownedField, handler string) {
	where := res.where()
	middleware, mine := res.quoteField(o.name, o.value), res.quoteField(o.name, handler)

	msg := "ehttp: " + what + " on " + where + ": header " + o.name + " has the value " + middleware +
		" from a middleware and the handler changed it to " + mine + "; the handler value is sent. " +
		"Fix: do not set " + o.name + " in the handler, or change the option of the middleware."

	ce := res.logger().Check(zap.WarnLevel, msg)
	if ce == nil {
		return
	}

	fields := []zap.Field{
		zap.String("rule", "handler overrides a middleware header"),
		zap.String("header", fieldName(o.name)),
		zap.String("middleware_value", middleware),
		zap.String("handler_value", mine),
	}

	if res.request != nil {
		fields = append(fields, zap.String("method", res.request.Method), zap.String("route", res.route()))
	}

	ce.Write(fields...)
}

// overrideOwned runs when the handler changes the owned field i with the call
// what. It warns, and the field is the handler field from then on.
func (res *ResponseWriter) overrideOwned(i int, what, value string) {
	o := res.dev.list[i]
	if value == o.value {
		return
	}

	res.warnOverride(what, o, value)
	res.dropOwned(i)
}

// ownedSeq is the sequence of the owner list; 0 when there is no list.
func (res *ResponseWriter) ownedSeq() uint32 {
	if res.dev == nil {
		return 0
	}

	return res.dev.seq
}
