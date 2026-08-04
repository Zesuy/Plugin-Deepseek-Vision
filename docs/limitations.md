# Limitations

- The supported interception boundary is exact: `SourceFormat` must be
  `openai-response`, `request_path` must be `/v1/responses`, and the host's
  final model must be in `target_models`. Anthropic Messages and Chat
  Completions are not implemented; image-bearing requests in those protocols
  receive no plugin conversion or image-removal guarantee.
- Release artifacts currently target Linux amd64. Other platforms require a
  native CGO build and matching CLIProxyAPI host; do not copy a Linux `.so` to
  macOS or Windows.
- The plugin calls CLIProxyAPI `host.model.execute` using the OpenAI Responses
  protocol. Provider-specific protocols and transports are host concerns;
  image `file_id` inputs are not supported by the plugin rewrite contract.
- Only URL and data-URI `input_image` references in the supported Responses
  input/content and function-call-output locations are processed. Unsupported
  image references fail closed rather than being forwarded.
- The walker scans all visible `input[]` turns in one request, so images from
  retained historical turns and the current turn are converted together. A
  `previous_response_id` does not expose server-side history to the callback;
  images hidden behind that identifier cannot be inspected or rewritten.
- There is one host model call per unique image/model/language/full-prompt key.
  Duplicate work in one request is merged and successful results may be reused
  from a small TTL cache. CLIProxyAPI owns provider concurrency. The plugin
  does not provide OCR and VLM as separate models; the configured VLM returns both
  visible-text transcription and visual explanation.
- For an eligible Responses image request, malformed JSON is a 400, unsupported
  image sources are a 422, configured body/reference/image-count limits are a
  413 with a category-specific public message and a content-free `host.log`
  diagnostic, and VLM/timeout/invalid-result/rewrite failures are a 502. Failures are
  fail-closed and never forward the original image; non-eligible requests pass
  through by design.
- When the runtime is unavailable before normal discovery, targeted malformed
  or image-shaped Responses requests are conservatively terminated with 502;
  this lifecycle fallback does not terminate unrelated models.
- The response stream is not modified. Preprocessing must finish before the
  host begins delivering a stream, so VLM latency contributes to first-byte
  latency.
- The process-local cache capacity and data-URI/URL TTLs are configurable
  (defaults: 128 entries, 15 minutes, and 2 minutes). Reconfigure/restart clears
  it. It is not distributed, so another CLIProxyAPI process may repeat the analysis.
- The opt-in full-context trace is process-local and intentionally stores
  plaintext user/image/model data. It is a diagnostic capture, not an audit log,
  and must not be left enabled as normal production logging.
- `deepseek-v4-pro` is retained as a future-supported target, but its Responses
  availability currently depends on the upstream service. It is not required,
  probed, or release-tested in v0.1.0; real validation uses `deepseek-v4-flash`.
