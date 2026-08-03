#!/usr/bin/env bash
# End-to-end test through a real CLIProxyAPI process and its c-shared loader.
# No external service, Docker, or credentials are required.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
CLI_ROOT=${CLIPROXY_ROOT:-}
TIMEOUT=${E2E_TIMEOUT_SECONDS:-180}
TMP=$(mktemp -d "${TMPDIR:-/tmp}/deepseek-vision-host-e2e.XXXXXX")
PLUGIN_DIR="$TMP/plugins"
mkdir -p "$PLUGIN_DIR/linux/amd64" "$TMP/auth"

HOST_PID=""
VLM_PID=""
UPSTREAM_PID=""
cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  for pid in "$HOST_PID" "$VLM_PID" "$UPSTREAM_PID"; do
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
    fi
  done
  for pid in "$HOST_PID" "$VLM_PID" "$UPSTREAM_PID"; do
    if [[ -n "$pid" ]]; then
      wait "$pid" 2>/dev/null || true
    fi
  done
  if [[ "${KEEP_E2E_TMP:-}" == "1" ]]; then
    echo "host-e2e temporary directory preserved: $TMP" >&2
  else
    rm -rf "$TMP"
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

if ! command -v curl >/dev/null || ! command -v python3 >/dev/null; then
  echo "host-e2e requires curl and python3" >&2
  exit 2
fi
if [[ -z "$CLI_ROOT" || ! -d "$CLI_ROOT" ]]; then
  echo "set CLIPROXY_ROOT to a CLIProxyAPI checkout (v7.2.113)" >&2
  exit 2
fi

free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

HOST_PORT=$(free_port)
VLM_PORT=$(free_port)
UPSTREAM_PORT=$(free_port)
VLM_STATE="$TMP/vlm-state.json"
UPSTREAM_STATE="$TMP/upstream-state.json"
VLM_MODE="$TMP/vlm-mode"
printf 'ok\n' >"$VLM_MODE"

python3 "$ROOT/test/e2e/mock_openai.py" --port "$VLM_PORT" --role vlm --state "$VLM_STATE" --mode-file "$VLM_MODE" >"$TMP/vlm.log" 2>&1 &
VLM_PID=$!
python3 "$ROOT/test/e2e/mock_openai.py" --port "$UPSTREAM_PORT" --role upstream --state "$UPSTREAM_STATE" >"$TMP/upstream.log" 2>&1 &
UPSTREAM_PID=$!

deadline=$((SECONDS + TIMEOUT))
until curl -fsS --max-time 1 "http://127.0.0.1:$VLM_PORT/healthz" >/dev/null 2>&1 && \
      curl -fsS --max-time 1 "http://127.0.0.1:$UPSTREAM_PORT/healthz" >/dev/null 2>&1; do
  (( SECONDS < deadline )) || { echo "mock server startup timed out" >&2; exit 1; }
  sleep 0.1
done

echo "building plugin and CLIProxyAPI host"
CGO_ENABLED=1 GOTOOLCHAIN=auto go build -buildmode=c-shared -trimpath \
  -ldflags='-s -w -X main.pluginVersion=0.1.0-host-e2e' \
  -o "$PLUGIN_DIR/linux/amd64/deepseek-vision-v0.1.0.so" "$ROOT"
(cd "$CLI_ROOT" && CGO_ENABLED=1 GOTOOLCHAIN=auto go build -trimpath -o "$TMP/cliproxy" ./cmd/server)

CONFIG="$TMP/config.yaml"
python3 - "$CONFIG" "$PLUGIN_DIR" "$HOST_PORT" "$VLM_PORT" "$UPSTREAM_PORT" <<'PY'
import pathlib
import sys

path, plugin_dir, host_port, vlm_port, upstream_port = sys.argv[1:]
text = f'''host: 127.0.0.1
port: {host_port}
debug: true
auth-dir: "{pathlib.Path(path).parent / 'auth'}"
api-keys:
  - client-key
remote-management:
  allow-remote: false
  secret-key: management-key
  disable-control-panel: true
plugins:
  enabled: true
  dir: "plugins"
  configs:
    deepseek-vision:
      enabled: true
      priority: 100
      store:
        source: "host-e2e"
        version: "0.1.0"
      target_models: [deepseek-v4-flash]
      vision_model: "gpt-5.6-luna"
      language: "en"
      request_timeout_seconds: 10
      max_images_per_request: 4
      max_request_bytes: 20971520
      max_image_reference_bytes: 1048576
      max_response_bytes: 1048576
      max_result_chars: 2000
      analysis_cache_size: 16
      analysis_cache_ttl_seconds: 60
      analysis_url_cache_ttl_seconds: 30
openai-compatibility:
  - name: vision-e2e
    base-url: "http://127.0.0.1:{vlm_port}/v1"
    api-key-entries:
      - api-key: vision-host-key
    models:
      - name: gpt-5.6-luna
        alias: gpt-5.6-luna
  - name: host-e2e
    base-url: "http://127.0.0.1:{upstream_port}/v1"
    api-key-entries:
      - api-key: upstream-key
    models:
      - name: deepseek-v4-flash
        alias: deepseek-v4-flash
        input-modalities: [text, image]
      - name: deepseek-v4-flash
        alias: deepseek-flash-alias
        input-modalities: [text, image]
      - name: other-model
        alias: other-model
        input-modalities: [text, image]
'''
pathlib.Path(path).write_text(text, encoding="utf-8")
PY

