package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/config"
)

func TestRegisterEnvelopeAndCapabilities(t *testing.T) {
	defer shutdownPlugin()
	raw, err := handleMethod("plugin.register", []byte(`{"schema_version":2}`))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK || env.Error != nil {
		t.Fatalf("envelope = %#v", env)
	}
	var result registration
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != 2 || !result.Capabilities.RequestInterceptor || result.Metadata.Name != pluginName {
		t.Fatalf("registration = %#v", result)
	}
	if len(result.Metadata.ConfigFields) != 9 {
		t.Fatalf("config field count = %d, want 9 fields", len(result.Metadata.ConfigFields))
	}
	wantFields := map[string]pluginapi.ConfigFieldType{
		"vision_model":                     pluginapi.ConfigFieldTypeString,
		"language":                         pluginapi.ConfigFieldTypeEnum,
		"max_inflight_vision_requests":     pluginapi.ConfigFieldTypeInteger,
		"emergency_max_images_per_request": pluginapi.ConfigFieldTypeInteger,
		"request_timeout_seconds":          pluginapi.ConfigFieldTypeInteger,
		"analysis_cache_size":              pluginapi.ConfigFieldTypeInteger,
		"analysis_cache_ttl_seconds":       pluginapi.ConfigFieldTypeInteger,
		"analysis_url_cache_ttl_seconds":   pluginapi.ConfigFieldTypeInteger,
		"trace_enabled":                    pluginapi.ConfigFieldTypeBoolean,
	}
	for _, field := range result.Metadata.ConfigFields {
		if field.Description == "" {
			t.Errorf("field %q has no description", field.Name)
		}
		if wantType, ok := wantFields[field.Name]; ok {
			if field.Type != wantType {
				t.Errorf("field %q type = %q, want %q", field.Name, field.Type, wantType)
			}
			delete(wantFields, field.Name)
		}
		if field.Name == "language" {
			want := []string{"zh", "en", "auto"}
			if len(field.EnumValues) != len(want) {
				t.Errorf("language enum = %#v, want %#v", field.EnumValues, want)
			} else {
				for i := range want {
					if field.EnumValues[i] != want[i] {
						t.Errorf("language enum = %#v, want %#v", field.EnumValues, want)
						break
					}
				}
			}
		}
	}
	if len(wantFields) != 0 {
		t.Fatalf("registration is missing representative config fields: %#v", wantFields)
	}
	afterAuth.RLock()
	handler := afterAuth.handler
	afterAuth.RUnlock()
	if handler == nil {
		t.Fatal("empty initial registration did not install the default host runtime")
	}
}

