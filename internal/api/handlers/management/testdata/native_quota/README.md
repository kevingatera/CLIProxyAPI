# Native quota fixtures

OpenCode Go, Kimi Coding, MiniMax inactive-plan, and CommandCode responses are sanitized captures from the exact account endpoints called by `fetchNativeQuotaReport`. They returned HTTP 200. CommandCode includes both the authentication response (`org: null`, so billing uses no `orgId`) and billing response.

Identity, credential, and unrelated string values use fixed placeholders. JSON structure, quota numbers, quota enums, and reset times are preserved. No production files or services were changed.

MiniMax returned provider code 2062 (no active token plan). `minimax-token-plan-remains.json` remains synthetic because the captured account has no active plan. The OpenCode exhausted-window and malformed-response unit cases also remain synthetic. No supported provider lacked a production credential.

Run `GOTOOLCHAIN=auto go test ./internal/api/handlers/management` to check the captured responses through the HTTP adapters. These captures are point-in-time test inputs, not current balances.
