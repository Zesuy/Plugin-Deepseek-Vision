package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

type requestInterceptRPC struct {
	pluginapi.RequestInterceptRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
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
	var cfg *config.Config
	if len(strings.TrimSpace(string(req.ConfigYAML))) == 0 {
		// Configuration is allowed to be incomplete while an operator edits it.
		// Initial registration can safely install host-backed defaults; later empty
		// edits preserve the last-known-good runtime.
		if method != pluginabi.MethodPluginRegister {
			return nil
		}
		cfg = config.Default()
	} else {
		// Validate before entering the lifecycle critical section so a malformed
		// update never delays shutdown. The validated snapshot is published after
		// the runtime gate is installed below.
		var err error
		cfg, err = config.ParseYAML(req.ConfigYAML)
		if err != nil {
			// User configuration errors are non-fatal lifecycle events. Returning an
			// RPC error makes CLIProxyAPI drop this plugin from its active snapshot,
			// which also removes ConfigFields until restart. Keep the last known-good
			// runtime (or the unavailable fail-closed fallback) and publish metadata.
			return nil
		}
	}
	if cfg == nil {
		return nil
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
				{Name: "vision_model", Type: pluginapi.ConfigFieldTypeString, Description: "宿主中已配置的视觉模型名称。默认值 / Host vision model. Default: gpt-5.6-luna."},
				{Name: "language", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"zh", "en", "auto"}, Description: "视觉分析语言：zh 中文、en English、auto 跟随请求。默认值 / Default: zh."},
				{Name: "max_inflight_vision_requests", Type: pluginapi.ConfigFieldTypeInteger, Description: "插件全局同时在途的宿主视觉请求数；多余任务排队。范围 1–16，默认 4。/ Global in-flight host vision requests; excess work queues. Range 1–16, default 4."},
				{Name: "emergency_max_images_per_request", Type: pluginapi.ConfigFieldTypeInteger, Description: "单个客户端请求的唯一图片应急上限，仅防御异常负载。范围 16–1024，默认 256。/ Emergency unique-image ceiling per client request. Range 16–1024, default 256."},
				{Name: "request_timeout_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "整次视觉预处理（含排队）的总超时秒数。范围 1–3600，默认 120。/ Total vision preprocessing deadline including queueing. Range 1–3600, default 120."},
				{Name: "analysis_cache_size", Type: pluginapi.ConfigFieldTypeInteger, Description: "prompt 组视觉分析缓存条目数，0 关闭缓存。默认值 / Prompt-group analysis cache entries; 0 disables. Default: 128."},
				{Name: "analysis_cache_ttl_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "data URI 分析缓存秒数。默认值 / Data-URI analysis cache TTL in seconds. Default: 900."},
				{Name: "analysis_url_cache_ttl_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "URL 图片分析缓存秒数。默认值 / URL-image analysis cache TTL in seconds. Default: 120."},
				{Name: "trace_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "完整明文调试 trace；包含请求、图片引用和模型结果，仅临时开启。默认值 / Full plaintext debug trace; includes requests, image references, and model results. Enable temporarily. Default: false."},
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
	var wrapped requestInterceptRPC
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, fmt.Errorf("decode request intercept: %w", err)
	}
	req := wrapped.RequestInterceptRequest
	if wrapped.HostCallbackID != "" {
		if req.Metadata == nil {
			req.Metadata = make(map[string]any)
		}
		req.Metadata[interceptor.HostCallbackIDMetadataKey] = wrapped.HostCallbackID
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