func TestLifecycleWithIncompleteConfigKeepsRegistrationMetadata(t *testing.T) {
	defer shutdownPlugin()
	request, err := json.Marshal(lifecycleRequest{
		SchemaVersion: pluginabi.SchemaVersion,
		ConfigYAML:    []byte("enabled: true\npriority: 100\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := handleMethod(pluginabi.MethodPluginRegister, request)
	if err != nil {
		t.Fatalf("metadata-only registration failed: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("registration envelope = %s, err=%v", raw, err)
	}
	var result registration
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Metadata.ConfigFields) == 0 {
		t.Fatal("metadata-only registration did not expose ConfigFields")
	}
	if _, err := handleMethod(pluginabi.MethodPluginReconfigure, request); err != nil {
		t.Fatalf("incomplete reconfigure removed registration metadata: %v", err)
	}
	malformed, err := json.Marshal(lifecycleRequest{
		SchemaVersion: pluginabi.SchemaVersion,
		ConfigYAML:    []byte("vision_model: [\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handleMethod(pluginabi.MethodPluginReconfigure, malformed); err != nil {
		t.Fatalf("malformed user configuration removed registration metadata: %v", err)
	}
}

func TestRegistrationUsesOverridableVersionVariable(t *testing.T) {
	original := pluginVersion
	pluginVersion = "0.1.0-test-override"
	defer func() { pluginVersion = original }()
	if got := pluginRegistration().Metadata.Version; got != pluginVersion {
		t.Fatalf("registration version = %q, want %q", got, pluginVersion)
	}
}

func TestLifecycleRejectsLegacySchema(t *testing.T) {
	if _, err := handleMethod("plugin.register", []byte(`{"schema_version":1}`)); err == nil {
		t.Fatal("legacy schema accepted")
	}
}

func TestAfterAuthHandlerSeam(t *testing.T) {
	defer SetAfterAuthHandler(nil)
	SetAfterAuthHandler(func(req pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, error) {
		return pluginapi.RequestInterceptResponse{Headers: req.Headers, Body: []byte(`{"handled":true}`)}, nil
	})
	req, err := json.Marshal(pluginapi.RequestInterceptRequest{Model: "deepseek-v4-flash", Body: []byte(`{"model":"deepseek-v4-flash"}`)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := handleMethod("request.intercept_after", req)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var got pluginapi.RequestInterceptResponse
	if err := json.Unmarshal(env.Result, &got); err != nil {
		t.Fatal(err)
	}
	if string(got.Body) != `{"handled":true}` {
		t.Fatalf("body = %s", got.Body)
	}
}

func TestAfterAuthWithoutHandlerFailsClosedImageRequest(t *testing.T) {
	defer SetAfterAuthHandler(nil)
	request, err := json.Marshal(pluginapi.RequestInterceptRequest{
		SourceFormat: "openai-response",
		Model:        "deepseek-v4-flash",
		Body:         []byte(`{"input":[{"content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`),
		Metadata:     map[string]any{"request_path": "/v1/responses"},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := handleMethod(pluginabi.MethodRequestInterceptAfter, request)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("envelope = %s, err=%v", raw, err)
	}
	var response pluginapi.RequestInterceptResponse
	if err := json.Unmarshal(env.Result, &response); err != nil {
		t.Fatal(err)
	}
	if !response.Terminate || response.StatusCode != http.StatusBadGateway || len(response.Body) != 0 {
		t.Fatalf("response = %#v", response)
	}
}

func TestTerminateJSON(t *testing.T) {
	raw, err := terminateJSON(http.StatusBadGateway, "vision_preprocess_error", "failed")
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var got pluginapi.RequestInterceptResponse
	if err := json.Unmarshal(env.Result, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Terminate || got.StatusCode != http.StatusBadGateway || got.ResponseHeaders.Get("Content-Type") != "application/json" {
		t.Fatalf("response = %#v", got)
	}
}

func TestCheckedABIRequestLengthBoundaries(t *testing.T) {
	for _, length := range []uint64{0, 1, maxABIRequestBytes} {
		got, err := checkedABIRequestLength(length)
		if err != nil || uint64(got) != length {
			t.Fatalf("checkedABIRequestLength(%d) = %d, %v", length, got, err)
		}
	}
	if _, err := checkedABIRequestLength(maxABIRequestBytes + 1); err != errABIRequestTooLarge {
		t.Fatalf("hard-cap error = %v", err)
	}
	if _, err := checkedABIRequestLength(maxCIntValue + 1); err != errABIRequestLengthOverflow {
		t.Fatalf("C.int overflow error = %v", err)
	}
}

func TestABIRequestCapAccountsForConfiguredBodyExpansion(t *testing.T) {
	for _, bodyBytes := range []uint64{16 << 20, 24 << 20, maxConfiguredBodyBytes} {
		encoded := base64EncodedLength(bodyBytes)
		if encoded < bodyBytes {
			t.Fatalf("base64 length %d is smaller than body %d", encoded, bodyBytes)
		}
		if encoded+maxABIEnvelopeOverhead > maxABIRequestBytes {
			t.Fatalf("body %d expands beyond ABI cap: encoded=%d cap=%d", bodyBytes, encoded, maxABIRequestBytes)
		}
	}
	if maxABIRequestBytes > maxCIntValue {
		t.Fatalf("ABI cap %d exceeds C.int copy limit %d", maxABIRequestBytes, maxCIntValue)
	}
}

func TestABIAdmissionBoundsConcurrentCopies(t *testing.T) {
	if abiInFlightRequests.Load() != 0 || abiInFlightBytes.Load() != 0 {
		t.Fatal("ABI admission state was not clean at test start")
	}
	if got := acquireABIAdmission(maxABIBudgetBytes + 1); got != abiAdmissionRequestBudget {
		t.Fatalf("oversized request failure = %q", got)
	}
	for i := int64(0); i < maxABIInFlightRequests; i++ {
		if got := acquireABIAdmission(1); got != abiAdmissionOK {
			t.Fatalf("admission slot %d was unexpectedly denied: %q", i, got)
		}
	}
	if got := acquireABIAdmission(1); got != abiAdmissionRequestCount {
		t.Fatalf("request-count failure = %q", got)
	}
	for i := int64(0); i < maxABIInFlightRequests; i++ {
		releaseABIAdmission(1)
	}
	if abiInFlightRequests.Load() != 0 || abiInFlightBytes.Load() != 0 {
		t.Fatal("ABI admission state did not release cleanly")
	}
	if got := acquireABIAdmission(maxABIBudgetBytes - 1); got != abiAdmissionOK {
		t.Fatalf("large admission = %q", got)
	}
	if got := acquireABIAdmission(2); got != abiAdmissionProcessBudget {
		t.Fatalf("process-budget failure = %q", got)
	}
	releaseABIAdmission(maxABIBudgetBytes - 1)
}

func TestOversizedInterceptorsTerminateWith413WithoutDecode(t *testing.T) {
	for _, method := range []string{"request.intercept_before", "request.intercept_after"} {
		raw, returnCode := oversizedRequestResult(method, errABIRequestTooLarge)
		if returnCode != 0 {
			t.Fatalf("%s return code = %d, want 0", method, returnCode)
		}
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil || !env.OK || env.Error != nil {
			t.Fatalf("%s envelope = %s, err=%v", method, raw, err)
		}
		var response pluginapi.RequestInterceptResponse
		if err := json.Unmarshal(env.Result, &response); err != nil {
			t.Fatal(err)
		}
		if !response.Terminate || response.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s response = %#v", method, response)
		}
		if response.ResponseHeaders.Get("Content-Type") != "application/json" {
			t.Fatalf("%s headers = %#v", method, response.ResponseHeaders)
		}
		var errorBody struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(response.ResponseBody, &errorBody); err != nil || errorBody.Error.Type != "request_too_large" {
			t.Fatalf("%s error body = %s, err=%v", method, response.ResponseBody, err)
		}
	}
	if raw, returnCode := oversizedRequestResult("plugin.reconfigure", errABIRequestTooLarge); returnCode == 0 {
		t.Fatalf("oversized lifecycle RPC returned success: %s", raw)
	}
}

func TestWriteResponseReportsUnwritableDestination(t *testing.T) {
	if writeResponse(nil, []byte(`{"ok":true}`)) {
		t.Fatal("writeResponse(nil, raw) = true")
	}
	if writeResponse(nil, nil) {
		t.Fatal("writeResponse(nil, nil) = true")
	}
}

func TestShutdownSupersedesBlockedConcurrentConfigure(t *testing.T) {
	configureLocked := make(chan struct{})
	shutdownSignaled := make(chan struct{})
	releaseConfigure := make(chan struct{})
	var configureOnce sync.Once
	var shutdownOnce sync.Once
	lifecycleTestHook = func(stage string) {
		switch stage {
		case "configure_locked":
			configureOnce.Do(func() { close(configureLocked) })
			<-releaseConfigure
		case "shutdown_signaled":
			shutdownOnce.Do(func() { close(shutdownSignaled) })
		}
	}
	defer func() {
		lifecycleTestHook = nil
		shutdownPlugin()
	}()

	req, err := json.Marshal(lifecycleRequest{SchemaVersion: 2, ConfigYAML: []byte("vision_model: concurrent")})
	if err != nil {
		t.Fatal(err)
	}
	configureResult := make(chan error, 1)
	go func() {
		_, err := handleMethod("plugin.reconfigure", req)
		configureResult <- err
	}()
	<-configureLocked
	shutdownDone := make(chan struct{})
	go func() {
		shutdownPlugin()
		close(shutdownDone)
	}()
	<-shutdownSignaled
	close(releaseConfigure)
	if err := <-configureResult; err == nil {
		t.Fatal("concurrent configuration was not superseded by shutdown")
	}
	<-shutdownDone
	if cfg := config.Snapshot(); cfg != nil {
		t.Fatalf("config snapshot reactivated after shutdown: %#v", cfg)
	}
	afterAuth.RLock()
	handler := afterAuth.handler
	afterAuth.RUnlock()
	if handler != nil {
		t.Fatal("after-auth handler reactivated after shutdown")
	}
}

func TestConfigureStartedDuringShutdownCannotReactivate(t *testing.T) {
	shutdownLocked := make(chan struct{})
	releaseShutdown := make(chan struct{})
	var lockedOnce sync.Once
	lifecycleTestHook = func(stage string) {
		if stage == "shutdown_locked" {
			lockedOnce.Do(func() { close(shutdownLocked) })
			<-releaseShutdown
		}
	}
	defer func() {
		lifecycleTestHook = nil
		shutdownPlugin()
	}()

	shutdownDone := make(chan struct{})
	go func() {
		shutdownPlugin()
		close(shutdownDone)
	}()
	<-shutdownLocked
	configureDone := make(chan error, 1)
	go func() {
		_, err := handleMethod("plugin.reconfigure", []byte(`{"schema_version":2}`))
		configureDone <- err
	}()
	select {
	case err := <-configureDone:
		if err == nil {
			t.Fatal("configuration succeeded while shutdown was pending")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("configuration did not promptly reject pending shutdown")
	}
	close(releaseShutdown)
	<-shutdownDone
	if config.Snapshot() != nil {
		t.Fatal("configuration reactivated after shutdown")
	}
}

func TestLifecycleConfigureShutdownRace(t *testing.T) {
	defer shutdownPlugin()
	const workers = 12
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = handleMethod("plugin.reconfigure", []byte(`{"schema_version":2}`))
		}()
		go func() {
			defer wg.Done()
			shutdownPlugin()
		}()
	}
	wg.Wait()
	shutdownPlugin()
	if config.Snapshot() != nil {
		t.Fatal("final shutdown left an active config")
	}
}
