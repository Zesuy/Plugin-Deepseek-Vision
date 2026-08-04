# deepseek-vision

> 🌐 Documentation: [简体中文](README.md) | **English**

`deepseek-vision` is a CLIProxyAPI v7 native plugin that makes image-bearing
OpenAI Responses requests usable with DeepSeek models that do not accept
images. After CLIProxyAPI resolves an alias, the plugin handles only
`/v1/responses` requests whose final model is `deepseek-v4-flash` by default;
additional targets require explicit configuration.
It sends each image once to an OpenAI-compatible VLM
(default: `gpt-5.6-luna`), then replaces the image with a text visual analysis
before the DeepSeek upstream request is sent.

The currently available and release-tested DeepSeek target is
`deepseek-v4-flash`. `deepseek-v4-pro` is not required for this release, is not
probed by release acceptance, and remains pending upstream Responses availability.

The plugin is strict for eligible Responses image requests: if the VLM call
fails, the original request is stopped with HTTP 502 and the unprocessed image
is not forwarded. It never implements a separate executor or response-stream
transformer; unsupported protocols retain the host's normal handling.

The support boundary is deliberately narrow: interception requires
`SourceFormat == "openai-response"`, metadata `request_path == "/v1/responses"`,
and a final model listed in `target_models`. Anthropic Messages and Chat
Completions image conversion are not implemented; those source protocols are
outside this contract and are left to the host's normal handling.

## Quick start

1. Build the Linux/amd64 package on a Linux/amd64 host (Go 1.26, CGO and a C
   compiler are required):

   ```bash
   VERSION=0.1.0 ./scripts/package.sh
   ./scripts/checksum.sh
   ```

2. For manual installation, keep exactly one unversioned library under the
   CLIProxyAPI plugin root. Remove stale versioned candidates before extracting:

   ```bash
   plugin_dir=plugins/linux/amd64
   mkdir -p "$plugin_dir"
   find "$plugin_dir" -maxdepth 1 -type f -name 'deepseek-vision-v*.so' -delete
   rm -f "$plugin_dir/deepseek-vision.so" "$plugin_dir/checksums.txt"
   unzip -o dist/deepseek-vision_0.1.0_linux_amd64.zip -d "$plugin_dir"
   test "$(find "$plugin_dir" -maxdepth 1 -type f -name 'deepseek-vision*.so' | wc -l)" -eq 1
   (cd "$plugin_dir" && sha256sum -c checksums.txt)
   ```

   The active path must be `plugins/linux/amd64/deepseek-vision.so`; do not
   leave `deepseek-vision-v*.so` files beside it. Store-managed installations
   instead use `deepseek-vision-vX.Y.Z.so` and pin the same `X.Y.Z` under
   `plugins.configs.deepseek-vision.store.version`. See
   [`docs/installation.md`](docs/installation.md) for active-path checks and
   upgrade/rollback procedures.

3. Copy [`config.example.yaml`](config.example.yaml) to the CLIProxyAPI
   configuration location. The plugin reuses a vision-capable model and
   credentials already configured in CLIProxyAPI, so it needs no separate
   endpoint, API key, or Docker environment variable.

4. Start CLIProxyAPI with its plugin root set to `./plugins`. A ready-to-edit
   Docker Compose example is in [`docker/docker-compose.example.yml`](docker/docker-compose.example.yml).

The default vision model is `gpt-5.6-luna`; set `vision_model` to any
vision-capable model available through CLIProxyAPI. CLIProxyAPI owns provider
protocol translation, routing, credentials, transport, and retry policy.

## Build and release

`scripts/package.sh` produces a deterministic ZIP with both
`deepseek-vision.so` and an embedded `checksums.txt` at the archive root. The
companion `dist/checksums.txt` records the archive SHA-256. `scripts/package-smoke.sh`
builds in a temporary directory and verifies ZIP members, permissions,
checksums, ABI symbols and obvious credential markers without leaving files in
the repository. Python's `zipfile` module is used, so the host does not need a
`zip` executable.

For another host, use the BuildKit artifact stage:

```bash
docker build --file Dockerfile.plugin --build-arg VERSION=0.1.0 \
  --output type=local,dest=./plugins/linux/amd64 .
```

GitHub Actions runs tests, race detection, vet, contract checks, C ABI builds
and package verification on every pull request. A `v*.*.*` tag publishes the
Linux/amd64 ZIP and checksums; no VLM or upstream API key is present in CI or
release assets.

## Runtime behavior

- Gate: `SourceFormat == "openai-response"`, metadata path exactly
  `/v1/responses`, and final model in the explicitly configured target list
  (the default list contains only `deepseek-v4-flash`).
- `/v1/responses/compact`, other APIs and other models pass through unchanged.
- The visible `input[]` array is scanned in full. Images in earlier
  conversation turns retained in the request (including images left during a
  Codex/Luna turn) and images in the current turn are converted together; the
  original image blocks and their references are removed only after every
  analysis succeeds. A `previous_response_id` is only an identifier:
  server-side history hidden behind it is not included in this callback and
  cannot be fetched or rewritten by the plugin.
- All images in one content/function-output prompt item are sent to Luna in one
  ordered multi-image host call. Their positions become numbered text markers,
  and one joint analysis is appended to that prompt item.
- Identical ordered image-group/model/language/full-prompt work is deduplicated
  within one request and reused across requests through a configurable TTL cache. Defaults
  are 128 entries, 15 minutes for data URIs, and 2 minutes for URLs;
  reconfigure starts a fresh cache.
- Host vision calls use a configurable global in-flight bound; excess prompt
  groups queue. A high emergency unique-image ceiling and body/reference/response
  sizes remain bounded together with the total preprocessing deadline. Explicit
  upstream 413 responses split multi-image groups in order.
- Invalid or incomplete configuration edits never unregister the plugin. The
  last known-good runtime remains active; if there is no valid runtime yet,
  targeted image requests fail closed while the configuration UI stays
  available.
- For an eligible Responses request, malformed JSON returns 400, unsupported
  image references return 422, configured size limits return 413 with the
  rejected limit category, and VLM,
  timeout, or rewrite failures return 502. Failures terminate the request and
  never fall back to forwarding an original image; non-eligible protocols and
  models retain pass-through behavior.
- Every 413 emits one content-free warning through the host's `host.log`, tied
  to the request ID when available. It records only the limit kind, actual and
  maximum values, active size settings, and configuration generation.
- For difficult multi-turn diagnosis, `trace_enabled: true` writes a plaintext
  event index and per-request bundle under `logs/deepseek-vision-trace/`. It
  includes the exact inbound body, image URLs/data URIs, prompt groups/context, cache
  plan, VLM requests/responses, and rewritten body. Credential headers and
  metadata are still redacted. Treat this directory as a full copy of user
  data and enable it only temporarily.
- If the runtime is unavailable before normal discovery can run, a targeted
  Responses request with malformed or image-shaped input is conservatively
  terminated with 502; unrelated models still pass through.
- `stream: true` is supported because preprocessing finishes before the host
  starts the response stream.

See [`docs/contracts.md`](docs/contracts.md),
[`docs/configuration.md`](docs/configuration.md),
[`docs/installation.md`](docs/installation.md) and
[`docs/security.md`](docs/security.md) for the full operational contract.

## Development

```bash
go test ./...
go test -race ./...
go vet ./...
./scripts/verify-contracts.sh
./scripts/package-smoke.sh
```

The repository intentionally contains no API keys, captured user payloads or
generated dynamic libraries.

## License

This project is licensed under the MIT License. See [`LICENSE`](LICENSE).

Copyright (c) 2026 Zesuy
