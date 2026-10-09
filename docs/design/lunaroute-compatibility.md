# Scoping: LunaRoute as a compatibility option for every AI feature

Status: proposed (2026-10-09); **phase 0 probe run on 2026-10-09, results in
§6a**. Nothing beyond the probe is implemented. This document
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

Resolved by the phase 0 probe (§6a), originally unverified:

1. **Strict `json_schema` is honoured** by the gateway and by `glm-5.3` /
   `glm-5.3-flash` with our schemas verbatim. **`deepseek-4.1-flash` rejects
   the incident schema** (400, "Unsupported JSON Schema structure false"):
   its Outlines grammar backend cannot compile the `additional_info` object,
   which has `patternProperties` and `additionalProperties: false` but no
   `properties`. The one-field NWS schema and the burn schema pass on all
   three models.
2. **Both `max_completion_tokens` and `max_tokens` are honoured** on all three
   models (OpenAI's gpt-5 family rejects `max_tokens`; keep sending
   `max_completion_tokens`).
3. **All three models think by default and it breaks the production
   budgets**: `glm-5.3` spent the whole 3000-token incident budget on
   reasoning (65 s, empty `content`, `finish_reason: length`), and every
   model spent the whole 1500-token NWS budget the same way. **Both knobs
   work on the GLM models**: `reasoning_effort: low` cut `glm-5.3` to 11
   reasoning tokens, 2.8 s, valid output; `chat_template_kwargs.enable_thinking:
   false` to 0 tokens, 3.3 s. Since OpenAI also accepts `reasoning_effort`
   (and rejects `chat_template_kwargs`), one config key covers both
   providers. DeepSeek's reaction to the knobs is still unknown because its
   schema rejection pre-empted the test; the portable-schema run will tell.
4. **Audio is live and included**: `GET /v1/audio/transcriptions/models`
   returns `whisper-large-v3` with `audio_policy: included`, and the synthetic
   burn-line recording transcribed word-perfectly in 0.9 s.
5. The `-flex` variants remain undocumented and were not probed.

## 4. Fit, surface by surface

**Chat enhancers (incidents, NWS).** Good fit in shape: same endpoint, same
message structure, structured output claimed. Three mismatches to design
around: (a) the `isReasoningModel` name-prefix heuristic in both enhancers
must become configuration, because `deepseek-4.1-flash` matches no prefix yet
is a reasoning model; (b) the 30 s `openai.timeout` is below LunaRoute's own
60 s park window and well below the measured time-to-first-answer of these
models in thinking mode (10 to 26 s on Artificial Analysis), so it must rise;
(c) thinking must be turned off: the probe showed the budgets are consumed by
reasoning otherwise, and `reasoning_effort: low` is enough.

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
  # Reasoning control, replacing the gpt-5 name-prefix heuristic. Sent
  # verbatim whenever non-empty; OpenAI's gpt-5 family and LunaRoute's GLM
  # models both honour it (§6a), so one key covers both providers.
  reasoningEffort: "low"          # "" = don't send the field
