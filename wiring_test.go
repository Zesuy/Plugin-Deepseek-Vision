package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestWiredLanguageReconfigureRotatesHostPrompt(t *testing.T) {
	originalExecute := hostVisionExecute
	defer func() {
		hostVisionExecute = originalExecute
		shutdownPlugin()
	}()
	var mu sync.Mutex
	var prompts []string
	hostVisionExecute = func(_ context.Context, request pluginapi.HostModelExecutionRequest, _ string) (pluginapi.HostModelExecutionResponse, error) {
		var payload requestPayloadForTest
		if err := json.Unmarshal(request.Body, &payload); err != nil {
			return pluginapi.HostModelExecutionResponse{}, err
		}
		mu.Lock()
		prompts = append(prompts, payload.Input[0].Content[0].Text)
		mu.Unlock()
		return pluginapi.HostModelExecutionResponse{
			StatusCode: http.StatusOK,
			Body:       []byte(`{"output_text":"Visible text: fixture\nVisual description: screen"}`),
		}, nil
	}
	configureLanguage := func(method, language string) {
		t.Helper()
		configYAML := []byte(fmt.Sprintf(`
target_models: [deepseek-v4-flash, deepseek-v4-pro]
vision_model: gpt-5.6-luna
language: %s
request_timeout_seconds: 2
emergency_max_images_per_request: 256
max_inflight_vision_requests: 2
max_request_bytes: 1048576
max_image_reference_bytes: 1048576
max_response_bytes: 1048576
max_result_chars: 20000
`, language))
		raw, _ := json.Marshal(lifecycleRequest{ConfigYAML: configYAML, SchemaVersion: 2})
		if _, err := handleMethod(method, raw); err != nil {
			t.Fatal(err)
		}
	}
	call := func(image string) {
		t.Helper()
		req, _ := json.Marshal(requestInterceptRPC{
			RequestInterceptRequest: pluginapi.RequestInterceptRequest{
				SourceFormat: "openai-response",
				Model:        "deepseek-v4-flash",
				Body:         []byte(fmt.Sprintf(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,%s"}]}]}`, image)),
				Metadata:     map[string]any{"request_path": "/v1/responses"},
			},
			HostCallbackID: "callback-language",
		})
		raw, err := handleMethod(pluginabi.MethodRequestInterceptAfter, req)
		if err != nil {
			t.Fatal(err)
		}
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
			t.Fatalf("envelope=%s err=%v", raw, err)
		}
		var resp pluginapi.RequestInterceptResponse
		if err := json.Unmarshal(env.Result, &resp); err != nil || resp.Terminate {
			t.Fatalf("response=%#v err=%v", resp, err)
		}
	}

	configureLanguage(pluginabi.MethodPluginRegister, "zh-CN")
	call("AAAA")
	invalid, _ := json.Marshal(lifecycleRequest{SchemaVersion: 2, ConfigYAML: []byte("vision_model: [\n")})
	if _, err := handleMethod(pluginabi.MethodPluginReconfigure, invalid); err != nil {
		t.Fatalf("invalid edit removed last known-good runtime: %v", err)
	}
	call("AAAB")
	configureLanguage(pluginabi.MethodPluginReconfigure, "en")
	call("AAAC")
	mu.Lock()
	got := append([]string(nil), prompts...)
	mu.Unlock()
	if len(got) != 3 || !strings.Contains(got[0], "Simplified Chinese") || !strings.Contains(got[1], "Simplified Chinese") || !strings.Contains(got[2], "in English") {
		t.Fatalf("language prompts = %#v", got)
	}
}

type requestPayloadForTest struct {
	Input []struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"input"`
}

func TestWiredHostExecutionUsesConfiguredModelAndCallback(t *testing.T) {
	originalExecute := hostVisionExecute
	defer func() {
		hostVisionExecute = originalExecute
		shutdownPlugin()
	}()
	var got pluginapi.HostModelExecutionRequest
	var callbackID string
	hostVisionExecute = func(_ context.Context, request pluginapi.HostModelExecutionRequest, id string) (pluginapi.HostModelExecutionResponse, error) {
		got, callbackID = request, id
		return pluginapi.HostModelExecutionResponse{
			StatusCode: http.StatusOK,
			Body:       []byte(`{"output_text":"Visible text: fixture\nVisual description: screen"}`),
		}, nil
	}
	configYAML := []byte(`
target_models: [deepseek-v4-flash]
vision_model: host-vision-model
language: auto
request_timeout_seconds: 2
emergency_max_images_per_request: 256
max_inflight_vision_requests: 2
max_request_bytes: 1048576
max_image_reference_bytes: 1048576
max_response_bytes: 1048576
max_result_chars: 20000
`)
	registrationRequest, _ := json.Marshal(lifecycleRequest{ConfigYAML: configYAML, SchemaVersion: 2})
	if _, err := handleMethod(pluginabi.MethodPluginRegister, registrationRequest); err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(requestInterceptRPC{
		RequestInterceptRequest: pluginapi.RequestInterceptRequest{
			SourceFormat: "openai-response",
			Model:        "deepseek-v4-flash",
			Body:         []byte(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`),
			Metadata:     map[string]any{"request_path": "/v1/responses"},
		},
		HostCallbackID: "host-callback-1",
	})
	raw, err := handleMethod(pluginabi.MethodRequestInterceptAfter, request)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("envelope=%s err=%v", raw, err)
	}
	var response pluginapi.RequestInterceptResponse
	if err := json.Unmarshal(env.Result, &response); err != nil || response.Terminate || strings.Contains(string(response.Body), "input_image") {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	if got.Model != "host-vision-model" || got.EntryProtocol != "openai-response" || got.ExitProtocol != "openai-response" || got.Stream {
		t.Fatalf("host request=%#v", got)
	}
	if callbackID != "host-callback-1" || got.Headers.Get("Authorization") != "" || got.Headers.Get("Cookie") != "" {
		t.Fatalf("callback=%q headers=%#v", callbackID, got.Headers)
	}
}

func TestInvalidInitialConfigurationStillPublishesFields(t *testing.T) {
	defer shutdownPlugin()
	rawRequest, _ := json.Marshal(lifecycleRequest{SchemaVersion: 2, ConfigYAML: []byte("vision_model: [\n")})
	raw, err := handleMethod(pluginabi.MethodPluginRegister, rawRequest)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("envelope=%s err=%v", raw, err)
	}
	var registered registration
	if err := json.Unmarshal(env.Result, &registered); err != nil {
		t.Fatal(err)
	}
	if len(registered.Metadata.ConfigFields) != 9 {
		t.Fatalf("config fields=%#v", registered.Metadata.ConfigFields)
	}
}
