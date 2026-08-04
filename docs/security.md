# Security notes

## Credentials and data

- The plugin reuses a model and credential already configured in CLIProxyAPI;
  it has no key field, environment-variable credential, or external backend.
- The plugin does not log Authorization headers, complete endpoint URLs,
  image data URIs or upstream response bodies.
- The plugin caches only derived analysis text and SHA-256 keys. It never keeps
  raw image bytes or the original image reference in cache entries; URL entries
  use a shorter TTL because their content may change.
- Package scripts scan source and generated archives for obvious key markers and
  fail closed. This is a guardrail, not a replacement for organization-wide
  secret scanning.

## Network

CLIProxyAPI owns provider transport, retries, concurrency, and credential
handling. Literal loopback/private/link-local image
URLs are rejected; deployment DNS names still need an egress/allowlist policy.
Prefer HTTPS for remote image references.

The plugin sends one prompt item's ordered image references and bounded text
context to the configured VLM. CLIProxyAPI chooses the provider for
`vision_model`. Review retention,
access control and data residency of the chosen provider.

## Prompt injection and failure safety

Text visible in an image is untrusted data. The prompt explicitly asks the VLM
not to follow instructions found inside the image. If any image analysis fails,
the complete request is terminated before the DeepSeek executor receives it;
partial rewrites and accidental original-image forwarding are not allowed.

`max_inflight_vision_requests` bounds callback fan-out without rejecting normal
multi-image prompts. `emergency_max_images_per_request` is a high, last-resort
unique-image abuse boundary; body/reference limits remain independent byte
boundaries. Provider retry and rate limiting remain host-owned.

Configured-limit and ABI-admission rejections emit a structured warning through
the host logger. Diagnostics are intentionally restricted to limit names,
integer sizes/counts, active limit values and configuration generation; image
references, request bodies, headers and credentials are never included.

## Opt-in plaintext trace

`trace_enabled` deliberately changes the privacy boundary for debugging. It
writes complete conversation bodies, image URLs/data URIs, prompt-group context, VLM
requests/responses, and rewritten bodies beneath
`logs/deepseek-vision-trace/`. Credential-like header and metadata fields are
still forcibly redacted, but image URLs may themselves contain signed query
parameters. Protect the mounted logs directory, enable the switch only for a
bounded reproduction, and securely remove retained bundles afterward.
