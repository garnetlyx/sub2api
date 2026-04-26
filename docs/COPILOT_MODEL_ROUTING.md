# Copilot Model Routing

## Model list authority

Sub2API routes to a GitHub Copilot account only when the requested model appears
in that account's `available_models` list. This list is fetched from the Copilot
API at import time and **refreshed on every token refresh cycle** — no manual
model allowlists or name-based heuristics are used.

- If Copilot adds a new model, it is automatically eligible for routing once the
  account's token refreshes (typically within the configured refresh window).
- If a model is not in `available_models`, Copilot accounts are excluded at
  pre-selection — they are never attempted, and no compatibility exclusion entry
  is created.
- Accounts with an empty `available_models` (e.g. freshly imported accounts
  before the first token refresh) are treated as unsupported for all models and
  are excluded unconditionally.

## Token refresh and model list sync

`CopilotTokenRefresher.Refresh()` calls `RefreshByRefreshToken`, which re-fetches
the current model list from the Copilot API alongside new credentials. The fresh
list is:

1. Persisted to the DB `extra.available_models` column via `UpdateExtra`.
2. Patched into the in-memory account struct so that the subsequent Redis cache
   sync (`postRefreshActions → schedulerCache.SetAccount`) writes the current
   list without an additional DB read.

Failure to update the extra field is logged (`copilot_token_refresh.available_models_update_failed`)
but does not fail the credential refresh.

## Diagnostics logging

When account selection fails (`*.account_select_failed`), the log now includes a
breakdown of why no account was available:

| Field | Description |
|---|---|
| `total_candidates` | Accounts in the scheduler snapshot for this group |
| `unschedulable` | Accounts excluded by runtime state checks |
| `rate_limited` | Subset of unschedulable: `RateLimitResetAt` in future |
| `overloaded` | Subset of unschedulable: `OverloadUntil` in future |
| `temp_unschedulable` | Subset of unschedulable: `TempUnschedulableUntil` set |
| `model_filtered` | Schedulable accounts excluded by model support check |
| `copilot_no_model_list` | Copilot accounts excluded because `available_models` is empty |
| `copilot_model_not_in_list` | Copilot accounts excluded because model is absent from list |
| `copilot_compat_excluded` | Whether the copilot platform is in the failover-loop exclusion set |

Example: a gpt-5.5 request where all OAI OAuth accounts are rate-limited and
Copilot does not list the model would show:

```
total_candidates=6 unschedulable=4 rate_limited=4 model_filtered=2
copilot_model_not_in_list=2 copilot_compat_excluded=false
```

This distinguishes "Copilot was tried and failed" (compat exclusion) from
"Copilot was never a candidate" (model filter), which was previously invisible
in the logs.
