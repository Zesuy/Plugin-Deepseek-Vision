# Troubleshooting

## Plugin is not discovered

Check the file name and platform directory. Manual mode must contain exactly
one unversioned candidate:

```text
<CLI_PROXY_PLUGIN_PATH>/linux/amd64/deepseek-vision.so
```

The basename must be `deepseek-vision.so` in manual mode. Confirm the host and
artifact architecture with `file`, verify the ABI symbol, and check for stale
versioned candidates:

```bash
file plugins/linux/amd64/deepseek-vision.so
nm -D plugins/linux/amd64/deepseek-vision.so | grep cliproxy_plugin_init
test "$(find plugins/linux/amd64 -maxdepth 1 -type f -name 'deepseek-vision*.so' | wc -l)" -eq 1
```

For store/versioned mode, the expected filename is
`deepseek-vision-vX.Y.Z.so`; set `plugins.configs.deepseek-vision.store.version`
to the same version and inspect the management API. Its `path` must point to
that versioned file and `metadata.version` must match `X.Y.Z`:

```bash
curl -fsS -H 'Authorization: Bearer <management-key>' \
  http://127.0.0.1:8085/v0/management/plugins \
  | jq '.plugins[] | select(.id == "deepseek-vision") | {path, registered, effective_enabled, metadata}'
curl -fsS -H 'Authorization: Bearer <management-key>' \
  http://127.0.0.1:8085/v0/management/plugins/deepseek-vision/config \
  | jq '{store, enabled, priority}'
```

Restart the host after replacing a dynamic library and inspect its plugin
registration log. Ensure the host's plugin directory is not mounted read-only
for a deployment that performs plugin-store installation.

If a manual install has both `deepseek-vision.so` and
`deepseek-vision-v*.so`, remove the versioned candidates and restart. If a
store install has multiple versions, the pinned `store.version` must match an
existing artifact; reinstall the requested archive if the host cleaned up an
unselected old file.

## CPAMC shows no configurable fields

CLIProxyAPI exposes `config_fields` only after a plugin has registered. A
discovered but disabled or unconfigured binary can therefore show the generic
"no declared visual configuration fields" message before its first load.

For first-time CPAMC setup, enable the plugin and save once, wait until its
status becomes `registered`, then reopen the configuration drawer. Invalid or
incomplete edits never return a lifecycle error to the host: the plugin stays
registered and keeps the last known-good runtime, or remains safely unavailable
when no valid generation exists. Correct the field and save again; restarting
CLIProxyAPI is not required.

Verify what the host received from the plugin with:

```bash
curl -fsS -H 'Authorization: Bearer <management-key>' \
  http://127.0.0.1:8085/v0/management/plugins \
  | jq '.plugins[]
      | select(.id == "deepseek-vision")
      | {registered, effective_enabled, config_fields}'
```

A current binary reports nine fields: model, language, global in-flight vision
requests, emergency image ceiling, total timeout, three cache controls, and the
plaintext trace switch.
If `registered` remains `false`, restart
CLIProxyAPI after replacing the library and inspect the plugin registration
error in the host log; an old binary or failed ABI load cannot publish field
metadata.

## Requests pass through unexpectedly

The plugin intentionally handles only `/v1/responses`, source format
`openai-response`, and final models listed in `target_models`. An alias is
checked after host resolution; `RequestedModel` alone is not sufficient.
`/v1/responses/compact`, non-Responses APIs, non-target models and requests
without images are expected pass-through cases. Anthropic Messages and Chat
Completions are outside the plugin contract and are not converted. If a request
uses `previous_response_id`, remember that server-side history remains hidden
from this callback; only images present in the current visible `input[]` can be
rewritten.

For v0.1.1, use `deepseek-v4-flash` when checking a real upstream. The
`deepseek-v4-pro` entry is future-supported configuration only; it is not a
required or probed service in this release.

## HTTP 502 from image requests

Check that `vision_model` exists in CLIProxyAPI and accepts image input through
OpenAI Responses. Provider errors, malformed VLM JSON, response-size limits and
exhausted host retries are intentional failures.
The error returned to the client is redacted; inspect only service-side status
metrics, never enable logging of request bodies or Authorization headers. For
eligible Responses image requests, plugin failures are terminal (typically
502); the plugin does not fall back to forwarding the original image.

## Docker issues

`docker build` requires a running daemon and access to the Go base image/module
proxy. Use `docker compose ... config` first to catch invalid paths or YAML.
Create the host directories referenced by the compose file and verify that the
plugin is under `plugins/linux/amd64`, not directly under `plugins/`.

## Slow or rejected requests

Adjust `max_inflight_vision_requests` or increase the bounded total timeout only
after checking VLM latency. Prompt groups queue instead of being rejected when
all callback slots are occupied. A 413 response distinguishes request-body,
image-reference and emergency unique-image limits. Check the matching CLIProxyAPI warning
from `host.log` for `limit_kind`, `actual`, `maximum`, active size settings and
`config_generation`; ABI admission warnings include their byte budget and
in-flight usage. This ordinary host-log warning never contains image data or
credentials. Provider retry behavior is controlled by CLIProxyAPI.

If the target model nevertheless calls `view_image`, verify that the rewritten
prompt contains the fixed vision-preprocessing notice and no local attachment
path. A successful rich `view_image` tool result is an array and will be
converted again; a string result (including a tool error) is preserved as a
normal Responses function output and must not be rejected by the plugin.

For a multi-turn request whose ordinary 413 warning is insufficient, enable:

```yaml
trace_enabled: true
```

Reproduce once, then inspect `logs/deepseek-vision-trace/events.jsonl` and the
referenced request bundle. `20-discovery.json` includes total, unique,
duplicate, prompt-group, last-image-item, earlier-item, content, and
function-output image counts. VLM artifacts show each multi-image call and any
413 split batches. The inbound body and image references are preserved in plaintext, so
disable tracing and remove the bundle after diagnosis.
