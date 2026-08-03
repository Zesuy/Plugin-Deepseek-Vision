package vision

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestHostClientUsesResponsesProtocolWithoutCredentials(t *testing.T) {
	var got pluginapi.HostModelExecutionRequest
	var callbackID string
	client, err := NewHostClient(HostOptions{
		Model:    "vision-model",
		Language: "zh",
		Execute: func(_ context.Context, request pluginapi.HostModelExecutionRequest, id string) (pluginapi.HostModelExecutionResponse, error) {
			got, callbackID = request, id
			return pluginapi.HostModelExecutionResponse{
				StatusCode: http.StatusOK,
				Body:       []byte(`{"output_text":"Visible text: hello\nVisual description: screen"}`),
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithHostCallbackID(context.Background(), "callback-1")
	result, err := client.Analyze(ctx, "data:image/png;base64,AAAA", "read the screen")
	if err != nil {
		t.Fatal(err)
	}
	if result == "" || callbackID != "callback-1" {
		t.Fatalf("result=%q callbackID=%q", result, callbackID)
	}
	if got.EntryProtocol != "openai-response" || got.ExitProtocol != "openai-response" || got.Model != "vision-model" || got.Stream {
		t.Fatalf("host request = %#v", got)
	}
	if got.Headers.Get("Authorization") != "" || got.Headers.Get("Cookie") != "" || got.Headers.Get("Content-Type") != "application/json" {
		t.Fatalf("host headers = %#v", got.Headers)
	}
	var payload requestPayload
	if err := json.Unmarshal(got.Body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Model != "vision-model" || len(payload.Input) != 1 || len(payload.Input[0].Content) != 2 {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestHostClientBoundsAndSanitizesFailures(t *testing.T) {
	for _, test := range []struct {
		name     string
		response pluginapi.HostModelExecutionResponse
	}{
		{name: "status", response: pluginapi.HostModelExecutionResponse{StatusCode: http.StatusUnauthorized, Body: []byte(`secret upstream text`)}},
		{name: "oversized", response: pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"output_text":"too large"}`)}},
		{name: "invalid", response: pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`not-json`)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			maxBytes := int64(1024)
			if test.name == "oversized" {
				maxBytes = 4
			}
			client, err := NewHostClient(HostOptions{
				Model:            "vision-model",
				MaxResponseBytes: maxBytes,
				Execute: func(context.Context, pluginapi.HostModelExecutionRequest, string) (pluginapi.HostModelExecutionResponse, error) {
					return test.response, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Analyze(context.Background(), "data:image/png;base64,AAAA", ""); err == nil {
				t.Fatal("host failure unexpectedly succeeded")
			}
		})
	}
}
