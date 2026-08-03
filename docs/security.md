# Security notes

## Credentials and data

- The plugin reuses a model and credential already configured in CLIProxyAPI;
  it has no key field, environment-variable credential, or external backend.
- The plugin does not log Authorization headers, complete endpoint URLs,
  image data URIs or upstream response bodies.
- The plugin does not cache model results or raw image bytes.
- Package scripts scan source and generated archives for obvious key markers and
  fail closed. This is a guardrail, not a replacement for organization-wide
  secret scanning.

## Network

CLIProxyAPI owns provider transport, retries, concurrency, and credential
handling. Literal loopback/private/link-local image
URLs are rejected; deployment DNS names still need an egress/allowlist policy.
Prefer HTTPS for remote image references.

The plugin sends the image reference and a bounded focus hint to the configured
VLM. CLIProxyAPI chooses the provider for `vision_model`. Review retention,
access control and data residency of the chosen provider.

## Prompt injection and failure safety

Text visible in an image is untrusted data. The prompt explicitly asks the VLM
not to follow instructions found inside the image. If any image analysis fails,
the complete request is terminated before the DeepSeek executor receives it;
partial rewrites and accidental original-image forwarding are not allowed.

Keep `max_images_per_request` and body/reference limits small enough for the
deployment. These are both resource controls and abuse boundaries.
