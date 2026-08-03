# Installation and operations

## Native installation

The plugin host selects dynamic libraries from
`<plugin-root>/<GOOS>/<GOARCH>`. For Linux amd64 there are two supported
installation modes. Choose one mode per plugin ID; do not mix an unversioned
file with versioned candidates.

### Manual mode (one unversioned file)

Manual mode is intentionally unambiguous: the directory contains exactly one
`deepseek-vision.so` and no `deepseek-vision-v*.so` files.

```bash
VERSION=0.1.0 ./scripts/package.sh
./scripts/checksum.sh
plugin_dir=plugins/linux/amd64
mkdir -p "$plugin_dir"
# Remove stale candidates for this plugin ID before installing the new file.
find "$plugin_dir" -maxdepth 1 -type f -name 'deepseek-vision-v*.so' -delete
rm -f "$plugin_dir/deepseek-vision.so" "$plugin_dir/checksums.txt"
unzip -o dist/deepseek-vision_0.1.0_linux_amd64.zip -d "$plugin_dir"
test -f "$plugin_dir/deepseek-vision.so"
test "$(find "$plugin_dir" -maxdepth 1 -type f -name 'deepseek-vision*.so' | wc -l)" -eq 1
(cd "$plugin_dir" && sha256sum -c checksums.txt)
```

Restart CLIProxyAPI after replacing the library. Verify that the management
API reports the unversioned active path:

```bash
curl -fsS -H 'Authorization: Bearer <management-key>' \
  http://127.0.0.1:8085/v0/management/plugins \
  | jq '.plugins[] | select(.id == "deepseek-vision") | {path, registered, effective_enabled, metadata}'
```

The active `path` must end in `/linux/amd64/deepseek-vision.so` and
`metadata.version` must match the package version. The host plugin ID remains
`deepseek-vision`.

### Store/versioned mode (pinned version)

Store mode uses a versioned filename, for example
`deepseek-vision-v0.1.0.so`, and pins the host selection with `store.version`.
The store source may be the built-in registry or an explicitly configured
source; the source value below is illustrative.

```yaml
plugins:
  enabled: true
  configs:
    deepseek-vision:
      enabled: true
      priority: 100
      store:
        source: <configured-store-source>
        version: "0.1.0"
```

Install the versioned asset through the CLIProxyAPI plugin store (preferred).
When managing the store asset outside the UI, rename the ZIP's unversioned
payload while installing it:

```bash
plugin_dir=plugins/linux/amd64
tmp_dir=$(mktemp -d)
unzip -q dist/deepseek-vision_0.1.0_linux_amd64.zip -d "$tmp_dir"
(cd "$tmp_dir" && sha256sum -c checksums.txt)
rm -f "$plugin_dir/deepseek-vision.so" "$plugin_dir"/deepseek-vision-v*.so
install -m 0755 "$tmp_dir/deepseek-vision.so" "$plugin_dir/deepseek-vision-v0.1.0.so"
rm -rf "$tmp_dir"
```

Keep `store.version` equal to the filename version (the host accepts either
`0.1.0` or `v0.1.0` in configuration and normalizes it). Do not leave an
unversioned `deepseek-vision.so` beside a pinned version.

After changing `store.version`, restart or trigger the host's plugin reload and
verify both the selected path and the configured pin:

```bash
curl -fsS -H 'Authorization: Bearer <management-key>' \
  http://127.0.0.1:8085/v0/management/plugins \
  | jq '.plugins[] | select(.id == "deepseek-vision") | {path, registered, effective_enabled, metadata}'
curl -fsS -H 'Authorization: Bearer <management-key>' \
  http://127.0.0.1:8085/v0/management/plugins/deepseek-vision/config \
  | jq '{store, enabled, priority}'
```

The active path must end in the pinned
`/linux/amd64/deepseek-vision-v0.1.0.so`, the metadata version must be
`0.1.0`, and the config response must show `store.version: "0.1.0"`.

Copy `config.example.yaml` into the CLIProxyAPI configuration directory. Image
analysis uses a vision-capable model already configured in CLIProxyAPI, so the
plugin needs no separate key or endpoint. Restart CLIProxyAPI after changing a
library; configuration changes can be applied through lifecycle reconfigure.

## Docker installation

The root `Dockerfile.plugin` has a scratch artifact stage. BuildKit can export
the library directly into the host plugin tree:

```bash
mkdir -p plugins/linux/amd64
docker build --file Dockerfile.plugin --target artifact \
  --build-arg VERSION=0.1.0 \
  --output type=local,dest=./plugins/linux/amd64 .
```

This requires a running Docker daemon and network access to the Go base image
and module proxy. It does not start CLIProxyAPI or send a VLM request. The
checked-in `.dockerignore` excludes credentials, runtime data, local workspaces,
and generated artifacts from the build context; do not bypass it with a broader
context directory. The Compose example follows the upstream v7 mounts
(`config.yaml`, auth home,
logs and plugin root):

```bash
docker compose -f docker/docker-compose.example.yml config
docker compose -f docker/docker-compose.example.yml up -d cli-proxy-api
```

The plugin root defaults to `./plugins`; override it with
`CLI_PROXY_PLUGIN_PATH=/absolute/path/to/plugins`. The example maps the config
file read-only and does not add a plugin credential environment variable.

## Upgrade and rollback

1. Build and verify the new version in a separate temporary directory.
2. In manual mode, stop/restart CLIProxyAPI, remove every old
   `deepseek-vision-v*.so`, replace the single unversioned
   `deepseek-vision.so` and checksum, then verify the active path.
3. In store mode, install `deepseek-vision-vX.Y.Z.so`, change
   `plugins.configs.deepseek-vision.store.version` to `X.Y.Z`, reload the host,
   and verify that both the active path and `metadata.version` changed to that
   version. Keep only the pinned version selected by the host.
4. To roll back manual mode, repeat the manual procedure with the previous
   archive. To roll back store mode, restore the previous versioned artifact
   (reinstall it if the host cleaned it up), set `store.version` back to the
   previous version, reload, and verify the path/version pair.

Never overwrite a known-good library with an unverified build. Keep prior
archives outside the repository so rollback does not depend on the network.
