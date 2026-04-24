# Model Catalog Normalization

Sub2API now separates the public model catalog from provider-specific upstream
model IDs for style-only aliases of the same model.

## Public canonical form

- Public model names use the dotted Claude version style when a dotted and a
  hyphenated form refer to the same model.
- Current examples:
  - `claude-sonnet-4-6` -> `claude-sonnet-4.6`
  - `claude-opus-4-6` -> `claude-opus-4.6`
  - `claude-haiku-4-5` -> `claude-haiku-4.5`
  - `claude-opus-4-6-thinking` -> `claude-opus-4.6-thinking`

## What is normalized

- Only style-only aliases are merged.
- `4.6` and `4-6` can match the same public model.
- Exact same-model overlaps across providers are still merged as before.

## What is not normalized

- Different versions are never merged.
  - `claude-sonnet-4.5` and `claude-sonnet-4.6` stay distinct.
  - `claude-opus-4.6` and `claude-opus-4.7` stay distinct.
- Date-stamped Claude IDs are not folded into short IDs by this layer.
  - `claude-haiku-4-5-20251001` remains distinct from `claude-haiku-4.5`.

## Surface behavior

- Account capability checks accept either style when the account mapping or
  available model list contains the other style.
- `GET /v1/models` emits deduplicated canonical public names.
- `GET /api/v1/admin/accounts/:id/models` also emits canonical public names.

## Upstream routing

- Canonicalization is only for public lookup and catalog output.
- Once an account is selected, Sub2API still sends the provider-specific model
  ID that the upstream expects.
- Example: a request for `claude-sonnet-4.6` can be scheduled onto a Kiro
  account and forwarded upstream as `claude-sonnet-4-6`.
