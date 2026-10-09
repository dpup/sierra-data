# Scoping: LunaRoute as a compatibility option for every AI feature

Status: proposed (2026-10-09). Nothing here is implemented. This document
inventories every model call the service makes, assesses whether each can be
routed through LunaRoute's OpenAI-compatible gateway unchanged, lists the
changes needed to make the provider a configuration choice, and recommends a
model. Findings marked **verified** were checked against the live gateway or
its documentation during scoping; those marked **unverified** need the probe
in §6 before any code ships.

## 1. Goal

Make the AI provider a **configuration option**, not a code path: the same
three features keep running against OpenAI by default, and an operator can
point them at `https://gw.lunaroute.com/v1` with a LunaRoute key by setting
two environment variables. "Replace" here means "able to replace", with
OpenAI retained as the default until the bake-off in §8 says otherwise.

## 2. Inventory: every AI call the codebase makes

All three surfaces use the same SDK, `github.com/sashabaranov/go-openai`
v1.41.1, and the same endpoint family. There is no other model use anywhere
(no embeddings, no vision, no tool calling).

| Surface | Code | Endpoint | Request shape | Cadence / volume | Failure posture |
|---|---|---|---|---|---|
| Road incident + road alert enhancement | `internal/lib/alerts/enhancer.go` (prompt in `openai.go`), called from `internal/services/roads.go` and `incidents.go` | `POST /v1/chat/completions` | system + user message; `response_format: json_schema` (`strict: true`); `max_completion_tokens: 3000`; `reasoning_effort: low` only when the model name starts with `gpt-5`/`o1`/`o3`/`o4`; no temperature | Cache-miss calls only, capped at 5 per incidents refresh and shared through a 24 h content-hash cache. Order of 100 calls/day. | Additive: failure keeps the raw text and heuristic severity. Log only. |
| NWS weather-alert summary | `internal/ingest/enhance_nws.go`, called from `scheduler.maybeEnhance` | `POST /v1/chat/completions` | Same pattern; `max_completion_tokens: 1500`; one-field schema | Only on alert content change, `grid.enhancement.budgetPerTick: 5`. A handful per day. | Additive: raw alert served; log only. |
| Burn line: transcription | `internal/clients/burnline/reader.go` `transcribe` | `POST /v1/audio/transcriptions` | multipart: `model: whisper-1`, `file` (mp3, ~90 s), `prompt`, `temperature: 0.1`, `language: en` | Once a day per enabled line, from CI (`.github/workflows/burn-line.yml`), not the server. One line today. | Hard error: no push, facet ages to UNKNOWN after 36 h. |
| Burn line: extraction | `internal/clients/burnline/reader.go` `extract` | `POST /v1/chat/completions` | user message only; `temperature: 0.1`; `response_format: json_schema` (`strict: true`); model from `-model` flag, default `gpt-4o` | Same as above | Same as above |
| Health check | `alertEnhancer.HealthCheck` | `POST /v1/chat/completions`, 16 tokens | **Never called** by any caller | n/a |

Things that reference the provider without calling it:

- `Enhancement.model` on stored events is stamped from `openai.model`
  (`internal/ingest/road_incident.go`, `scheduler.enhancerModel`). It is a free
  string, so `deepseek-4.1-flash` or `glm-5.3` is an honest value with no
  schema change. The proto comment already anticipates non-OpenAI names.
- The 24 h enhancement cache key (`internal/lib/alerts/content_hash.go`)
  includes `promptVersion` but **not the model**, so a provider switch does not
  invalidate it. The cache is in-memory and a deploy restarts the process, so
  this only matters for a live config reload (which prefab does not do).
- Config: `openai.{apiKey, model, timeout, maxRetries}` in `prefab.yaml`,
  overridable as `PF__OPENAI__API_KEY` etc. **`maxRetries` is dead config** (not
  read anywhere), and go-openai performs no retries of its own.
