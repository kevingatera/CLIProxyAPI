# Request archive

The request archive keeps a credential-masked record of proxied traffic:
downstream requests and responses, tool inputs and results, streamed
transcripts, and upstream attempts. It applies to every provider and runs
through the shared request logging middleware. Management routes are excluded.

It is off by default. Masking runs before any byte of archive content is
written to disk, including temporary files.

## Enable

```yaml
observability:
  logs:
    request-archive:
      enabled: true
      max-size-gb: 30
      # max-age-days: 0              # 0 keeps files until the size cap removes them
      # max-capture-mb: 32           # per-request cap on buffered transcript bytes
      # max-concurrent-captures: 16  # requests that may buffer bodies at once
      # metadata-only: false         # omit all bodies, keep metadata
      # directory: /path/to/archive  # must be empty or an existing archive directory
```

The top-level key `request-archive` is also accepted. Relative `directory`
paths resolve from the config file directory.

Enabling the archive also turns on `request-log`, so upstream attempts are
recorded. Commercial mode turns upstream-attempt capture off.

## Directory

The archive creates a marker file in its directory. A directory that is not
empty and has no marker is refused at startup, and nothing is written there.
Use a directory that only the archive writes to. Retention deletes completed
`.log` and `.log.gz` files in that directory, so any other log file placed
there can be deleted.

## Reload and restart

- Size, age, capture, concurrency and metadata-only settings apply on reload.
- Turning a running archive off applies on reload. Its in-flight streams are
  discarded, not written.
- Turning the archive on for a server started without it does not start the
  archive. Request logging stays off until a restart, and the server logs a
  warning. The legacy logger never receives unmasked traffic from this change.
- Changing `directory` requires a restart.
- The archive writes only to the local directory. It does not forward records
  to CLIProxyAPIHome.

## Retention

Completed `.log` and `.log.gz` files in the archive directory are removed at
startup and after each completed request. Expired files go first, then the
oldest files until the directory fits `max-size-gb`. `max-size-gb` uses
decimal bytes: 30 GB is 30,000,000,000 bytes.

Temporary capture files and the marker file are never removed by retention.
With no traffic, expired files stay until the next request or restart.

Files are created with mode 0600 and the directory with mode 0700.

## Memory and capture limits

Each archived request has two in-memory buffers, and each is capped at
`max-capture-mb`: upstream bodies buffered by the executors, and the client
transcript held until the stream closes for masking. Bytes past a cap are
dropped, and the record ends with a marker:

- `[ARCHIVE TRUNCATED: per-request capture limit reached]` when the budget runs out.
- `[ARCHIVE OMITTED: concurrent capture limit reached]` when the request was
  over `max-concurrent-captures`. Its bodies are omitted, its metadata is kept.
- `[ARCHIVE OMITTED: metadata-only mode]` in metadata-only mode.

`max-concurrent-captures` counts requests that hold a capture budget. A request
holds one from the start until its record is finished, so it bounds in-flight
archived requests, not only streams.

Worst-case buffered memory is about `max-capture-mb` times two times
`max-concurrent-captures`, which is 1 GB at the defaults. The middleware
already buffers non-streaming response bodies, so that memory is separate from
this bound. Client responses are not shortened by these limits.

Compressed non-streaming responses are decompressed for masking only up to the
capture budget. Metadata-only archives never decompress a body.

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
chunk boundaries. At every cut, the archive trims the tail of the kept text
when it could be the start of a known secret (any length, down to one byte)
or of a recognizable credential prefix followed by an open token. The partial
credential is dropped. Binary bodies are replaced with an omission marker.

Limits:

- Masking is heuristic. Unlabelled secrets that the gateway does not know and
  that do not match a known format are not detected.
- A cut can leave a structured value partly written. Only the trimming above
  applies to it, so a partial JSON field can remain.
- Prompts are kept unless `metadata-only` is set. This is credential masking,
  not removal of personal information.

## Validation

Use a throwaway key and distinctive synthetic markers. Send one request per
protocol, then check that:

- the client response is unchanged
- the markers appear in the archive
- the throwaway key does not appear
- files are mode 0600 and the directory is mode 0700
