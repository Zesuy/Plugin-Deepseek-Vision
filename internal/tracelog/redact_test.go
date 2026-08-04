package tracelog

import (
	"net/http"
	"net/url"
	"testing"
)

func TestSensitiveNameVariants(t *testing.T) {
	for _, name := range []string{
		"Authorization", "x-api-key", "api_key", "api.key", "Api Key",
		"access_token", "refresh-token", "client_secret", "Cookie", "password",
	} {
		if !SensitiveName(name) {
			t.Errorf("SensitiveName(%q) = false", name)
		}
	}
	for _, name := range []string{"request_id", "trace_id", "model", "content-type"} {
		if SensitiveName(name) {
			t.Errorf("SensitiveName(%q) = true", name)
		}
	}
}

func TestRedactionCoversHeadersAndNestedMetadata(t *testing.T) {
	headers := RedactHeaders(http.Header{
		"X-Api_Key": []string{"header-secret"},
		"X-Visible": []string{"visible"},
	})
	if headers["X-Api_Key"][0] != "[REDACTED]" || headers["X-Visible"][0] != "visible" {
		t.Fatalf("headers = %#v", headers)
	}
	metadata := RedactJSONLike(map[string]any{
		"nested": map[string]any{"api_key": "metadata-secret", "request_id": "request-1"},
	}, "metadata").(map[string]any)
	nested := metadata["nested"].(map[string]any)
	if nested["api_key"] != "[REDACTED]" || nested["request_id"] != "request-1" {
		t.Fatalf("metadata = %#v", metadata)
	}
	query := RedactValues(url.Values{"access_token": []string{"query-secret"}, "mode": []string{"debug"}})
	if query.Get("access_token") != "[REDACTED]" || query.Get("mode") != "debug" {
		t.Fatalf("query = %#v", query)
	}
}
