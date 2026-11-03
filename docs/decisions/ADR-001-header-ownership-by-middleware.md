# ADR-001: Header ownership by middleware

- Status: Accepted
- Date: 2026-11-03
- Owners: Equal

## Context
Middleware such as Cache, Secure, RequestId and CORS sets response headers before the handler runs. A handler can also write the same header. The library needs one rule for the case where both write it. Vary has several members that come from different writers, so it needs a separate rule.

## Decision
The handler can override a header that a middleware set. The last writer wins. In Development mode, the library tracks which writer owns each header. When a handler overrides a middleware header, the library logs a warning with the header name, the route and both values. It does not panic. In Production mode, the library does not track owners, so it adds no cost. Vary is merge only: a handler adds members, and it cannot remove a member or replace the list.

## Alternatives
- Middleware value always wins, and the handler write is ignored: Rejected. A handler cannot correct a value that a middleware set, and the rule hides the conflict.
- Panic on any override in both modes: Rejected. A panic in Production turns a valid handler write into an outage.
- Track owners in Production too: Rejected. It adds cost to every request for a case that is a developer error.

## Consequences
- Positive: The handler keeps full control of a header. Developers see the conflict in Development mode. Production has no tracking cost.
- Negative: In Production, a conflict is silent. A developer who does not use Development mode can miss an override.
- Follow-up: The README recommends WithMode(Development) for development and tests.

## Evidence
RFC 9110 section 16.3 and section 12.5.5 for Vary. Fetch section 3.2.5 for the CORS headers. The rule replaces the earlier rule that the middleware value always wins.
