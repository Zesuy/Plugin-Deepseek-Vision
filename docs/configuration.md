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
| `max_inflight_vision_requests` | Global in-flight prompt-group VLM calls; excess work queues | 4 |
| `emergency_max_images_per_request` | Last-resort unique-image ceiling | 256 |
| `max_request_bytes` | Raw Responses body limit | 20 MiB |
| `max_image_reference_bytes` | URL/data URI limit | 15 MiB |
| `max_response_bytes` | VLM response limit | 4 MiB |
| `max_result_chars` | Extracted result limit | 20,000 |
| `analysis_cache_size` | Maximum prompt-group analysis entries; `0` disables | 128 |
| `analysis_cache_ttl_seconds` | Data-URI analysis TTL | 900 |
| `analysis_url_cache_ttl_seconds` | URL-image analysis TTL | 120 |
| `trace_enabled` | Full plaintext debug trace | `false` |

The native ABI applies an additional process-wide admission budget of 32 MiB of
raw RPC bytes and four concurrent callbacks. This protects the C-to-Go copy and
subsequent JSON/rewrite allocations; it can reject a request before the larger
per-configuration `max_request_bytes` ceiling is reached.

Limit rejections are diagnosed through CLIProxyAPI's native `host.log`. A 413
warning contains `limit_kind`, `actual`, `maximum`, the active body/reference/
emergency image-count settings, and `config_generation`. ABI admission failures instead
report the ABI request bytes, hard cap, process budget and in-flight usage. No
request body, image reference, header or credential is logged.

The plugin calls `host.model.execute` with OpenAI Responses input and lets
CLIProxyAPI route `vision_model` using its existing provider credentials. The
nested execution skips this plugin's own interceptor, so it does not recurse.
No additional VLM endpoint or key is supported or required. CLIProxyAPI also
owns provider protocol translation, transport, retry, and credential policy.

The CPAMC form exposes `vision_model`, `language`, global in-flight vision
requests, the emergency image ceiling, total timeout, the three cache controls,
and a boolean `trace_enabled` switch. Their descriptions include bilingual
defaults; key integer controls also state their validation ranges. Advanced
size controls remain available through YAML.

## Full-context debug trace

`trace_enabled: true` creates `logs/deepseek-vision-trace/events.jsonl` and one
request bundle below `logs/deepseek-vision-trace/requests/`. In the Docker
example this is the host-mounted `./logs/deepseek-vision-trace/` directory.
Each bundle preserves the exact inbound multi-turn body, complete image URLs or
data URIs, discovered image positions and prompt-group context, cache/deduplication plan,
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

Deprecated `vision_backend`, `vision_base_url`, `vision_api_key_env`,
`per_call_timeout_seconds`, `retry_max_attempts`, `max_concurrency`,
`cache_size`, and `cache_ttl_seconds` fields are accepted only for decoding and
unconditionally ignored. Configure the actual model/provider in CLIProxyAPI.

Each runtime generation owns an LRU using the configured capacity and TTLs.
Keys hash the ordered prompt-group image references, model, normalized language,
and complete prompt.
Reconfigure or restart creates a fresh cache. Setting `analysis_cache_size: 0`
disables cross-request reuse while retaining single-request deduplication.

`deepseek-v4-pro` is not enabled by default because its Responses endpoint is
not part of the validated release surface. Add it explicitly to
`target_models` only after verifying that upstream path in your deployment.

The VLM prompt is not a generic caption request. All images attached to one
Responses content/output item are sent together in order. Luna is asked to
label the images, faithfully transcribe text, and describe both individual
content and cross-image relationships. Image text is declared untrusted and
must never be followed as an instruction. The configured language applies to
the explanation while transcription preserves original characters. Up to
2,000 runes of text from the same prompt item are included as bounded context.
The rewritten prompt explicitly tells the non-vision target model that these
attachments have already been analyzed and must not be reopened with
`view_image`; exact Codex temporary paths tied to the consumed image wrappers
are removed while the user's request text is preserved.

`max_images_per_request` from older builds remains decodable but is ignored. It
cannot silently restore the former four-block rejection behavior.

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
idempotent, removes every discovered `input_image` block and reference from its
original structured position, and verifies that no image block remains.
