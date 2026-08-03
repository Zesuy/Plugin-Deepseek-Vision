package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseDefaults(t *testing.T) {
	cfg, err := ParseYAML(nil)
	if err != nil {
		t.Fatalf("ParseYAML(nil): %v", err)
	}
	if cfg.VisionBackend != VisionBackendHost || cfg.VisionModel != DefaultVisionModel || cfg.RequestTimeout != 120*time.Second {
		t.Fatalf("defaults = %#v", cfg)
	}
	if cfg.VisionBaseURL != "" {
		t.Fatalf("default VisionBaseURL must be empty, got %q", cfg.VisionBaseURL)
	}
	if len(cfg.TargetModels) != 1 || cfg.TargetModels[0] != "deepseek-v4-flash" {
		t.Fatalf("target models = %#v", cfg.TargetModels)
	}
}

func TestVisionBackendDefaultsAndLegacyExternalInference(t *testing.T) {
	host, err := ParseYAML([]byte("vision_model: host-vision"))
	if err != nil {
		t.Fatal(err)
	}
	if host.VisionBackend != VisionBackendHost || host.VisionBaseURL != "" {
		t.Fatalf("host config = %#v", host)
	}
	external, err := ParseYAML([]byte("vision_base_url: https://vision.example/v1"))
	if err != nil {
		t.Fatal(err)
	}
	if external.VisionBackend != VisionBackendExternal {
		t.Fatalf("legacy external backend = %q", external.VisionBackend)
	}
}

func TestParseHostWrapperIgnoresHostOwnedSwitches(t *testing.T) {
	raw := []byte(`plugins:
  enabled: false
  configs:
    deepseek-vision:
      enabled: false
      priority: 1
      vision_base_url: http://127.0.0.1:8317/v1
      vision_model: test-model
      cache_size: 0
`)
	cfg, err := ParseYAML(raw)
	if err != nil {
		t.Fatalf("ParseYAML(wrapper): %v", err)
	}
	if cfg.VisionModel != "test-model" || cfg.CacheSize != 0 {
		t.Fatalf("config = %#v", cfg)
	}
}

func TestParseStoreManagedWrapperIgnoresHostMetadata(t *testing.T) {
	raw, err := os.ReadFile("testdata/store-managed-wrapper.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseYAML(raw)
	if err != nil {
		t.Fatalf("ParseYAML(store wrapper): %v", err)
	}
	if cfg.VisionModel != "store-managed-model" {
		t.Fatalf("vision model = %q", cfg.VisionModel)
	}
	if _, err := ParseYAML([]byte("store: {}\nunknown_plugin_field: true")); err == nil {
		t.Fatal("store metadata caused unknown plugin field to be accepted")
	}
}

func TestParseRejectsNonemptyTrailingYAMLDocument(t *testing.T) {
	if _, err := ParseYAML([]byte("vision_base_url: http://127.0.0.1:8317/v1\nvision_model: first\n---\nvision_base_url: http://127.0.0.1:8317/v1\nvision_model: second\n")); err == nil {
		t.Fatal("nonempty trailing YAML document accepted")
	}
	if _, err := ParseYAML([]byte("vision_base_url: http://127.0.0.1:8317/v1\nvision_model: first\n---\n# empty trailing document\n")); err != nil {
		t.Fatalf("empty trailing YAML document rejected: %v", err)
	}
}

func TestTargetModelsAreCanonicalizedBeforePublication(t *testing.T) {
	cfg, err := ParseYAML([]byte("vision_base_url: http://127.0.0.1:8317/v1\ntarget_models: ['  deepseek-v4-flash  ', 'deepseek-v4-pro ']"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"deepseek-v4-flash", "deepseek-v4-pro"}
	for i := range want {
		if cfg.TargetModels[i] != want[i] {
			t.Fatalf("TargetModels = %#v", cfg.TargetModels)
		}
	}
	if _, err := ParseYAML([]byte("vision_base_url: http://127.0.0.1:8317/v1\ntarget_models: ['deepseek-v4-flash', ' deepseek-v4-flash ']")); err == nil {
		t.Fatal("canonical duplicate target model accepted")
	}
}

func TestVisionBaseURLIsTrimmedAndCanonicalized(t *testing.T) {
	cfg, err := ParseYAML([]byte("vision_base_url: '  https://vision.example/v1///  '"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VisionBaseURL != "https://vision.example/v1" {
		t.Fatalf("VisionBaseURL = %q", cfg.VisionBaseURL)
	}
}

func TestParseRejectsUnknownAndInvalidValues(t *testing.T) {
	for _, raw := range []string{
		"unknown_field: true",
		"max_concurrency: 0",
		"vision_backend: unknown",
		"vision_backend: external",
		"vision_base_url: file:///tmp/x",
		"vision_base_url: https://vision.example/v1?token=secret",
		"vision_base_url: https://vision.example/v1#fragment",
		"vision_base_url: https://vision.example/v1/responses",
		"vision_base_url: https://vision.example/v1%2Fresponses",
		"vision_base_url: https://vision.example:bad/v1",
		"vision_api_key_env: bad-name",
		"per_call_timeout_seconds: 61\nrequest_timeout_seconds: 60",
	} {
		if _, err := ParseYAML([]byte(raw)); err == nil {
			t.Errorf("ParseYAML(%q) error = nil", raw)
		}
	}
}

func TestConfigureIsAtomicOnFailure(t *testing.T) {
	good, err := ParseYAML([]byte("vision_base_url: http://127.0.0.1:8317/v1\nvision_model: first"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ConfigureYAML([]byte("vision_base_url: http://127.0.0.1:8317/v1\nvision_model: first")); err != nil {
		t.Fatal(err)
	}
	if err := ConfigureYAML([]byte("vision_api_key_env: not-valid")); err == nil {
		t.Fatal("invalid reconfigure returned nil")
	}
	if got := Snapshot().VisionModel; got != good.VisionModel {
		t.Fatalf("snapshot changed after failed configure: %q", got)
	}
}

func TestShutdownIdempotent(t *testing.T) {
	if err := ConfigureYAML([]byte("vision_base_url: http://127.0.0.1:8317/v1\nvision_model: before-shutdown")); err != nil {
		t.Fatal(err)
	}
	Shutdown()
	Shutdown()
	if Snapshot() != nil {
		t.Fatal("Snapshot() is non-nil after shutdown")
	}
	// Tests in this package share process state; restore defaults for callers.
	if err := ConfigureYAML([]byte(strings.TrimSpace("{}"))); err != nil {
		t.Fatal(err)
	}
}
