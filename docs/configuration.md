# Configuration

The host passes either the plugin mapping directly or the complete
`plugins.configs.deepseek-vision` document. `config.example.yaml` shows the
complete host shape. All values are validated before an atomic reconfigure;
an invalid update leaves the previous snapshot active.

| Field | Meaning | Default |
| --- | --- | --- |
| `enabled`, `priority` | Host-owned switches | host-defined |
| `target_models` | Final upstream models eligible for interception | `deepseek-v4-flash` |
| `vision_model` | VLM model identifier | `gpt-5.6-luna` |
| `language` | Preferred output language | `zh` |
| `request_timeout_seconds` | Total preprocessing deadline | 120 |
| `max_images_per_request` | Image blocks accepted per request | 4 |
| `max_request_bytes` | Raw Responses body limit | 20 MiB |
| `max_image_reference_bytes` | URL/data URI limit | 15 MiB |
| `max_response_bytes` | VLM response limit | 4 MiB |
| `max_result_chars` | Extracted result limit | 20,000 |
| `analysis_cache_size` | Maximum derived-text cache entries; `0` disables | 128 |
| `analysis_cache_ttl_seconds` | Data-URI analysis TTL | 900 |
| `analysis_url_cache_ttl_seconds` | URL-image analysis TTL | 120 |
| `trace_enabled` | Full plaintext debug trace | `false` |

The native ABI applies an additional process-wide admission budget of 32 MiB of
raw RPC bytes and four concurrent callbacks. This protects the C-to-Go copy and
subsequent JSON/rewrite allocations; it can reject a request before the larger
per-configuration `max_request_bytes` ceiling is reached.

Limit rejections are diagnosed through CLIProxyAPI's native `host.log`. A 413
warning contains `limit_kind`, `actual`, `maximum`, the active body/reference/
image-count settings, and `config_generation`. ABI admission failures instead
report the ABI request bytes, hard cap, process budget and in-flight usage. No
request body, image reference, header or credential is logged.

The plugin calls `host.model.execute` with OpenAI Responses input and lets
CLIProxyAPI route `vision_model` using its existing provider credentials. The
nested execution skips this plugin's own interceptor, so it does not recurse.
No additional VLM endpoint or key is supported or required. CLIProxyAPI also
owns provider protocol translation, transport, retry, and credential policy.

The CPAMC form exposes `vision_model`, `language`, the three cache controls, and
a boolean `trace_enabled` switch. Their descriptions include bilingual
defaults. Advanced gating, timeout, and size controls remain available through
YAML.

## Full-context debug trace

`trace_enabled: true` creates `logs/deepseek-vision-trace/events.jsonl` and one
request bundle below `logs/deepseek-vision-trace/requests/`. In the Docker
example this is the host-mounted `./logs/deepseek-vision-trace/` directory.
Each bundle preserves the exact inbound multi-turn body, complete image URLs or
data URIs, discovered image positions and focus hints, cache/deduplication plan,
every VLM request and response, parsed VLM result, rewritten request body, and
the final interceptor result. The event stream references the bundle and uses
the host-provided request/trace IDs.

This mode is intentionally high sensitivity. Treat the directory as a complete
copy of user conversations and image data. Authorization, API-key, token,
secret, credential, and cookie header/metadata fields are always replaced with
`[REDACTED]`; image URLs and data URIs are not redacted. Files use mode `0600`,
directories use `0700`, request bundles are capped at 1 GiB by deleting the
oldest complete inactive bundle, and the event stream rotates at 64 MiB with
three backups. Disable the switch immediately after diagnosis. Disabling does
not delete existing traces.

Trace open/write/rotation failures never reject configuration or change a
request result. The plugin disables tracing and emits one ordinary host warning
for the failed generation.

All deprecated fields from earlier builds, including `vision_backend`,
`vision_base_url`, `vision_api_key_env`, retry, concurrency, and cache fields,
are accepted only for decoding and unconditionally ignored. Configure the
actual model/provider in CLIProxyAPI.

Each runtime generation owns an LRU using the configured capacity and TTLs.
Keys hash the image reference, model, normalized language, and complete prompt.
Reconfigure or restart creates a fresh cache. Setting `analysis_cache_size: 0`
disables cross-request reuse while retaining single-request deduplication.

`deepseek-v4-pro` is not enabled by default because its Responses endpoint is
not part of the validated release surface. Add it explicitly to
`target_models` only after verifying that upstream path in your deployment.

The VLM prompt is not a generic caption request. It requires a `Visible text:`
section that faithfully transcribes text, code, tables, labels, and errors
(`[illegible]` instead of guessing), plus a `Visual description:` section for
UI/layout, objects, relationships, charts, and context. Image text is declared
untrusted and must never be followed as an instruction. The description uses
the configured language while transcription preserves original characters. A
bounded 2,000-rune focus hint from surrounding user text may be appended.

## Gate and pass-through rules

The handler requires all of:

```text
SourceFormat == "openai-response"
metadata.request_path == "/v1/responses"
final Model in target_models
```

The compact path, unknown image references and unsupported request shapes do not
silently pass an image through: unsupported images terminate with a client
error, while a VLM failure terminates with HTTP 502. A successful rewrite is
idempotent and no original image URL/data URI remains in the forwarded body.
