package ehttp

import (
	"net/http"
	"strings"

	"go.uber.org/zap"
)

// ownedInline is how many owned fields a response keeps without an allocation.
const ownedInline = 3

// ownedField is a field, or a Vary member, that a middleware owns.
type ownedField struct {
	name  string // canonical field name, or the Vary member
	value string // the owned value; empty for a Vary member
	vary  bool
	stamp uint32 // ownedSeq when the entry was last set
}

// Own sets the field name to value and marks it as owned by the middleware:
// the handler cannot change it later. A handler that calls Set, Append or
// Remove on an owned field, or writes it through Header, is making a mistake
// (see fail): Development panics. Production logs a warning and keeps the value
// of the middleware.
//
// Own is for middleware only: the library cannot tell a handler from a
// middleware, so a handler that calls Own breaks the rule it protects. A
// middleware can call Own again to replace the value that another middleware
// owns; only the handler cannot. Own has the checks of Set, and Vary and
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
	res.markOwned(http.CanonicalHeaderKey(name), value, false)
}

// OwnVary is Vary, and it marks each name as an owned member of Vary: the
// handler can add members, but a Vary member of the middleware cannot be
// removed. Like Own, it is for middleware only.
func (res *ResponseWriter) OwnVary(names ...string) {
	if !res.addVary("OwnVary", names) {
		return
	}

	for _, value := range names {
		for value != "" {
			var member string

			member, value, _ = strings.Cut(value, ",")

			if member = strings.TrimSpace(member); member != "" {
				res.markOwned(member, "", true)
			}
		}
	}
}

// markOwned adds the entry, or replaces the entry that has the same name.
func (res *ResponseWriter) markOwned(name, value string, vary bool) {
	if res.owned == nil {
		res.owned = res.ownedBuf[:0]
	}

	res.ownedSeq++

	if o := res.lookupOwned(name, vary); o != nil {
		o.value, o.stamp = value, res.ownedSeq
		return
	}

	res.owned = append(res.owned, ownedField{name: name, value: value, vary: vary, stamp: res.ownedSeq})
}

// findOwned returns the entry of the field name, or nil. A name of Vary has no
// entry: its members do.
func (res *ResponseWriter) findOwned(name string) *ownedField {
	return res.lookupOwned(name, false)
}

func (res *ResponseWriter) lookupOwned(name string, vary bool) *ownedField {
	for i := range res.owned {
		if o := &res.owned[i]; o.vary == vary && equalASCIIFold(o.name, name) {
			return o
		}
	}

	return nil
}

// inheritOwned copies the owned fields of from. An interceptor writer starts
// with the owned fields of the writer it wraps.
func (res *ResponseWriter) inheritOwned(from *ResponseWriter) {
	res.ownedSeq = from.ownedSeq

	if len(from.owned) == 0 {
		return
	}

	res.owned = append(res.ownedBuf[:0], from.owned...)
}

// mergeOwned takes the owned fields of the inner writer of an interceptor.
// base is the owned sequence when the interceptor was made. An entry that this
// writer changed after that point is newer (a middleware after the handler),
// so it stays. Else the inner entry wins (a middleware inside the interceptor).
func (res *ResponseWriter) mergeOwned(from *ResponseWriter, base uint32) {
	for _, o := range from.owned {
		e := res.lookupOwned(o.name, o.vary)

		switch {
		case e == nil:
			res.markOwned(o.name, o.value, o.vary)
		case e.stamp <= base && !o.vary:
			res.markOwned(o.name, o.value, false)
		}
	}
}

// restoreOwned runs before the header goes out. It puts back an owned value
// that a raw write through Header, or a native handler, changed.
func (res *ResponseWriter) restoreOwned() {
	if len(res.owned) == 0 {
		return
	}

	header := res.w.Header()

	for i := range res.owned {
		if o := &res.owned[i]; o.vary {
			res.restoreVary(header, o)
		} else {
			res.restoreField(header, o)
		}
	}
}

func (res *ResponseWriter) restoreField(header http.Header, o *ownedField) {
	got := header[o.name]
	if len(got) == 1 && got[0] == o.value {
		return
	}

	res.failOverride("a raw write through res.Header()", o, joinLines(got))
	header.Set(o.name, o.value)
}

func (res *ResponseWriter) restoreVary(header http.Header, o *ownedField) {
	line := joinLines(header[HeaderVary])
	if varyHasLine(line, o.name) || varyHasLine(line, "*") {
		return
	}

	res.failOverride("a raw write through res.Header()", o, line)
	header.Set(HeaderVary, varyAdd(line, o.name))
}

// failOverride reports that the handler changed an owned field. attempted is
// the value that the handler wanted (empty for a removal).
func (res *ResponseWriter) failOverride(what string, o *ownedField, attempted string) {
	name := o.name
	owned := o.value

	if o.vary {
		name, owned = HeaderVary, o.name
	}

	res.fail(strictError{
		level:  zap.WarnLevel,
		rule:   "handler overrides a middleware header",
		what:   what,
		why:    "header " + name + " is owned by a middleware, with the value " + quoteValue(owned) + "; the handler cannot change it to " + quoteValue(attempted) + " (author decision: header ownership)",
		fix:    "do not set " + name + " in the handler; change the option of the middleware, or remove the middleware from this route",
		header: name,
		extra: []zap.Field{
			zap.String("middleware_value", quoteValue(owned)),
			zap.String("attempted_value", quoteValue(attempted)),
		},
	})
}