(cd "$TMP" && "$TMP/cliproxy" -config "$CONFIG" -local-model >"$TMP/host.log" 2>&1) &
HOST_PID=$!
BASE="http://127.0.0.1:$HOST_PORT"

deadline=$((SECONDS + TIMEOUT))
until curl -fsS --max-time 2 -H 'Authorization: Bearer client-key' "$BASE/v1/models" >/dev/null 2>&1; do
  if ! kill -0 "$HOST_PID" 2>/dev/null; then
    echo "CLIProxyAPI exited during startup" >&2
    sed -n '1,160p' "$TMP/host.log" >&2 || true
    exit 1
  fi
  (( SECONDS < deadline )) || { echo "CLIProxyAPI startup timed out" >&2; exit 1; }
  sleep 0.2
done

# The management secret comes from config.yaml. Keep the process alive beyond
# CLIProxyAPI's short-lived password-flag mode so slow VLM calls cannot race a
# self-termination regression.
sleep 11
if ! kill -0 "$HOST_PID" 2>/dev/null; then
  echo "CLIProxyAPI exited before the post-startup liveness check" >&2
  sed -n '1,160p' "$TMP/host.log" >&2 || true
  exit 1
fi

mgmt() {
  curl -fsS --max-time 4 -H 'Authorization: Bearer management-key' "$@"
}
state_json() {
  curl -fsS --max-time 3 "$1/state"
}
count_role() {
  python3 -c 'import json,sys; print(len(json.load(sys.stdin).get("requests", [])))'
}
VLM_BEFORE=$(state_json "http://127.0.0.1:$VLM_PORT" | count_role)
UPSTREAM_BEFORE=$(state_json "http://127.0.0.1:$UPSTREAM_PORT" | count_role)

mgmt "$BASE/v0/management/plugins" >"$TMP/plugins.json"
python3 - "$TMP/plugins.json" <<'PY'
import json, sys
payload = json.load(open(sys.argv[1], encoding="utf-8"))
assert payload["plugins_enabled"] is True
items = {x["id"]: x for x in payload["plugins"]}
item = items["deepseek-vision"]
assert item["registered"] is True, item
assert item["effective_enabled"] is True, item
assert item["configured"] is True, item
fields = {field["name"]: field["type"] for field in item["config_fields"]}
assert len(fields) == 5, fields
assert fields["vision_model"] == "string", fields
assert fields["language"] == "enum", fields
assert fields["analysis_cache_size"] == "integer", fields
assert fields["analysis_cache_ttl_seconds"] == "integer", fields
assert fields["analysis_url_cache_ttl_seconds"] == "integer", fields
PY
mgmt "$BASE/v0/management/plugins/deepseek-vision/config" >"$TMP/plugin-config.json"
python3 - "$TMP/plugin-config.json" <<'PY'
import json, sys
payload = json.load(open(sys.argv[1], encoding="utf-8"))
assert payload.get("store", {}).get("source") == "host-e2e", payload
assert payload.get("store", {}).get("version") == "0.1.0", payload
assert payload.get("analysis_cache_size") == 16, payload
assert payload.get("analysis_cache_ttl_seconds") == 60, payload
assert payload.get("analysis_url_cache_ttl_seconds") == 30, payload
PY
echo "plugin registered/effective and store metadata accepted"

# A user can leave an incomplete edit without making the plugin disappear from
# the management snapshot. The last known-good runtime remains active.
mgmt -X PATCH -H 'Content-Type: application/json' \
  -d '{"vision_model":null}' \
  "$BASE/v0/management/plugins/deepseek-vision/config" >/dev/null
