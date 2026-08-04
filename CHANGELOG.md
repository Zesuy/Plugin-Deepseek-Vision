# Changelog

All notable release changes are documented here. Versions follow Semantic
Versioning, and release tags use the `vX.Y.Z` form.

## [0.1.1] - 2026-08-04

Initial public release.

### Added

- Native CLIProxyAPI v7 request interception for image-bearing OpenAI
  Responses requests targeting `deepseek-v4-flash`.
- Host-owned vision execution through `host.model.execute`, reusing existing
  model routing, credentials, protocol translation, transport, and retries.
- Ordered multi-image analysis per prompt item, including visible historical
  turns and array-form function-call outputs.
- Request-local deduplication and configurable process-local TTL caching of
  derived prompt-group analysis.
- Global in-flight vision backpressure, a high emergency unique-image ceiling,
  and adaptive ordered splitting only after an explicit host 413 response.
- Atomic fail-closed rewriting with a final guarantee that no `input_image`
  reaches the non-vision target model.
- A fixed downstream notice that tells DeepSeek to use the supplied visual
  analysis instead of reopening consumed attachments with `view_image`.
- Bilingual CPAMC configuration fields with defaults and validation ranges.
- Full-context opt-in trace bundles for multi-turn request diagnosis.
- Deterministic Linux amd64 packaging, checksums, contract validation, race
  tests, mock-host E2E coverage, and tag-driven GitHub Releases.

### Release boundary

- Validated host SDK: CLIProxyAPI v7.2.113.
- Published artifact target: Linux amd64.
- Release-tested DeepSeek target: `deepseek-v4-flash`.
- Chat Completions, Anthropic Messages, `file_id`-only images, and hidden
  `previous_response_id` history are outside the supported conversion boundary.

[0.1.1]: https://github.com/Zesuy/Plugin-Deepseek-Vision/releases/tag/v0.1.1
