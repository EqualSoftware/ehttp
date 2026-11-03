# ADR-003: Strict configuration at boot

- Status: Accepted
- Date: 2026-11-03
- Owners: Equal

## Context
Some configuration errors were silent or appeared only at request time. Examples are an invalid port, a route with a duplicate pattern, an unknown method and an invalid CORS setting. Client input is a different case: a client can send any value, and a panic on client input is a denial of service.

## Decision
Invalid configuration panics at boot. The checks cover the port, a duplicate route by shape (a parameter name does not change the shape), a route method outside GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS, CONNECT and TRACE, Timeout with a duration of zero or less, a negative cache duration, templates and CORS configuration. Client input never causes a panic. Invalid client input gets a 4xx response, except where the RFC says a server must ignore the header. All boot errors use one message format.

## Alternatives
- Return an error from each option: Rejected. The developer can ignore the error, and the server starts with a wrong configuration.
- Log a warning and use a default: Rejected. The default hides the mistake, which is the silent failure that the rule prevents.
- Allow WebDAV and other methods: Rejected for this rule. The list is closed. Methods outside the list stop working, and the author accepted this change.

## Consequences
- Positive: A configuration mistake stops the process at start. The error does not reach a user.
- Negative: A valid method that the list does not contain stops working. A developer must use a different route.
- Follow-up: The README lists the method list and the boot rules.

## Evidence
RFC 9110 section 9.1 (method names) and section 9.3 (method definitions). RFC 9110 section 13.1.3 and section 14.2 for the client headers that a server must ignore. The boot checks are tested in the unit tests of the server and the middleware.