mgmt "$BASE/v0/management/plugins" | python3 -c '
import json,sys
item=next(x for x in json.load(sys.stdin)["plugins"] if x["id"]=="deepseek-vision")
assert item["registered"] is True and item["effective_enabled"] is True, item
'
mgmt -X PATCH -H 'Content-Type: application/json' \
  -d '{"vision_model":"gpt-5.6-luna"}' \
  "$BASE/v0/management/plugins/deepseek-vision/config" >/dev/null
echo "incomplete edit preserved registration and last known-good runtime"

post() {
  local path=$1
  local body=$2
  local out=$3
  curl -sS --max-time 20 -o "$out" -w '%{http_code}' \
    -H 'Authorization: Bearer client-key' -H 'Content-Type: application/json' \
    -X POST "$BASE$path" --data-binary "$body"
}

make_request() {
  local model=$1 stream=$2 suffix=$3
  python3 - "$model" "$stream" "$suffix" <<'PY'
import base64, json, sys
model, stream, suffix = sys.argv[1], sys.argv[2] == "true", sys.argv[3]
image = "data:image/png;base64," + base64.b64encode(("e2e-" + suffix).encode()).decode()
print(json.dumps({"model": model, "input": [{"role": "user", "content": [
    {"type": "input_text", "text": "Describe this " + suffix},
    {"type": "input_image", "image_url": image}
]}], "stream": stream}, separators=(",", ":")) )
PY
}

assert_success() {
  local label=$1 path=$2 body=$3 expected_vlm=$4 expected_upstream=$5
  local out="$TMP/$label.out"
  local code
  code=$(post "$path" "$body" "$out")
  [[ "$code" == "200" ]] || { echo "$label failed HTTP $code: $(head -c 500 "$out")" >&2; exit 1; }
  local vlm_count upstream_count
  vlm_count=$(state_json "http://127.0.0.1:$VLM_PORT" | count_role)
  upstream_count=$(state_json "http://127.0.0.1:$UPSTREAM_PORT" | count_role)
  [[ "$vlm_count" == "$expected_vlm" ]] || { echo "$label VLM count=$vlm_count want=$expected_vlm" >&2; exit 1; }
  [[ "$upstream_count" == "$expected_upstream" ]] || { echo "$label upstream count=$upstream_count want=$expected_upstream" >&2; exit 1; }
}

assert_last_upstream_rewritten() {
  python3 - "$UPSTREAM_STATE" <<'PY'
import json, sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
body = state["requests"][-1].get("body") or {}
raw = json.dumps(body, ensure_ascii=False)
assert "input_image" not in raw, raw[:1000]
assert "data:image/" not in raw, raw[:1000]
assert "Visual analysis" in raw or "Visible text" in raw, raw[:1000]
PY
}

body=$(make_request deepseek-v4-flash false direct)
assert_success direct /v1/responses "$body" "$((VLM_BEFORE + 1))" "$((UPSTREAM_BEFORE + 1))"
assert_last_upstream_rewritten

# The identical request is rewritten from the generation-local cache and does
# not make another VLM call.
assert_success cache-hit /v1/responses "$body" "$((VLM_BEFORE + 1))" "$((UPSTREAM_BEFORE + 2))"
assert_last_upstream_rewritten

body=$(make_request deepseek-flash-alias false alias)
assert_success alias /v1/responses "$body" "$((VLM_BEFORE + 2))" "$((UPSTREAM_BEFORE + 3))"
assert_last_upstream_rewritten

body=$(make_request deepseek-v4-flash true stream)
assert_success stream /v1/responses "$body" "$((VLM_BEFORE + 3))" "$((UPSTREAM_BEFORE + 4))"
grep -q 'data:' "$TMP/stream.out" || { echo "stream response did not contain SSE" >&2; exit 1; }

multi_body=$(python3 - <<'PY'
import base64, json
def image(n):
    return "data:image/png;base64," + base64.b64encode(("multi-" + str(n)).encode()).decode()
print(json.dumps({"model":"deepseek-v4-flash","input":[
 {"role":"user","content":[{"type":"input_image","image_url":image(1)},{"type":"input_image","image_url":image(2)}]},
 {"type":"function_call_output","call_id":"e2e","output":[{"type":"input_image","image_url":image(3)}]}
],"stream":False}, separators=(",", ":")))
PY
)
assert_success multi /v1/responses "$multi_body" "$((VLM_BEFORE + 6))" "$((UPSTREAM_BEFORE + 5))"
assert_last_upstream_rewritten

no_image='{"model":"deepseek-v4-flash","input":[{"role":"user","content":[{"type":"input_text","text":"plain"}]}],"stream":false}'
assert_success no-image /v1/responses "$no_image" "$((VLM_BEFORE + 6))" "$((UPSTREAM_BEFORE + 6))"

