# Security notes

## Credentials and data

- Prefer host mode, which reuses a model and credential already configured in
  CLIProxyAPI and stores no additional VLM key in plugin configuration.
- External mode reads its VLM key from `DEEPSEEK_VISION_API_KEY` (or the
  configured environment variable), never from a plaintext plugin field.
- CPAMC v7.2.113 has no secret/password plugin field type. Never paste a bare
  API key into `vision_api_key_env`; that field accepts an environment variable
  name only.
- The plugin does not log Authorization headers, complete endpoint URLs,
  image data URIs or upstream response bodies.
- Cache entries contain only visual-analysis text and metadata; raw image bytes
  are not retained.
- Package scripts scan source and generated archives for obvious key markers and
  fail closed. This is a guardrail, not a replacement for organization-wide
  secret scanning.

## Network

In external mode, the VLM client uses an owned HTTP client with a total timeout,
TLS handshake and response-header deadlines, redirects disabled, response-size
limits and finite retry/backoff. In host mode, CLIProxyAPI owns provider
transport and credential handling. Literal loopback/private/link-local image
URLs are rejected; deployment DNS names still need an egress/allowlist policy.
Prefer HTTPS for non-local external endpoints.

The plugin sends the image reference and a bounded focus hint to the configured
VLM. In host mode, CLIProxyAPI chooses the provider for `vision_model`; in
external mode, do not point `vision_base_url` at an untrusted service. Review
retention, access control and data residency of the chosen provider.

## Prompt injection and failure safety

Text visible in an image is untrusted data. The prompt explicitly asks the VLM
not to follow instructions found inside the image. If any image analysis fails,
the complete request is terminated before the DeepSeek executor receives it;
partial rewrites and accidental original-image forwarding are not allowed.

Keep `max_images_per_request`, body/reference limits and `max_concurrency`
small enough for the deployment. These are both resource controls and abuse
boundaries. If external mode is used, rotate its key after development or if it
may have appeared in a shell history, process listing or external logs.
