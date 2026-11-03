# ADR-004: Strong ETag for static files

- Status: Accepted
- Date: 2026-11-03
- Owners: Equal

## Context
SendFile sent a weak ETag built from the file name, the modification time as text and the size. The text of the time depends on the time zone of the server, so the same file gave different tags on different servers. A weak tag cannot be used for If-Match or If-Range, which need a strong comparison.

## Decision
SendFile sends a strong ETag in the form "<hex UnixNano>-<hex size>", as nginx does. The value uses nanoseconds of the modification time. It has no W/ prefix and no file name. It does not depend on the time zone. The ETag enables If-Match and If-Range for files. HEAD returns the same ETag as GET. A rewrite of the file changes the ETag.

## Alternatives
- Weak ETag with the file name and the time text: Rejected. It depends on the time zone and it cannot be used for If-Match.
- Hash of the file content: Rejected. It reads the whole file on each request, which costs I/O and CPU.
- Last-Modified only, no ETag: Rejected. Last-Modified has one second resolution, and it cannot support If-Match.

## Consequences
- Positive: The same file gives the same tag on every server. Clients can use If-Match and If-Range on files. There is no hash cost.
- Negative: The tag is only as strong as the modification time. A rewrite with the same size in the same file system tick keeps the tag. Tools that restore the modification time, such as rsync -t, touch -d and container image layers, also keep the tag. nginx has the same limit.
- Follow-up: The SendFile documentation states the limit.

## Evidence
RFC 9110 section 8.8.1 (ETag), section 8.8.3 (opaque validators), section 8.8.3.1 (generation of a validator with sub-second resolution), section 13.1.1 (If-Match), section 13.1.5 (If-Range). The test sendfile-etag_test.go checks the tag across time zones and HEAD equal to GET.
