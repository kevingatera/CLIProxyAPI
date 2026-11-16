# Request archive

The request archive keeps a credential-masked record of proxied traffic:
downstream requests and responses, tool inputs and results, streamed
transcripts, and upstream attempts. It applies to every provider and runs
through the shared request logging middleware. Management routes are excluded.

It is off by default. Credentials are masked before any byte reaches disk,
including temporary files.

## Enable

```yaml
observability:
  logs:
    request-archive:
      enabled: true
      max-size-gb: 30
      # max-age-days: 0              # 0 keeps files until the size cap removes them
      # max-capture-mb: 32           # in-memory transcript cap per request
      # max-concurrent-captures: 16  # streaming requests buffering at once
      # metadata-only: false         # omit all bodies, keep metadata
      # directory: /path/to/archive  # default: requests/ under the logs directory
```

The top-level key `request-archive` is also accepted. Relative `directory`
paths resolve from the config file directory.

Enabling the archive also turns on `request-log`, so upstream attempts are
recorded. Commercial mode turns upstream-attempt capture off.

## Reload and restart

- Size, age, capture, concurrency and metadata-only settings apply on reload.
- Turning a running archive off applies on reload.
- Turning the archive on for a server started without it, or changing
  `directory`, requires a restart.
- The archive writes only to the local directory. It does not forward records
  to CLIProxyAPIHome.

## Retention

Completed `.log` and `.log.gz` files in the archive directory are removed at
startup and after each completed request. Expired files go first, then the
oldest files until the directory fits `max-size-gb`. `max-size-gb` uses
decimal bytes: 30 GB is 30,000,000,000 bytes.

Active temporary captures and unrelated files are never removed. With no
traffic, expired files stay until the next request or restart.

Files are created with mode 0600 and the directory with mode 0700.

## Memory and capture limits

Streaming transcripts stay in memory until the response finishes, then they
are masked and written. Two limits bound that memory:

- `max-capture-mb` caps the bytes one request keeps across all sections. Bytes
  past the cap are dropped, and the record ends with
  `[ARCHIVE TRUNCATED: per-request capture limit reached]`.
- `max-concurrent-captures` caps streaming requests that buffer transcripts.
  Requests over the limit are still recorded with metadata and
  `[ARCHIVE OMITTED: concurrent capture limit reached]`.

Worst-case transcript memory is about `max-capture-mb` times
`max-concurrent-captures`, which is 512 MB at the defaults. Non-streaming
bodies are already buffered by the middleware; the cap bounds what is stored.

Client responses are not shortened by these limits. Chunks are queued, not
dropped, so an archived transcript has no gaps before the cap.

## Masking

Masking runs before every write, including streamed chunks and temporary files.
It covers:

- gateway client keys and configured provider keys
- runtime OAuth credentials
- sensitive headers and cookies
- credential-named JSON fields, including JSON stored inside a string
- URL userinfo
- recognizable API key and JWT formats
- labelled credentials in plain text

Known secrets are matched after JSON escape decoding and across streamed
chunk boundaries. A capture cut inside a known secret drops the partial secret.
Binary bodies are replaced with an omission marker.

Limits:

- Masking is heuristic. Unlabelled secrets that the gateway does not know and
  that do not match a known format are not detected.
- Prompts are kept unless `metadata-only` is set. This is credential masking,
  not removal of personal information.
- Compressed non-streaming responses are decompressed before masking, and that
  decompression has no size limit.

## Validation

Use a throwaway key and distinctive synthetic markers. Send one request per
protocol, then check that:

- the client response is unchanged
- the markers appear in the archive
- the throwaway key does not appear
- files are mode 0600 and the directory is mode 0700
