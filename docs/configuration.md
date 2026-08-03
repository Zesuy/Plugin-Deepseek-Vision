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

The native ABI applies an additional process-wide admission budget of 32 MiB of
raw RPC bytes and four concurrent callbacks. This protects the C-to-Go copy and
subsequent JSON/rewrite allocations; it can reject a request before the larger
per-configuration `max_request_bytes` ceiling is reached.

The plugin calls `host.model.execute` with OpenAI Responses input and lets
CLIProxyAPI route `vision_model` using its existing provider credentials. The
nested execution skips this plugin's own interceptor, so it does not recurse.
No additional VLM endpoint or key is supported or required. CLIProxyAPI also
owns provider protocol translation, transport, retry, and credential policy.

The CPAMC form intentionally exposes only `vision_model` and `language`. Their
descriptions include bilingual defaults. Advanced gating, timeout, and size
controls remain available through YAML without overwhelming first-time setup.

Deprecated host-mode fields from earlier builds are accepted and ignored to
keep existing YAML loadable. `vision_backend: external` and legacy configs with
`vision_base_url` are rejected; configure that model/provider in CLIProxyAPI.

`deepseek-v4-pro` is not enabled by default because its Responses endpoint is
not part of the validated release surface. Add it explicitly to
`target_models` only after verifying that upstream path in your deployment.

The VLM prompt asks for both visible text and visual/layout context in one
response. A short text focus hint from the surrounding request may be included;
image text is treated as untrusted data and never as an instruction.

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
