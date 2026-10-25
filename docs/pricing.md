# Model price estimates

Codex reports token counts without a dollar cost. Aimebu estimates its turn
cost from [`internal/pricing/models.json`](../internal/pricing/models.json).
Each model entry has US dollars per million **input**, **output**,
**cache_read**, and **cache_write** tokens, plus `as_of` (the date checked)
and `source` (a direct pricing page). The shipped entries use published OpenAI
API Standard rates for short context. They are a planning estimate for a Codex
session, not a subscription charge or a provider invoice. Long context,
service tiers, regional processing, and future rate changes can differ.
An unlisted model or a turn without both input and output counts has no cost
estimate. Daily cost caps use this estimate when no harness cost is available.

## Updating the price list

An AI updating prices should:

1. Check the model's **official provider pricing page** and use its direct
   URL as `source`. Read the Standard short-context rates for all four token
   categories. Do not infer a missing rate from a different model.
2. Add or revise that model's entry in `internal/pricing/models.json`, with
   numeric dollars per million tokens and today's UTC date in `as_of`.
   Preserve other entries unless the provider says they changed.
3. Run `go test ./internal/pricing ./internal/adapter/codex ./internal/store`
   and `node scripts/test-message-usage.cjs`, then the required full gate.
   Check that an estimated Codex turn is labelled `estimated` in both its
   message usage and aggregate coverage, while a harness-reported cost remains
   `known`.

OpenAI publishes the current rates in its
[model catalog](https://developers.openai.com/api/docs/models) and
[pricing table](https://developers.openai.com/api/docs/pricing).