```

Code changes, by file:

- `internal/config/config.go`: add `BaseURL` and `ReasoningEffort` to
  `OpenAIClient`. One new constructor,
  `OpenAIClient.NewClient() *openai.Client`, that applies `DefaultConfig(key)`,
  overrides `BaseURL` when set, and installs an `http.Client` with `Timeout`.
  (Or a tiny `internal/lib/llm` package if config should stay dependency-free;
  either is fine, the point is one constructor.)
- `internal/lib/alerts/enhancer.go` and `internal/ingest/enhance_nws.go`:
  take the config (or the built client) instead of `(apiKey, model)`; delete
  both copies of `isReasoningModel`; set `ReasoningEffort` from config;
  treat empty `content` with
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

- `internal/lib/alerts/openai.go`: **no schema change.** The portable
  `additional_info` form was only ever for DeepSeek, which §6b rules out, and
  it costs the GLM models their metadata. Keep the schema as is.
- `internal/clients/burnline/reader.go`: the extraction sends
  `temperature: 0.1`, which the gpt-5 family rejects (the OpenAI baseline leg
  failed exactly there) and the LunaRoute models accept. Production is fine
  only because the CLI defaults to gpt-4o. Either drop the temperature or
  keep the burn-line model off the gpt-5 family; drop it, since the
  structured output does not benefit from it.

What does **not** change: the prompts, the cache, the budgets, the
single-writer ingest model, the `translate, never assert` policy, and the
`Enhancement` proto.

## 6. Probe checklist (phase 0, before any of §5)

**Implemented as `cmd/test-llm`** (`make test-llm`), run from CI by the manual
workflow `.github/workflows/llm-probe.yml`. The workflow reads the repo secret
`LUNAROUTE_API_KEY` for the LunaRoute leg and the existing `OPENAI_API_KEY`
for a gpt-5-mini baseline leg, synthesizes a burn-line recording with espeak
so the audio endpoint is exercised without a phone call, and posts both
Markdown reports as the run's step summary (also uploaded as artifacts). Run
it from the Actions tab once the secret exists; the inputs let you change the
model list, the latency repetitions and the parallel-request count.

The tool sends the production request shapes (it imports the incident prompt
and schema, and the exported `ingest.NWSSystemPrompt`/`NWSSummarySchema` and
`burnline.ExtractPrompt`/`ExtractSchema`), so a pass means the real code would
pass. Items 1 to 6 are automated; item 7 prints the raw outputs for hand
scoring with the enum, length, decoration and forbidden-place checks applied
heuristically.

The checks, against each candidate model, recording the raw request and
response:

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

## 6a. Probe results (run 2, 2026-10-09 12:20 UTC)

Workflow run [37929213439](https://github.com/dpup/sierra-data/actions/runs/37929213439);
the full reports are its step summary and artifacts. Production-shape
requests were sent **without** `reasoning_effort` on this run (the tool now
sends it by default, matching production on gpt-5-mini).

| Check | gpt-5-mini (OpenAI) | glm-5.3 | glm-5.3-flash | deepseek-4.1-flash |
|---|---|---|---|---|
| Listed in `/models` | yes (137) | yes (14) | yes | yes |
| Incident schema, verbatim, 3000 budget | 200, valid, 11.7 s (1344 reasoning tok) | **empty**: all 3000 tokens reasoning, 65 s | 200, valid, 19.2 s (1092 reasoning tok) | **400**: schema rejected |
| NWS schema, 1500 budget | 200, 7.8 s; summary 324 chars (cap 320) | **empty**, 17.8 s | **empty**, 16.2 s | **empty**, 9.2 s |
| Burn extraction (temp 0.1), expected `orange` | **400**: temperature not allowed | `orange`, 98, 5.4 s | `orange`, 98, 3.9 s | `orange`, 100, 1.5 s |
| `max_completion_tokens` / `max_tokens` | honoured / rejected | both honoured | both honoured | both honoured |
| `reasoning_effort: low` on the incident request | 384 reasoning tok, 5.3 s, valid | 11 tok, 2.8 s, valid | 1 tok, 3.2 s, valid | blocked by schema |
| `enable_thinking: false` | rejected (400) | 0 tok, 3.3 s, valid | 0 tok, 3.9 s, valid | blocked by schema |
| Latency, 3 runs, no tuning | 8.5 / 9.1 / 11.2 s | 15.1 / 22.6 / 45 s | 16.9 / 19.8 / 20.1 s | all failed |
| Audio (synthetic burn line) | whisper-1, 1.8 s, exact | whisper-large-v3, 0.9 s, exact (shared) | | |
| 3 parallel requests | n/a (probe bug, fixed) | 3 × 200 in ~10.5 s, no 429 | | |

Hand scoring of the one complete LunaRoute incident output (`glm-5.3-flash`):
valid enums, `moderate`/`restricted`, both timestamps correct, 74-character
condensed summary with no location, "near Arnold" taken from the supplied
place list, no decoration. Equivalent to the gpt-5-mini output.

What this settles: the gateway is a drop-in for the request shapes, the
single required change is sending `reasoning_effort` from config (not from a
model-name prefix), and the incident schema needs the portable
`additional_info` form only if DeepSeek is in play. What the next run must
settle: the portable schema on all four models, DeepSeek with thinking
tuned, and hand-scored incident outputs from `glm-5.3` with tuning on (the
tool now prints them).

## 6b. Probe results (run 3, 2026-10-09 12:34 UTC, tuning on)

Workflow run [37930798331](https://github.com/dpup/sierra-data/actions/runs/37930798331).
Every production-shape request carried `reasoning_effort: low`; the
LunaRoute leg **also** carried `chat_template_kwargs.enable_thinking: false`
(the dispatch set `no_thinking`), so its figures are for thinking fully off.
The OpenAI leg is now the true production baseline (gpt-5-mini sends `low`).

| Check | gpt-5-mini (`low`) | glm-5.3 (off) | glm-5.3-flash (off) | deepseek-4.1-flash (off) |
|---|---|---|---|---|
| Incident schema, verbatim | valid, 5.2 s | valid, 1.9 s | valid, 3.0 s | **400**, schema rejected |
| Incident schema, portable `additional_info` | valid, 6 pairs, 5.2 s | valid, **0 pairs**, 1.5 s | valid, **0 pairs**, 2.1 s | valid, 5 pairs, 1.6 s |
| NWS summary | 309 chars, 2.9 s | **`"..."`** (6 tokens), 5.1 s | 323 chars (cap 320), 1.0 s | 305 chars, 0.7 s |
| Burn extraction, expected `orange` | 400 (temperature) | `orange`, 98, 2.6 s | `orange`, **confidence 0.98 and transcript `"..."`**, 0.8 s | `orange`, 100, 0.6 s |
| Incident latency, 3 runs | 4.7 / 5.0 / 5.1 s | 1.0 / 1.1 / 1.2 s | 2.5 / 2.6 / 3.5 s | all failed (schema) |
| Provider-default thinking, incident | 1152 reasoning tok, 11 s | 3000 tok, empty, 17.7 s | 2115 tok, valid, 26.4 s | schema |
| `reasoning_effort: low` alone, incident | 384 tok, 5.5 s, valid | 1 tok, 1.6 s, valid | 1 tok, 2.5 s, valid | schema |
| Audio | whisper-1, 2.4 s, exact | whisper-large-v3, 0.9 s, exact | | |
| 3 parallel requests | 3 × 200, 2.4 to 4 s | 3 × 200, 4.8 to 6.4 s | | |

Hand scoring of the incident outputs (all tuned):

- **`glm-5.3`**: three outputs, all correct. Enums `moderate`/`restricted`,
  timestamps exact, "near Arnold" from the place list, lane number and
  responder carried through, no decoration, no invention. Equivalent to
  gpt-5-mini's and a fifth of the latency.
- **`glm-5.3-flash`**: correct on the verbatim-schema run; the
  thinking-off variant graded a blocked through lane `road_status: open`,
  and the `low` variant returned `restriction_details: null`. Weaker
  classification than `glm-5.3`.
- **`deepseek-4.1-flash`** (portable schema): enums and timestamps correct,
  but `details` dropped the route ("blocking one lane of the highway") and
  `restriction_details` asserted **"one-way traffic control is in effect"**,
  which is nowhere in the input. That is the invented-fact failure mode the
  prompt exists to prevent, on the first fixture.

Two **new findings**, both from thinking being fully off rather than `low`:

1. **Placeholder output.** `glm-5.3` answered the NWS request with
   `{"summary": "..."}` and `glm-5.3-flash` answered the burn request with
   `cleanedTranscription: "..."` and `confidence: 0.98` (a fraction; the
   prompt asks for 0 to 100 and `reader.go` would store 0). Neither happened
   with `reasoning_effort: low` alone on the incident request, and both
   fields are the ones that ask the model to reproduce or condense a long
   text, so the hypothesis is that `enable_thinking: false` costs the GLM
   models the pass they use to do that. **Run 4 with the workflow defaults
   (`low` only, `no_thinking` off) decides it.** The probe now flags
   placeholder answers and fraction-scaled confidence explicitly.
2. **The portable schema costs the GLM models their metadata**: both
   returned an empty `additional_info` list where gpt-5-mini gave six pairs
   and DeepSeek five. The prompt still describes the field as an object with
   named keys; if the portable form ships, one prompt line ("a list of
   key/value pairs; include at least incident_type") should restore it.

What this settles beyond §6a: with `low`, `glm-5.3` is faster than
gpt-5-mini by about 4 s per incident with equal output quality; DeepSeek
fails the faithfulness bar on first contact and is not worth the schema
change; `glm-5.3-flash` is both slower and less accurate than `glm-5.3`.
What remains: confirm `low` alone keeps the NWS and burn outputs whole (run
4), then phase 1.

## 6c. Probe results (run 4, 2026-10-09 15:31 UTC, same tuning as run 3)

Workflow run [37952188309](https://github.com/dpup/sierra-data/actions/runs/37952188309).
Dispatched with `no_thinking` ticked again, so the LunaRoute leg repeated
run 3's configuration (`low` plus `enable_thinking: false`) rather than the
`low`-alone check. That makes it a second sample of the same setting, which
turned out to be what was needed.

| Check | gpt-5-mini (`low`) | glm-5.3 (off) | glm-5.3-flash (off) | deepseek-4.1-flash (off) |
|---|---|---|---|---|
| Incident schema, verbatim | valid, 6.2 s | valid, 4.3 s | valid, 2.9 s | 400, schema |
| NWS summary | 323 chars (cap 320), 2.5 s | **283 chars, whole**, 1.0 s | 358 chars, 2.0 s | 299 chars, 0.5 s |
| Burn extraction | 400 (temperature) | `orange`, 97, **transcript whole**, 1.5 s | `orange`, 99, **transcript `"..."` again**, 2.2 s | `orange`, **confidence 0.98**, 1.1 s |
| Incident latency, 3 runs | 4.8 / 4.8 / 4.9 s | 1.8 / 2.0 / 2.5 s | 2.9 / 3.8 / 4.1 s | schema |
| `low` alone, incident | 512 reasoning tok, 5.3 s, valid | 1 tok, 1.9 s, valid | 1 tok, 4.1 s, valid | schema |
| Audio | whisper-1, 1.9 s, exact | whisper-large-v3, 1.0 s, exact | | |
| 3 parallel requests | 3 × 200, 3 to 5 s | 3 × 200, 4.4 to 9.3 s | | |

What changed against run 3: `glm-5.3`'s NWS summary and burn transcript
came back **whole** under the same setting that produced `"..."` last time,
while `glm-5.3-flash` produced the `"..."` transcript **twice in two runs**.
So the placeholder is **intermittent for `glm-5.3` and habitual for
`glm-5.3-flash`**, and the knob is not the clean explanation §6b proposed.
That changes the conclusion in one way: whichever setting ships, the code
must treat a placeholder as a failed enhancement rather than as content.
Specifically:

- `enhance_nws.go` already rejects an empty summary; it must also reject a
  summary that is a placeholder (`"..."` or a handful of characters), so the
  raw alert is served instead.
- `burnline/reader.go` falls back to the raw transcript only when the cleaned
  one is empty; `"..."` would be stored as the transcript. Same fix.
- `reader.go` should read a confidence in (0, 1] as a fraction and scale it,
  since DeepSeek returned `0.98` twice and `glm-5.3-flash` once.

All three are cheap, provider-neutral, and belong in phase 2 whichever
provider is used. The remaining `low`-alone comparison is still worth one
run, and the probe now judges the NWS and burn requests under each knob on
its own, so the next dispatch answers it however the inputs are set.

Hand scoring, incident outputs: `glm-5.3` correct on all three variants
again (one `enable_thinking: false` output returned an empty
`additional_info` object, the only blemish). `glm-5.3-flash` correct on all
three, with one thinking-off output that narrated the dispatch log
timestamp by timestamp in `details`. DeepSeek, portable schema, correct this
time but with three thin metadata pairs; the portable form also led
`glm-5.3` to copy every top-level field into `additional_info` (12 pairs of
noise), which closes the question of ever shipping it.

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
  would replace. The condition from the first draft is now met: with
  `reasoning_effort: low` it answered the production incident request in
  1.6 to 2.8 s with valid output (§6a, §6b), against 5.0 to 5.5 s for
  gpt-5-mini under the same setting, and its three hand-scored incident
  outputs were all correct. Without the setting it is unusable (65 s, empty
  output), so the config change in §5 is a prerequisite, not a nicety. Use
  `reasoning_effort: low`, the one knob OpenAI also accepts. The placeholder
  answers seen with thinking fully off (§6b) recurred only on
  `glm-5.3-flash` in run 4 (§6c); on `glm-5.3` they are intermittent, and
  the phase-2 guards in §6c make them a served raw alert rather than bad
  content either way.
- **`deepseek-4.1-flash` is not recommended.** It needs a schema change to
  run at all (§6a), and with that change in place it invented "one-way
  traffic control is in effect" on the first fixture (§6b), the exact
  failure mode the grounding rules exist to prevent. Its speed advantage over
  `glm-5.3` with thinking off is also gone (1.6 s vs 1.0 s). Drop the
  portable-schema work unless another reason for it appears.
- **`glm-5.3-flash` is dominated**: slower than `glm-5.3` with thinking
  off (2.6 s vs 1.1 s median) and less accurate on the fixture (graded a
  blocked lane `open`, returned a placeholder transcript, §6b). No reason to
  pick it here.
- **`-background` variants**: appropriate for the burn-line CLI (one call a
  day, 10-minute budget) and nothing else; a 240 s park would blow the
  enhancers' timeout and starve an ingest tick. Not worth a config knob on
  day one; the model id is already configurable.
- **`whisper-large-v3`** for transcription: the newer sibling of OpenAI's
  `whisper-1` (large-v2), same prompt-priming behaviour, free of charge on
  the gateway at launch, and confirmed live for this organisation with a
  word-perfect transcript in half the time (§6a).

Keep `gpt-5-mini` on OpenAI as the shipped default until the bake-off has run
against real traffic for a week with both providers' outputs stored under
their `enhancement.model` stamps, which the revision history already gives us
for free.

## 8. Phased plan

| Phase | Work | Size |
|---|---|---|
| 0. Probe | **Runs 2 to 4 done (§6a to §6c).** The probe now compares both knobs on NWS and burn by itself; one more dispatch is optional confirmation, not a blocker. | done |
| 1. Compatibility option | §5 config + client constructor + both enhancers + burn-line CLI + workflow variables + docs. Default behaviour unchanged. | 1 day |
| 2. Robustness | Real `maxRetries` (429 honouring `Retry-After`, one bounded 503 retry); startup `HealthCheck`; empty-content-on-length error; placeholder-output and fraction-confidence guards (§6c); a per-request enhancement deadline so a slow provider cannot hold `ListIncidents` past its own refresh interval. These are worth doing for OpenAI too. | 0.5 to 1 day |
| 3. Bake-off | Run production with `PF__OPENAI__BASE_URL` set on a staging or second instance, `glm-5.3` first, for a week. Compare stored `summary`/`headline` revisions against OpenAI's by `enhancement.model`. Score on the §6 item 7 rubric. | calendar week, ~0.5 day of review |
| 4. Cutover (optional) | Flip defaults in `prefab.yaml` and the burn-line repo variables; retire the OpenAI key if nothing else uses it. | hours |

Phases 1 and 2 are mergeable with no behaviour change and no LunaRoute
account. Phase 0's answers (§6a) fixed phase 1's shape: `reasoningEffort` from
config, `max_completion_tokens` kept, the portable schema only if DeepSeek
matters.

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
