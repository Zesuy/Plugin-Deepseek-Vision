package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestWiredLanguageReconfigureRotatesServiceAndPrompt(t *testing.T) {
	var mu sync.Mutex
	var prompts []string
	vlm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Input []struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode VLM payload: %v", err)
		} else if len(payload.Input) == 0 || len(payload.Input[0].Content) == 0 {
			t.Error("VLM payload has no prompt")
		} else {
			mu.Lock()
			prompts = append(prompts, payload.Input[0].Content[0].Text)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output_text":"Visible text: fixture\nVisual description: screen"}`))
	}))
	defer vlm.Close()
	old, had := os.LookupEnv("WIRING_LANGUAGE_KEY")
	_ = os.Setenv("WIRING_LANGUAGE_KEY", "test-key")
	defer func() {
		if had {
			_ = os.Setenv("WIRING_LANGUAGE_KEY", old)
		} else {
			_ = os.Unsetenv("WIRING_LANGUAGE_KEY")
		}
		shutdownPlugin()
	}()

	configureLanguage := func(method, language string) {
		t.Helper()
		configYAML := []byte(fmt.Sprintf(`
target_models: [deepseek-v4-flash, deepseek-v4-pro]
vision_base_url: %s/v1
vision_model: gpt-5.6-luna
vision_api_key_env: WIRING_LANGUAGE_KEY
language: %s
request_timeout_seconds: 2
per_call_timeout_seconds: 1
retry_max_attempts: 1
max_concurrency: 2
max_images_per_request: 2
max_request_bytes: 1048576
max_image_reference_bytes: 1048576
max_response_bytes: 1048576
max_result_chars: 20000
cache_size: 8
cache_ttl_seconds: 60
`, vlm.URL, language))
		raw, err := json.Marshal(lifecycleRequest{ConfigYAML: configYAML, SchemaVersion: 2})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := handleMethod(method, raw); err != nil {
			t.Fatal(err)
		}
	}
	call := func() {
		t.Helper()
		req, err := json.Marshal(pluginapi.RequestInterceptRequest{
			SourceFormat: "openai-response", Model: "deepseek-v4-flash",
			Body:     []byte(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`),
			Metadata: map[string]any{"request_path": "/v1/responses"},
		})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := handleMethod("request.intercept_after", req)
		if err != nil {
			t.Fatal(err)
		}
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
			t.Fatalf("unexpected envelope: %s", raw)
		}
		var resp pluginapi.RequestInterceptResponse
		if err := json.Unmarshal(env.Result, &resp); err != nil || resp.Terminate {
			t.Fatalf("intercept response=%#v err=%v", resp, err)
		}
	}

	configureLanguage("plugin.register", "zh-CN")
	call()
	configureLanguage("plugin.reconfigure", "en")
	call()
	mu.Lock()
	got := append([]string(nil), prompts...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("VLM calls=%d, want two generations", len(got))
	}
	if !strings.Contains(got[0], "Simplified Chinese") || !strings.Contains(got[1], "in English") {
		t.Fatalf("language prompts not rotated: %#v", got)
	}
}

func TestWiredAfterAuthUsesConfiguredVLMAndRemovesImage(t *testing.T) {
	var calls atomic.Int32
	vlm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("authorization=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output_text":"Visible text: fixture\nVisual description: screen"}`))
	}))
	defer vlm.Close()
	old, had := os.LookupEnv("WIRING_TEST_VISION_KEY")
	_ = os.Setenv("WIRING_TEST_VISION_KEY", "test-key")
	defer func() {
		if had {
			_ = os.Setenv("WIRING_TEST_VISION_KEY", old)
		} else {
			_ = os.Unsetenv("WIRING_TEST_VISION_KEY")
		}
		shutdownPlugin()
	}()
	configYAML := []byte("\n" + strings.ReplaceAll(`
target_models: [deepseek-v4-flash, deepseek-v4-pro]
vision_base_url: BASE_URL/v1
vision_model: gpt-5.6-luna
vision_api_key_env: WIRING_TEST_VISION_KEY
language: zh
request_timeout_seconds: 2
per_call_timeout_seconds: 1
retry_max_attempts: 1
max_concurrency: 2
max_images_per_request: 2
max_request_bytes: 1048576
max_image_reference_bytes: 1048576
max_response_bytes: 1048576
max_result_chars: 20000
cache_size: 0
cache_ttl_seconds: 60
`, "BASE_URL", vlm.URL))
	reg, err := json.Marshal(lifecycleRequest{ConfigYAML: configYAML, SchemaVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handleMethod("plugin.register", reg); err != nil {
		t.Fatal(err)
	}
	req, err := json.Marshal(pluginapi.RequestInterceptRequest{
		SourceFormat: "openai-response", Model: "deepseek-v4-flash",
		Body:     []byte(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`),
		Metadata: map[string]any{"request_path": "/v1/responses"},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := handleMethod("request.intercept_after", req)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("unexpected plugin envelope: %s", raw)
	}
	var resp pluginapi.RequestInterceptResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Terminate || calls.Load() != 1 || strings.Contains(string(resp.Body), "input_image") || strings.Contains(string(resp.Body), "data:image") {
		t.Fatalf("response=%#v calls=%d", resp, calls.Load())
	}
}

func TestWiredRegistrationWithoutTokenPublishesMetadataAndTerminatesImageRequests(t *testing.T) {
	old, had := os.LookupEnv("WIRING_TEST_VISION_KEY")
	_ = os.Unsetenv("WIRING_TEST_VISION_KEY")
	defer func() {
		if had {
			_ = os.Setenv("WIRING_TEST_VISION_KEY", old)
		} else {
			_ = os.Unsetenv("WIRING_TEST_VISION_KEY")
		}
		shutdownPlugin()
	}()
	configYAML := []byte(`
target_models: [deepseek-v4-flash, deepseek-v4-pro]
vision_base_url: http://127.0.0.1:1/v1
vision_model: gpt-5.6-luna
vision_api_key_env: WIRING_TEST_VISION_KEY
language: zh
request_timeout_seconds: 2
per_call_timeout_seconds: 1
retry_max_attempts: 1
max_concurrency: 2
max_images_per_request: 2
max_request_bytes: 1048576
max_image_reference_bytes: 1048576
max_response_bytes: 1048576
max_result_chars: 20000
cache_size: 0
cache_ttl_seconds: 60
`)
	reg, err := json.Marshal(lifecycleRequest{ConfigYAML: configYAML, SchemaVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	rawRegistration, err := handleMethod("plugin.register", reg)
	if err != nil {
		t.Fatalf("metadata registration failed without runtime token: %v", err)
	}
	var registrationEnvelope envelope
	if err := json.Unmarshal(rawRegistration, &registrationEnvelope); err != nil || !registrationEnvelope.OK {
		t.Fatalf("registration envelope = %s, err=%v", rawRegistration, err)
	}
	var registered registration
	if err := json.Unmarshal(registrationEnvelope.Result, &registered); err != nil {
		t.Fatal(err)
	}
	if len(registered.Metadata.ConfigFields) == 0 {
		t.Fatal("registration did not expose ConfigFields")
	}
	if _, err := handleMethod("plugin.reconfigure", reg); err == nil {
		t.Fatal("missing token reconfigure unexpectedly reported ready")
	}
	req, err := json.Marshal(pluginapi.RequestInterceptRequest{
		SourceFormat: "openai-response", Model: "deepseek-v4-flash",
		Body:     []byte(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`),
		Metadata: map[string]any{"request_path": "/v1/responses"},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := handleMethod("request.intercept_after", req)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("unexpected plugin envelope: %s", raw)
	}
	var resp pluginapi.RequestInterceptResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Terminate || resp.StatusCode != 502 || len(resp.Body) != 0 || strings.Contains(string(resp.ResponseBody), "WIRING_TEST") {
		t.Fatalf("missing-token response=%#v body=%s", resp, resp.ResponseBody)
	}
}
