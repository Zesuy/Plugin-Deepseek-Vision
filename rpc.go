package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/config"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/interceptor"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	RequestInterceptor bool `json:"request_interceptor"`
}

// AfterAuthHandler is the request integration seam. The ABI layer remains
// independent of request rewriting: a handler receives the decoded request
// and returns the replacement response. A nil handler selects the lifecycle
// fallback, which still protects configured target-model image requests.
type AfterAuthHandler func(pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, error)

var afterAuth struct {
	sync.RWMutex
	handler AfterAuthHandler
}

// unavailableTargets preserves the last successfully published model gate
// after shutdown. The fallback must remain scoped to DeepSeek targets; it must
// not reject image requests for unrelated Responses models merely because the
// plugin has no active runtime generation.
var unavailableTargets struct {
	sync.RWMutex
	models []string
}

var (
	// lifecycleMu makes configuration publication + runtime replacement and
	// config clearing + runtime shutdown + handler removal single operations.
	lifecycleMu              sync.Mutex
	lifecycleEpoch           atomic.Uint64
	lifecycleShutdownPending atomic.Int32

	// lifecycleTestHook is assigned only by deterministic package tests before
	// worker goroutines start, and cleared after they join.
	lifecycleTestHook func(string)
)

// SetAfterAuthHandler wires the request preprocessor.  It is safe to call
// concurrently with requests; each call observes one handler snapshot.
func SetAfterAuthHandler(handler AfterAuthHandler) {
	afterAuth.Lock()
	afterAuth.handler = handler
	afterAuth.Unlock()
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(method, request); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodPluginShutdown:
		shutdownPlugin()
		return okEnvelope(struct{}{})
	case pluginabi.MethodRequestInterceptBefore:
		return passThroughRequest(request)
	case pluginabi.MethodRequestInterceptAfter:
		return interceptAfterAuth(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(method string, raw []byte) error {
	observedEpoch := lifecycleEpoch.Load()
	if lifecycleShutdownPending.Load() > 0 {
		return errors.New("plugin shutdown is in progress")
	}
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return fmt.Errorf("decode lifecycle request: %w", err)
		}
	}
	if req.SchemaVersion < pluginabi.SchemaVersion {
		return fmt.Errorf("host schema version %d is unsupported; schema version %d or newer is required", req.SchemaVersion, pluginabi.SchemaVersion)
	}
	if len(strings.TrimSpace(string(req.ConfigYAML))) == 0 {
		if method == pluginabi.MethodPluginRegister {
			// Registration is also the management UI's metadata discovery seam. It
			// must succeed before the host can expose ConfigFields to an operator.
			return nil
		}
		return errors.New("explicit plugin configuration is required")
	}
	// Validate before entering the lifecycle critical section so a malformed
	// update never delays shutdown. The validated snapshot is published after
	// the runtime gate is installed below.
	cfg, err := config.ParseYAML(req.ConfigYAML)
	if err != nil {
		if method == pluginabi.MethodPluginRegister {
			// Do not create a configuration bootstrap deadlock: an incomplete or
			// invalid initial document must not prevent the management UI from
			// discovering the fields needed to repair it. Reconfigure remains strict.
			return nil
		}
		return fmt.Errorf("validate plugin configuration: %w", err)
	}
	if cfg == nil || strings.TrimSpace(cfg.VisionBaseURL) == "" {
		if method == pluginabi.MethodPluginRegister {
			if cfg != nil {
				rememberUnavailableTargets(cfg.TargetModels)
			}
			return nil
		}
		return errors.New("vision_base_url must be explicitly configured")
	}
	if strings.TrimSpace(os.Getenv(cfg.VisionAPIKeyEnv)) == "" {
		if method == pluginabi.MethodPluginRegister {
			rememberUnavailableTargets(cfg.TargetModels)
			return nil
		}
		return errors.New("vision API key environment variable is unavailable")
	}

	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	runLifecycleTestHook("configure_locked")
	if lifecycleShutdownPending.Load() > 0 || lifecycleEpoch.Load() != observedEpoch {
		return errors.New("plugin shutdown superseded configuration")
	}
	// Publish the new runtime gate before publishing the config snapshot. The
	// request handler uses the runtime directly, so this ordering avoids a
	// window where management sees new targets while interception still uses the
	// old generation. PublishValidated is infallible after ParseYAML validation.
	rememberUnavailableTargets(cfg.TargetModels)
	reconfigureRuntimeWithConfig(cfg)
	if err := config.PublishValidated(cfg); err != nil {
		return fmt.Errorf("validate plugin configuration: %w", err)
	}
	return nil
}