non_target=$(make_request other-model false non-target)
assert_success non-target /v1/responses "$non_target" "$((VLM_BEFORE + 6))" "$((UPSTREAM_BEFORE + 7))"

compact=$(make_request deepseek-v4-flash false compact)
code=$(post /v1/responses/compact "$compact" "$TMP/compact.out")
[[ "$code" == "200" ]] || { echo "compact failed HTTP $code" >&2; exit 1; }
vlm_count=$(state_json "http://127.0.0.1:$VLM_PORT" | count_role)
upstream_count=$(state_json "http://127.0.0.1:$UPSTREAM_PORT" | count_role)
[[ "$vlm_count" == "$((VLM_BEFORE + 6))" ]] || { echo "compact was not bypassed" >&2; exit 1; }
[[ "$upstream_count" == "$((UPSTREAM_BEFORE + 8))" ]] || { echo "compact upstream count=$upstream_count" >&2; exit 1; }

printf 'fail\n' >"$VLM_MODE"
failure=$(make_request deepseek-v4-flash false vlm-failure)
code=$(post /v1/responses "$failure" "$TMP/failure.out")
[[ "$code" == "502" ]] || { echo "VLM failure HTTP $code, want 502" >&2; exit 1; }
grep -q 'vision_preprocess_error' "$TMP/failure.out" || { echo "failure response was not sanitized" >&2; exit 1; }
[[ "$(state_json "http://127.0.0.1:$UPSTREAM_PORT" | count_role)" == "$((UPSTREAM_BEFORE + 8))" ]] || { echo "upstream called after VLM failure" >&2; exit 1; }
printf 'ok\n' >"$VLM_MODE"

unsupported='{"model":"deepseek-v4-flash","input":[{"role":"user","content":[{"type":"input_image","file_id":"file_unsupported"}]}],"stream":false}'
code=$(post /v1/responses "$unsupported" "$TMP/unsupported.out")
[[ "$code" == "422" ]] || { echo "unsupported image source HTTP $code, want 422" >&2; exit 1; }
[[ "$(state_json "http://127.0.0.1:$UPSTREAM_PORT" | count_role)" == "$((UPSTREAM_BEFORE + 8))" ]] || { echo "upstream called for unsupported image" >&2; exit 1; }

mgmt -X PATCH -H 'Content-Type: application/json' -d '{"enabled":false}' "$BASE/v0/management/plugins/deepseek-vision/enabled" >/dev/null
deadline=$((SECONDS + 20))
until [[ "$(mgmt "$BASE/v0/management/plugins" | python3 -c 'import json,sys; print(next(x for x in json.load(sys.stdin)["plugins"] if x["id"]=="deepseek-vision")["effective_enabled"])')" == "False" ]]; do
  (( SECONDS < deadline )) || { echo "plugin disable did not settle" >&2; exit 1; }
  sleep 0.2
done
disabled=$(make_request deepseek-v4-flash false disabled)
assert_success disabled /v1/responses "$disabled" "$((VLM_BEFORE + 7))" "$((UPSTREAM_BEFORE + 9))"
mgmt -X PATCH -H 'Content-Type: application/json' -d '{"enabled":true}' "$BASE/v0/management/plugins/deepseek-vision/enabled" >/dev/null
deadline=$((SECONDS + 20))
until [[ "$(mgmt "$BASE/v0/management/plugins" | python3 -c 'import json,sys; print(next(x for x in json.load(sys.stdin)["plugins"] if x["id"]=="deepseek-vision")["effective_enabled"])')" == "True" ]]; do
  (( SECONDS < deadline )) || { echo "plugin re-enable did not settle" >&2; exit 1; }
  sleep 0.2
done
reloaded=$(make_request deepseek-v4-flash false reloaded)
assert_success reloaded /v1/responses "$reloaded" "$((VLM_BEFORE + 8))" "$((UPSTREAM_BEFORE + 10))"

if grep -E 'fixture-vlm-key|upstream-key|data:image/' "$TMP/host.log" >/dev/null 2>&1; then
  echo "sensitive credential or image reference leaked to host log" >&2
  exit 1
fi

echo "host process/dynamic-loader E2E passed"
echo "covered: registration, effective status, store/cache metadata, cache hit, direct/alias/stream, content/output/multi-image rewrite, compact/non-target/no-image bypass, VLM fail-closed 502, unsupported 422, disable/re-enable reload, log redaction"
echo "bounded gaps: management delete/unload and 413 oversized callback are not asserted; host management delete removes the fixture library and cannot be safely restored within one process run"
