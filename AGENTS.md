# Sub2API Agent Instructions

## Ownership

- `sub2api` owns public model identification, account-pool scheduling, provider-family failover, request-shape compatibility handling, and related cache policy.
- `litellm` is not the public model-identification layer. Do not move routing or model-recognition responsibility there.

## Config Discipline

- Runtime-tuning parameters must live in `backend/internal/config/config.go` and `deploy/config.example.yaml`.
- Do not hardcode new behavioral TTLs, cooldowns, retry budgets, or cache windows in service logic when they may need operator tuning.
- A new runtime parameter is only complete when all of these are updated together:
  - config struct
  - default value
  - validation
  - example config
  - docs when operator-facing behavior changes

## Current Example

- Provider-family compatibility exclusion cache TTL is configured as `gateway.compatibility_exclusion_ttl_seconds`, not hardcoded in the failover path.