- The burn-line CLI reads a bare `OPENAI_API_KEY` from the environment (CI
  secret), not the prefab config.
- The live prompt check `TestNWSEnhancerLive` keys on `PF__OPENAI__API_KEY`
  and `PF__OPENAI__MODEL`.

## 3. What LunaRoute is, and the facts that matter here

LunaRoute (lunaroute.com) sells **flat-rate concurrency, not tokens**: Pro is
$99/month for 2 priority concurrent requests, Max $199.99 for 4, Apex $299
for 6. Every model is available on every plan; the plan changes only your
concurrency. Prompts and responses are zero-retention. Three API dialects are
offered on one gateway: OpenAI Chat Completions, OpenAI Responses, and
Anthropic Messages, plus audio transcription, embeddings, rerank, images and a
"System One" decision endpoint.

Verified during scoping (live gateway, 2026-10-09):

- `gw.lunaroute.com` is reachable from this environment. `/v1/models`,
  `/v1/chat/completions` and `/v1/audio/transcriptions` all answer `401` with
  `MISSING_LUNAROUTE_HEADER` without a key, naming the three accepted auth
  forms: `LUNAROUTE-API-KEY`, `Authorization: Bearer`, `x-api-key`. The key
  must start with `lr_`; any other bearer value is forwarded upstream as a
  bring-your-own-key credential rather than treated as LunaRoute auth.
- go-openai sends `Authorization: Bearer <key>`, which the docs accept.
  `ClientConfig.BaseURL` is the override hook; our own tests already use it
  (`internal/clients/burnline/reader_test.go` points the real client at an
  `httptest` server). **No new dependency is needed.**

Verified from the documentation (docs.lunaroute.com):

- **Concurrency model.** A request beyond the priority allowance overflows
  (lower fair-share priority, same price) or **parks for up to 60 s**; a full
  queue or expired park returns `429 CONCURRENT_REQUEST_LIMIT_EXCEEDED` with
  `Retry-After: 60`. A `-background` model id parks for up to 240 s at lower
  priority. The docs say: set client timeouts "comfortably above 60 seconds".
- **Audio transcription** is documented on `POST /v1/audio/transcriptions`
  with `whisper-large-v3`, and accepts exactly the fields we send (`file`,
  `model`, `prompt`, `temperature` in [0,1], `language`, `response_format`
  defaulting to `json` → `{"text": ...}`). Limits: 25 MiB upload, 3600 s. Our
  90 s MP3 is far inside both. Audio is metered but **not charged** at launch.
  The page says both "ships switched off" and "live on every auth surface";
  `GET /v1/audio/transcriptions/models` settles it for a given key.
- **Error envelope** is `{"error": {"code", "message"}}`, which go-openai's
  `APIError` decodes (`Code` is `any`, `Message` a string).
- **Nothing in the documentation mentions `response_format`, `json_schema`,
  `strict`, `max_completion_tokens`, `reasoning_effort` or
  `reasoning_content` for chat.** The catalog the user supplied lists "JSON
  Schema" as a capability of `deepseek-4.1-flash`, `glm-5.3`, `glm-5.3-flash`
  and the vision variants, so schema-constrained output is claimed; its exact
  dialect is **unverified** (§6).

Unverified and material:

1. Whether the gateway honours OpenAI's `response_format: {type: json_schema,
   json_schema: {strict: true, schema}}` as sent by go-openai, and whether the
   upstream grammar engine accepts the constructs our schemas use
   (`patternProperties`, `maxLength`, `"type": ["string", "null"]`).
2. Whether `max_completion_tokens` is accepted, translated to `max_tokens`, or
   ignored. The docs show only `max_tokens` (on the Messages example).
3. How to disable or bound reasoning. All three chat candidates are
   reasoning models and DeepSeek V4.1 Flash **thinks by default**; its
   reasoning shares the completion budget and arrives in a separate
   `reasoning_content` field (which go-openai v1.41.1 decodes). With our
   current 1500/3000-token budgets, a long think can return
   `finish_reason: length` and an **empty `content`**, which today surfaces as
   a JSON parse error (handled, but it means zero enhancements).
   go-openai has no `extra_body`; the only non-standard field it can send is
   `chat_template_kwargs`, which vLLM/SGLang-style backends read as
   `{"enable_thinking": false}`. Whether LunaRoute's upstream honours it, or
   accepts `reasoning_effort`, is **unverified**.
4. The `-flex` variants in the catalog are undocumented.

## 4. Fit, surface by surface

**Chat enhancers (incidents, NWS).** Good fit in shape: same endpoint, same
message structure, structured output claimed. Three mismatches to design
around: (a) the `isReasoningModel` name-prefix heuristic in both enhancers
must become configuration, because `deepseek-4.1-flash` matches no prefix yet
is a reasoning model; (b) the 30 s `openai.timeout` is below LunaRoute's own
60 s park window and well below the measured time-to-first-answer of these
models in thinking mode (10 to 26 s on Artificial Analysis), so it must rise;
(c) thinking must be turned off or bounded, or the completion budgets raised,
or both.

**Concurrency.** Our worst case is three chat calls in flight at once: the
ingest scheduler runs each poller in its own goroutine (`go s.run` in
`internal/ingest/scheduler.go`), so the road-incident poller (which drives
`ListIncidents` and its five cache-miss enhancements), the NWS poller and the
periodic roads refresher can overlap, and the burn-line CLI adds a fourth once
a day. All enhancement loops are **sequential** within a goroutine. On a Pro
plan (2 priority slots) the third call overflows or parks, which only costs
latency. No code change is needed for concurrency; the timeout change in (b)
absorbs it.

**Latency on the request path.** `ListIncidents` refreshes lazily on request
and runs its enhancements inline, so five cache misses at 15 s each would put
75 s on a user request. This is a pre-existing hazard that gpt-5-mini at low
effort kept small; a thinking model makes it real. The fix is model-side
(thinking off, §7) rather than architectural, but §8 phase 2 adds a guard.

**Burn line.** Best fit of the three: one transcription and one extraction a
day, off the request path, 10-minute per-line budget, and Whisper large-v3 is
the same model family as OpenAI's `whisper-1`. The two changes are a base URL
and two model ids. If audio turns out to be disabled for the operator's
organisation, the fallback is to keep **transcription on OpenAI and move only
the extraction**, which the reader's structure already allows (two separate
client calls; it would need two clients instead of one).

**Dead config and dead code become live questions.** `maxRetries` should be
implemented (one retry honouring `Retry-After` on 429, bounded backoff on
503) or removed; `HealthCheck` should be called once at startup so a wrong base
URL or key fails loudly at deploy rather than as a day of silent "enhancement
failed, keeping structural fields" log lines.

**Not recommended: the Responses or Anthropic dialects.** go-openai v1.41.1
has no Responses API client, and Messages would mean a second SDK and a second
request shape for the same models on the same gateway. Chat Completions is
the one dialect all three of our surfaces already speak and the one with the
smallest diff. Revisit only if the probe shows structured output works on
`/v1/responses` but not on chat.

## 5. Design: the compatibility option

Principle: **one place builds the client**, every surface takes it, and the
provider is decided by config. No provider-specific branches in the enhancers.

Config (`prefab.yaml`, `openai:` section, env via the existing
`PF__OPENAI__*` mapping; `config.ValidateEnvOverrides` registers new keys
automatically by reflection, so `PF__OPENAI__BASE_URL` is accepted the moment
the field exists):

```yaml
openai:
  apiKey: ""                      # PF__OPENAI__API_KEY (an lr_ key for LunaRoute)
  baseUrl: ""                     # PF__OPENAI__BASE_URL; empty = api.openai.com.
                                  # LunaRoute: https://gw.lunaroute.com/v1
  model: "gpt-5-mini"             # PF__OPENAI__MODEL
  timeout: "120s"                 # was 30s; see §4 (b)
  maxRetries: 1                   # make it real, or delete it
  # Reasoning control, replacing the gpt-5 name-prefix heuristic:
  reasoningEffort: "low"          # "" = don't send the field
  disableThinking: false          # sends chat_template_kwargs.enable_thinking=false
