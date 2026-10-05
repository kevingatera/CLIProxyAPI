# Redacted request archive

The homelab fork can keep a provider-independent archive of downstream prompts,
requests, tool inputs/results, responses, and recorded upstream attempts. It uses
the shared HTTP logging middleware, including SSE and supported websocket
transcripts. It does not depend on a provider or model allowlist. Management
routes are excluded so credential administration is not archived.

Enable it at startup:

```yaml
request-archive:
  enabled: true
  max-size-gb: 30
  # Optional. Defaults to requests/ under the configured logs directory.
  # directory: /CLIProxyAPI/logs/requests
```

The equivalent v8 path is `observability.logs.request-archive`. Enabling the
archive turns on the executor request-capture hooks even when `request-log` was
false. Enabling/disabling archive mode or changing its directory requires a
restart. The size limit can be reloaded. A zero/omitted limit defaults to 30 GB.
GB uses decimal bytes: 30 GB is 30,000,000,000 bytes. There is no age limit.

Completed logs are pruned oldest first at startup and after each completed
request. Retention applies only to `.log` and `.log.gz` files in the dedicated
archive directory. Active temporary captures and unrelated metadata are not
pruned. Files are owner-only (0600) and the directory is 0700. An in-flight
capture can temporarily exceed the completed-file budget.

Masking runs before file or temporary-file writes. It covers gateway client keys,
configured provider keys, runtime OAuth credentials, sensitive headers/cookies,
credential fields in JSON, URL credentials, recognizable API-key/JWT formats,
and labelled credentials in text. Streaming redaction also checks secrets split
between network chunks and JSON text/tool deltas. The live request and response
remain unchanged. Binary bodies and compressed bodies that cannot be decoded are
recorded with explicit omission markers instead of unsafe opaque bytes.

Prompt text is retained. Arbitrary unlabelled secrets that are neither known to
the gateway nor recognizable as credentials cannot be detected reliably. This
is credential masking, not automatic removal of all personal information.

Archive mode retains upstream details and streaming transcripts in memory until
redaction is complete. Its streaming queues apply backpressure instead of
silently dropping chunks. Large concurrent responses therefore increase memory
use, and slow storage can delay stream completion. No network timeout is added.

On omv-108, the default archive maps to:

```
/srv/ssd-apps/cliproxyapi/logs/requests/
```

Inspect files on the host with SSH. Do not dump real prompt bodies or credential
values into agent output. For validation, send distinctive synthetic markers
through multiple protocols, confirm wire responses are unchanged, then check
only marker presence, file permissions, retention, and absence of exact known
credentials. Tests cover streaming chunk saturation, split credentials,
compressed responses, websocket sections, temporary files, and retention.
