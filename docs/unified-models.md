# Unified model routing

`routing.unified-models` is opt-in. It presents a canonical catalog while keeping
existing provider-specific request IDs registered. It applies to streaming,
non-streaming and token-count requests. Manual credential pins retain their
existing semantics.

```yaml
routing:
  unified-models:
    enabled: true
    bare-names: false
    expose-legacy: false
    models:
      - id: deepseek-v4.1-flash
        routes:
          - provider: opencode-go
            source: opencode-go/deepseek-v4.1-flash
            model: deepseek-v4.1-flash
            plan: included
          - provider: command-code
            source: commandcode/deepseek/deepseek-v4.1-flash
            model: deepseek/deepseek-v4.1-flash
            plan: metered
```

`source` must already be registered for that credential. `model` is its verified
upstream wire ID. Do not map different versions, high-speed variants or preview
models to one canonical ID. Provider model catalogs demonstrate advertised
identity; a live generation request is still necessary to prove access.

The default public ID is `cliproxy/deepseek-v4.1-flash`. `bare-names: true`
changes discovery to `deepseek-v4.1-flash`. This changes canonical IDs, so update
clients using them. Existing provider-specific IDs remain usable. Enable
`expose-legacy` to also list those IDs. The management provider page exposes all
three switches. Its dedicated PATCH endpoint edits only existing boolean lines,
creates a private timestamped backup, and preserves the rest of the document.

## Selection

1. Require an enabled credential, an executor, the registered source model and no
   active model/credential cooldown.
2. Exclude fresh provider-reported exhausted windows and exhausted credit balances.
3. Prefer an included plan unless a known binding window has less than 5% remaining.
   Missing quota does not force a subscription request onto a metered relay.
4. Within that plan preference, a higher route `priority` takes precedence.
   Use it to prefer a native subscription over another included relay; disabled,
   exhausted and cooling-down credentials remain excluded. With equal priorities,
   prefer fresh known capacity over unknown capacity.
   Unknown is not unlimited.
5. Compare binding-window headroom adjusted for observed allowance depletion and
   time until reset. Scores are grouped into 5% bands to reduce trivial switches.
6. Use stable credential ordering for ties. Retry a different eligible route for
   capacity, rate-limit, server and transport failures before streaming begins.
   An invalid request is terminal; recognized insufficient-credit errors can fail
   over even when a provider returns HTTP 400.

Native account reports refresh in the background every five minutes. Selection
never performs network calls. Reports expire after ten minutes, and a passed
reset makes that bucket unknown until refreshed. Reports are account-wide,
not per-model token grants. Model token burn is a bounded ten-minute observation;
it is shown separately because tokens cannot be converted to provider allowance
without a documented conversion. Allowance depletion compares consecutive
reports of the same window and reset, including usage outside this proxy.

`GET /v8/management/routing/unified-models` explains each route, its eligibility,
plan, quota freshness, binding remaining fraction, depletion and observed model
token rate. `GET /v8/management/routing/traces` proves the actual attempts.
These management endpoints require authentication. They never return API keys.

This feature does not change the routing policy of legacy model IDs. In
particular, existing Claude OAuth-first ordering remains in effect.
# Private allowance snapshots

Routes can set `subscription-only: true` with `auth-kind: oauth`. Such a route
requires the provider's `is_subs_active` boolean to be true. Missing or inactive
subscription status excludes the credential even while its model registration
is cached. This prevents a subscription route from silently using ordinary
API billing. Meta Muse publishes this status when its OAuth device credential
is exchanged for the Muse Code inference key.

`routing.allowances` can configure `collector-key`, `service-url` and
`service-key`. Use distinct random tokens of at least 32 characters. The
collector key grants only `GET /capacity/v1/collect`; it grants no management or
inference access. Its reports contain opaque credential indexes, provider
provenance, account or model scope and observation expiry. Shared protocol
credentials are grouped into one allowance account.

The independent model-capacity service reads that collector and exposes an
authenticated `/v1/allowances` snapshot. CLIProxy reads it in the background,
preserving the original observation time. If the service is unreachable or
expired, native quota refresh remains available. No network calls occur during
request selection. Missing provider adapters remain unknown.

Keep these private keys in runtime configuration only. They are excluded from
the general JSON configuration response. The public provider-plan-catalog is a
separate static program with no account connector or private runtime secrets.
