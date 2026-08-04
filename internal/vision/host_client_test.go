package vision

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/tracelog"
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

func TestHostClientWritesFullPlaintextTrace(t *testing.T) {
	root := filepath.Join(t.TempDir(), "trace")
	sink := tracelog.New(tracelog.Options{Root: root, MaxTotalBytes: 1 << 20, MaxEventBytes: 1 << 20})
	sink.Configure(true)
	session := sink.Start(tracelog.RequestMeta{RequestID: "request-vlm", TraceID: "trace-vlm", ConfigGeneration: 2})
	if session == nil {
		t.Fatal("trace session is nil")
	}
	ctx := tracelog.WithSession(context.Background(), session)
	ctx = tracelog.WithJob(ctx, tracelog.Job{ID: 1, ImageNumbers: []int{2, 4}})
	ctx = WithHostCallbackID(ctx, "callback-vlm")

	responseBody := []byte(`{"output_text":"Visible text: plaintext response\nVisual description: traced"}`)
	client, err := NewHostClient(HostOptions{
		Model: "vision-model", Language: "zh",
		Execute: func(_ context.Context, request pluginapi.HostModelExecutionRequest, callbackID string) (pluginapi.HostModelExecutionResponse, error) {
			if callbackID != "callback-vlm" || !strings.Contains(string(request.Body), "data:image/png;base64,PLAINTEXT") {
				t.Fatalf("request=%s callback=%q", request.Body, callbackID)
			}
			return pluginapi.HostModelExecutionResponse{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Set-Cookie": []string{"provider-secret"}, "X-Api_Key": []string{"header-api-key"}, "X-Trace": []string{"visible"}},
				Body:       responseBody,
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Analyze(ctx, "data:image/png;base64,PLAINTEXT", "full focus hint")
	if err != nil || !strings.Contains(result, "plaintext response") {
		t.Fatalf("result=%q err=%v", result, err)
	}
	session.Close()
	sink.Close()

	entries, err := os.ReadDir(filepath.Join(root, "requests"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("bundles=%v err=%v", entries, err)
	}
	bundle := filepath.Join(root, "requests", entries[0].Name())
	checks := map[string][]string{
		"40-vlm-job-001-metadata.json":     {"data:image/png;base64,PLAINTEXT", "full focus hint"},
		"40-vlm-job-001-request.json":      {"data:image/png;base64,PLAINTEXT"},
		"40-vlm-job-001-response.json":     {"plaintext response"},
		"40-vlm-job-001-parsed-result.txt": {"plaintext response"},
	}
	for name, fragments := range checks {
		raw, readErr := os.ReadFile(filepath.Join(bundle, name))
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		for _, fragment := range fragments {
			if !strings.Contains(string(raw), fragment) {
				t.Fatalf("%s missing %q: %s", name, fragment, raw)
			}
		}
	}
	responseMetadata, err := os.ReadFile(filepath.Join(bundle, "40-vlm-job-001-response-metadata.json"))
	if err != nil || strings.Contains(string(responseMetadata), "provider-secret") || strings.Contains(string(responseMetadata), "header-api-key") || !strings.Contains(string(responseMetadata), "[REDACTED]") {
		t.Fatalf("response metadata redaction failed: err=%v body=%s", err, responseMetadata)
	}
}