```

Code changes, by file:

- `internal/config/config.go`: add `BaseURL`, `ReasoningEffort`,
  `DisableThinking` to `OpenAIClient`. One new constructor,
  `OpenAIClient.NewClient() *openai.Client`, that applies `DefaultConfig(key)`,
  overrides `BaseURL` when set, and installs an `http.Client` with `Timeout`.
  (Or a tiny `internal/lib/llm` package if config should stay dependency-free;
  either is fine, the point is one constructor.)
- `internal/lib/alerts/enhancer.go` and `internal/ingest/enhance_nws.go`:
  take the config (or the built client) instead of `(apiKey, model)`; delete
  both copies of `isReasoningModel`; set `ReasoningEffort` and
  `ChatTemplateKwargs` from config; treat empty `content` with
  `finish_reason == "length"` as a distinct error so the log says "budget" not
  "invalid JSON"; keep `MaxCompletionTokens` and, if the probe says the gateway
  ignores it, also set `MaxTokens` (go-openai sends both when both are set).
- `cmd/server/main.go`: build the client once, pass it to both enhancers, call
  `HealthCheck` at startup (fatal on failure, as the missing-key check already
  is). Log `baseUrl` with the existing "OpenAI enhancement enabled" line.
- `cmd/burn-line/main.go`: read `OPENAI_BASE_URL` (the de-facto standard
  variable the LunaRoute docs use) in addition to `OPENAI_API_KEY`; add
  `-transcribe-model` (default `whisper-1`) beside `-model`; pass the base URL
  through `openai.DefaultConfig`. The reader's `Config` gets a
  `TranscribeModel` field so `openai.Whisper1` is no longer hard-coded.
- `.github/workflows/burn-line.yml`: add `OPENAI_BASE_URL: ${{ vars.OPENAI_BASE_URL }}`
  (a repo variable, not a secret; empty keeps OpenAI) and the two model
  flags from variables, so switching the CI job is a settings change.
- `internal/ingest/enhance_nws_live_test.go`: honour `PF__OPENAI__BASE_URL`.
- Tests: the existing `fakeRoundTripper` and `httptest` stubs need no change.
  Add one test per enhancer asserting the request body carries
  `reasoning_effort` / `chat_template_kwargs` exactly when configured, and one
  asserting the length-truncated-empty-content error.
- Docs: `CLAUDE.md` (Environment Setup and OpenAI API sections), `README.md`,
  `internal/services/CLAUDE.md`, `internal/ingest/CLAUDE.md`,
  `internal/clients/CLAUDE.md` (burnline row says "Twilio + OpenAI"). No
  `CHANGELOG.md` entry is required: nothing on `/api/v1` changes shape. Worth a
  dated note anyway that `enhancement.model` may now carry non-OpenAI ids,
  since consuming sites may render it.

What does **not** change: the prompts, the schemas (unless §6 forces a
simplification), the cache, the budgets, the single-writer ingest model, the
`translate, never assert` policy, and the `Enhancement` proto.

## 6. Probe checklist (phase 0, before any of §5)

A throwaway Go test or `cmd/test-enhancer` tool, run with a real `lr_` key
against each candidate model, recording the raw request and response:

1. `response_format: json_schema, strict: true` with the **incident schema
   verbatim** (`AlertEnhancementSchema`). Does it 200? Does the output validate?
   Repeat with the one-field NWS schema and the burn extraction schema.
2. The same request with `max_completion_tokens` only, then `max_tokens` only.
   Which is honoured? Does an unknown field 400?
3. `reasoning_effort: low` and `chat_template_kwargs: {enable_thinking: false}`:
   accepted, ignored, or rejected? Does `reasoning_content` appear in the
   response and is `content` ever empty with `finish_reason: length` at our
   budgets?
4. Latency: time to complete for the incident prompt, 10 runs, thinking on
   and off. The target is the current gpt-5-mini low-effort number (measure
   that too, same harness, same fixtures).
5. `GET /v1/audio/transcriptions/models` with the operator's key, then one
   real transcription of a stored burn-line recording with the prompt and
   temperature we send. Compare the transcript to the `whisper-1` one.
6. A deliberate `429`: fire three concurrent requests on a Pro key and
   confirm the go-openai error carries `HTTPStatusCode: 429` and the
   `Retry-After` header is readable (needed for the retry in §5).
7. Three fixture incidents through the full prompt (`SystemPrompt` plus the
   grounding `place_names` list) per model, scored by hand on: valid enums,
   no invented place names, no "(Style: ...)" or attribution decoration, a
   condensed summary under 120 characters with no location. These are the
   failure modes the prompt was tuned against on gpt-5-mini; a new model
   family can regress any of them.

Each item is a yes/no that changes §5: item 1 failing means dropping
`patternProperties`/`maxLength` from the schema and validating `additional_info`
in Go; item 2 decides the token-field handling; item 3 decides whether
`disableThinking` is implementable or budgets must rise to ~8k; item 5 decides
whether transcription moves at all.

## 7. Model evaluation

Candidates are the chat models in the supplied catalog: `deepseek-4.1-flash`,
`glm-5.3`, `glm-5.3-flash`, their vision siblings (unneeded; we send no images)
and `-background` variants (same model, lower scheduling priority, longer
park). `whisper-large-v3` is the only transcription model. The decision and
embedding models do not apply.

What our workload needs, in order: (1) faithfulness, since the policy is
"translate, never assert" and the incident prompt forbids naming any place not
in the input; (2) reliable schema adherence; (3) low latency with thinking
off, because incident enhancement sits on a request path; (4) cost is a
non-factor on a flat plan at ~150 calls/day.

Independent measurements (Artificial Analysis, OpenRouter, October 2026; all
chat numbers are in thinking mode, so latencies are an upper bound):

| | deepseek-4.1-flash | glm-5.3-flash | glm-5.3 |
|---|---|---|---|
| Intelligence Index | 39 | 42 | 45 |
| AA-Omniscience (higher = fewer hallucinations) | −5 | 7 | 14 |
| Output speed (tok/s) | 217 | 53 | 83 |
| Time to first token | 1.2 s | 3.0 s | 2.6 s |
| Time to first *answer* token (thinking on) | 10 s | 41 s | 27 s |
| Context | 1 M | 1 M | 1 M |

Reading:

- **`glm-5.3` is the recommendation for the two server-side enhancers and
  the burn-line extraction.** It is the most faithful of the three by a wide
  margin on the hallucination index, which is the property the grounding
  rules depend on, and it is in the same capability band as the gpt-5-mini it
  would replace. Its thinking-mode latency is unacceptable on the request
  path, so this recommendation is **conditional on §6 item 3**: thinking off
  (or `reasoning_effort: low` honoured). With thinking off its time-to-first-
  token and 83 tok/s put a 600-token incident response around 10 s, in the
  same range as today.
- **`deepseek-4.1-flash` is the fallback if latency wins.** Fastest by far
  and the cheapest per token on third-party hosts, but the negative
  hallucination score is the wrong direction for a prompt whose main failure
  mode was an invented "(near Merced)". Use it only if the fixture scoring in
  §6 item 7 shows no invented geography.
- **`glm-5.3-flash` is dominated**: slower output than `glm-5.3` on the
  measured host and less faithful. No reason to pick it here.
- **`-background` variants**: appropriate for the burn-line CLI (one call a
  day, 10-minute budget) and nothing else; a 240 s park would blow the
  enhancers' timeout and starve an ingest tick. Not worth a config knob on
  day one; the model id is already configurable.
- **`whisper-large-v3`** for transcription: the newer sibling of OpenAI's
  `whisper-1` (large-v2), same prompt-priming behaviour, and free of charge on
  the gateway at launch. The only risk is the audio feature being off for the
  organisation (§6 item 5).

Keep `gpt-5-mini` on OpenAI as the shipped default until the bake-off has run
against real traffic for a week with both providers' outputs stored under
their `enhancement.model` stamps, which the revision history already gives us
for free.

## 8. Phased plan

| Phase | Work | Size |
|---|---|---|
| 0. Probe | §6 checklist as a disposable tool or live test; write the answers into this doc. Needs an `lr_` key. | 0.5 day |
| 1. Compatibility option | §5 config + client constructor + both enhancers + burn-line CLI + workflow variables + docs. Default behaviour unchanged. | 1 day |
| 2. Robustness | Real `maxRetries` (429 honouring `Retry-After`, one bounded 503 retry); startup `HealthCheck`; empty-content-on-length error; a per-request enhancement deadline so a slow provider cannot hold `ListIncidents` past its own refresh interval. These are worth doing for OpenAI too. | 0.5 to 1 day |
| 3. Bake-off | Run production with `PF__OPENAI__BASE_URL` set on a staging or second instance, `glm-5.3` first, for a week. Compare stored `summary`/`headline` revisions against OpenAI's by `enhancement.model`. Score on the §6 item 7 rubric. | calendar week, ~0.5 day of review |
| 4. Cutover (optional) | Flip defaults in `prefab.yaml` and the burn-line repo variables; retire the OpenAI key if nothing else uses it. | hours |

Phases 1 and 2 are mergeable with no behaviour change and no LunaRoute
account. Phase 0 blocks on a key and should run first because its answers can
change phase 1 (schema simplification, token-field handling).

## 9. Cost and operational notes

- Today's OpenAI spend for this workload is small: on the order of 150
  gpt-5-mini calls a day at a few thousand tokens each, low single-digit
  dollars a month. LunaRoute's Pro plan is $99/month flat. As a **sole
  provider for this service alone** it is more expensive; it is worth it if
  the operator already holds a plan for other work, values zero retention, or
  wants provider independence. That is the operator's call, and the
  compatibility option is cheap enough to build regardless.
- A single `lr_` key serves both the server and the CI job; the docs'
  `LUNAROUTE-PROJECT-ID` header would let usage be attributed per surface, but
  go-openai cannot add arbitrary headers without a custom transport. Skip
  unless the ledger matters.
- Two secrets become four settings: `PF__OPENAI__API_KEY` and
  `PF__OPENAI__BASE_URL` on the server, `OPENAI_API_KEY` (secret) and
  `OPENAI_BASE_URL` (variable) in GitHub Actions. The variable name follows the
  LunaRoute docs' own setup instructions so an operator can copy them.

## Sources

- LunaRoute site and docs: https://www.lunaroute.com/ ,
  https://docs.lunaroute.com/concepts/lanes/ ,
  https://docs.lunaroute.com/api/audio-transcriptions/ ,
  https://docs.lunaroute.com/api/errors/ ,
  https://docs.lunaroute.com/getting-started/authentication/ ,
  https://docs.lunaroute.com/models/
- Model measurements: https://artificialanalysis.ai/models/comparisons/deepseek-v4-1-flash-vs-glm-5-3 ,
  https://artificialanalysis.ai/models/comparisons/deepseek-v4-1-flash-vs-glm-5-3-flash ,
  https://openrouter.ai/compare/deepseek/deepseek-v4.1-flash/z-ai/glm-5.3
- DeepSeek thinking-mode default and `reasoning_content`: https://api-docs.deepseek.com/guides/thinking_mode/
