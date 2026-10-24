# Redacted request archive

The request archive keeps a provider-independent, credential-masked record of
downstream prompts, requests, tool inputs/results, responses, and recorded
upstream attempts. It is off by default. It uses the shared HTTP logging
middleware, including SSE and supported websocket transcripts. It does not
depend on a provider or model allowlist. Management routes are excluded so
credential administration is not archived.

Enable it at startup:

```yaml
request-archive:
  enabled: true
  max-size-gb: 30
  # Optional. Delete completed files older than this many days. 0 keeps them
  # until the size cap removes them.
  max-age-days: 0
  # Optional. In-memory transcript cap per request. 0 uses 32.
  max-capture-mb: 32
  # Optional. Streaming requests that may buffer transcripts at once. 0 uses 16.
  max-concurrent-captures: 16
  # Optional. Keep URL, headers, status and timing; omit all bodies.
  metadata-only: false
  # Optional. Defaults to requests/ under the configured logs directory.
  # directory: /var/lib/cliproxy/archive
```

The equivalent v8 path is `observability.logs.request-archive`. Enabling the
archive turns on the executor request-capture hooks even when `request-log` was
false. Enabling/disabling archive mode or changing its directory requires a
restart. Size, age, capture and metadata-only settings reload without restart.
GB uses decimal bytes: 30 GB is 30,000,000,000 bytes.

## Retention

Completed logs are removed at startup and after each completed request: first
any file older than `max-age-days`, then the oldest files until the directory
fits `max-size-gb`. Retention applies only to `.log` and `.log.gz` files in the
dedicated archive directory. Active temporary captures and unrelated metadata
are not removed. With no traffic, expired files stay until the next request or
restart. Files are owner-only (0600) and the directory is 0700.

## Memory bounds

Archive mode holds streaming transcripts in memory until redaction is complete.
Two limits bound that memory:

- `max-capture-mb` caps the bytes one request keeps across all sections. Bytes
  past the cap are dropped and the record ends with
  `[ARCHIVE TRUNCATED: per-request capture limit reached]`.
- `max-concurrent-captures` caps streaming requests that buffer transcripts.
  Requests over the limit still get a record with metadata and
  `[ARCHIVE OMITTED: concurrent capture limit reached]`.

Worst-case archive memory is about `max-capture-mb` times
`max-concurrent-captures` (512 MB by default). The proxied traffic is never
blocked or changed by these limits. No network timeout is added.

## Masking

Masking runs before file or temporary-file writes. It covers gateway client keys,
configured provider keys, runtime OAuth credentials, sensitive headers/cookies,
credential fields in JSON, URL credentials, recognizable API-key/JWT formats,
and labelled credentials in text. Streaming redaction also checks secrets split
between network chunks and JSON text/tool deltas. When a capture limit cuts a
transcript inside a known credential, the partial credential is dropped. The
live request and response remain unchanged. Binary bodies and compressed bodies
that cannot be decoded are recorded with explicit omission markers instead of
unsafe opaque bytes.

Prompt text is retained unless `metadata-only` is set. Arbitrary unlabelled
secrets that are neither known to the gateway nor recognizable as credentials
cannot be detected reliably. This is credential masking, not automatic removal
of all personal information.

## Validation

Mount the archive directory on the host (for example
`/var/lib/cliproxy/archive`) and inspect files there. Do not copy real prompt
bodies or credential values into tickets or chat. Send distinctive synthetic
markers through multiple protocols, confirm wire responses are unchanged, then
check only marker presence, file permissions, retention, and absence of exact
known credentials.