func shutdownPlugin() {
	lifecycleShutdownPending.Add(1)
	lifecycleEpoch.Add(1)
	runLifecycleTestHook("shutdown_signaled")
	lifecycleMu.Lock()
	defer func() {
		lifecycleMu.Unlock()
		lifecycleShutdownPending.Add(-1)
	}()
	runLifecycleTestHook("shutdown_locked")
	config.Shutdown()
	shutdownRuntime()
	SetAfterAuthHandler(nil)
}

func runLifecycleTestHook(stage string) {
	if hook := lifecycleTestHook; hook != nil {
		hook(stage)
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "Zesuy",
			GitHubRepository: "https://github.com/zesuy/Plugin-Deepseek-Vision",
			Logo:             "",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "target_models", Type: pluginapi.ConfigFieldTypeArray, Description: "Final upstream models eligible for image preprocessing."},
				{Name: "vision_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "OpenAI-compatible VLM base URL."},
				{Name: "vision_model", Type: pluginapi.ConfigFieldTypeString, Description: "VLM model identifier."},
				{Name: "vision_api_key_env", Type: pluginapi.ConfigFieldTypeString, Description: "Environment variable containing the VLM API key."},
				{Name: "language", Type: pluginapi.ConfigFieldTypeString, Description: "Preferred language for visual analysis."},
				{Name: "request_timeout_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Total preprocessing deadline."},
				{Name: "per_call_timeout_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Per-image VLM request deadline."},
				{Name: "retry_max_attempts", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum attempts for transient VLM failures."},
				{Name: "max_concurrency", Type: pluginapi.ConfigFieldTypeInteger, Description: "Global concurrent VLM call limit."},
				{Name: "max_images_per_request", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum image blocks in a request."},
				{Name: "max_request_bytes", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum intercepted request size."},
				{Name: "max_image_reference_bytes", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum image URL or data reference size."},
				{Name: "max_response_bytes", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum VLM response size."},
				{Name: "max_result_chars", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum extracted VLM result characters."},
				{Name: "cache_size", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum cached VLM results."},
				{Name: "cache_ttl_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "VLM result cache lifetime."},
			},
		},
		Capabilities: registrationCapability{RequestInterceptor: true},
	}
}

func passThroughRequest(raw []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode request intercept: %w", err)
	}
	return okEnvelope(pluginapi.RequestInterceptResponse{Headers: req.Headers, Body: req.Body})
}

func interceptAfterAuth(raw []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode request intercept: %w", err)
	}
	afterAuth.RLock()
	handler := afterAuth.handler
	afterAuth.RUnlock()
	if handler == nil {
		response, _ := interceptor.HandleUnavailable(req, unavailableTargetSnapshot()...)
		return okEnvelope(response)
	}
	response, err := handler(req)
	if err != nil {
		return nil, err
	}
	return okEnvelope(response)
}

func rememberUnavailableTargets(models []string) {
	unavailableTargets.Lock()
	unavailableTargets.models = append([]string(nil), models...)
	unavailableTargets.Unlock()
}

func unavailableTargetSnapshot() []string {
	if cfg := config.Snapshot(); cfg != nil && len(cfg.TargetModels) > 0 {
		return append([]string(nil), cfg.TargetModels...)
	}
	unavailableTargets.RLock()
	models := append([]string(nil), unavailableTargets.models...)
	unavailableTargets.RUnlock()
	if len(models) > 0 {
		return models
	}
	return append([]string(nil), config.Default().TargetModels...)
}

func okEnvelope(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, err := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	if err != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"encode error"}}`)
	}
	return raw
}

// terminateJSON turns a preprocessing failure into a direct downstream
// response without exposing upstream text.
func terminateJSON(status int, typ, message string) ([]byte, error) {
	if status < 400 || status > 599 {
		return nil, errors.New("termination status must be a 4xx or 5xx code")
	}
	body, err := json.Marshal(map[string]any{"error": map[string]any{"type": typ, "message": message}})
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.RequestInterceptResponse{
		Terminate:       true,
		StatusCode:      status,
		ResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		ResponseBody:    body,
	})
}
