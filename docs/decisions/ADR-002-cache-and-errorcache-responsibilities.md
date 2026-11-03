# ADR-002: Cache and ErrorCache responsibilities

- Status: Accepted
- Date: 2026-11-03
- Owners: Equal

## Context
Cache and ErrorCache both set Cache-Control. Error responses need a different cache policy from success responses. Some error statuses are cacheable by default, and some must never be stored. Client-specific statuses must not be shared between clients. A server timeout must not be cached.

## Decision
Cache(d) applies only to responses with a status below 400. ErrorCache sets the policy for error responses by status. The default table is: a default lifetime only for 404, 405, 410, 414 and 501, which RFC 9110 section 15.1 makes heuristically cacheable. Every other 4xx and 5xx status gets no-store, unless ErrorCacheStatus sets a lifetime. Statuses 428, 429, 431 and 511 always get no-store. ErrorCacheStatus on these statuses panics at boot, because RFC 6585 says a cache must not store them. For 401, 403 and 407 with a lifetime, the library always sends private. An existing Cache-Control header from the handler wins. ErrorCache(0) sets no-store. Only one shared cache writer runs for each request. The writer closest to the handler decides for each status group. A Timeout 503 response sets no Cache-Control header.

## Alternatives
- Cache applies to all statuses: Rejected. It stores client-specific errors and error pages without a policy.
- Default lifetime for all error statuses: Rejected. 401, 403, 429 and 503 must not be stored by a shared cache.
- Timeout sets no-store on its 503: Rejected. Cache policy belongs to ErrorCache. RFC 9110 section 15.6.4 says 503 is not heuristically cacheable, so no header is needed.

## Consequences
- Positive: Error responses follow the RFC rules for each status. Client-specific errors are not shared. Developers get a boot error for an invalid status setting.
- Negative: The table is fixed in the library. A developer who wants a different policy must set it by status with ErrorCacheStatus.
- Follow-up: The README documents the table and the shared writer order.

## Evidence
RFC 9110 section 15.1 (heuristically cacheable statuses), section 15.6.4 (503), section 15.5.6 (405). RFC 6585 sections 3 to 6 (428, 429, 431, 511 must not be stored). RFC 9111 section 3.5 for shared caches. The Phase 2e review verified the table against the RFC text.
