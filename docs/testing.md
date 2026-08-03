# Testing and release verification

Run the local checks from the repository root:

```bash
go test ./...
go test -race ./...
go vet ./...
./scripts/verify-contracts.sh
./scripts/package-smoke.sh
```

The package smoke test builds with CGO, verifies `cliproxy_plugin_init`, embeds
and checks the plugin SHA-256, checks ZIP root members and scans the archive for
obvious credential material. It uses a temporary directory and removes it on
exit. The regular package command writes only to the ignored `dist/` directory.

For Docker validation, first check the rendered configuration without starting
services:

```bash
docker compose -f docker/docker-compose.example.yml config
docker build --file Dockerfile.plugin --target artifact \
  --build-arg VERSION=0.1.0 \
  --output type=local,dest=/tmp/deepseek-vision-plugin .
```

End-to-end validation uses a real CLIProxyAPI process with mock providers to
assert that `host.model.execute` performs routing, credentials, protocol
translation, and self-skip without recursion, and that a failed VLM call
results in zero downstream calls. No plugin key is required. Release acceptance
targets `deepseek-v4-flash`; `deepseek-v4-pro` remains future-supported only.
