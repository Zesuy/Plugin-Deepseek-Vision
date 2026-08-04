package interceptor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/config"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/responses"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/tracelog"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/vision"
)

type testAnalyzer struct {
	mu        sync.Mutex
	focus     []string
	refs      []string
	err       error
	started   chan struct{}
	continueC chan struct{}
}

type failOnceAnalyzer struct {
	mu    sync.Mutex
	calls int
}

type batchTestAnalyzer struct {
	mu      sync.Mutex
	batches [][]vision.ImageInput
	prompts []string
	active  int
	peak    int
	limit   int
	status  int
	block   chan struct{}
	started chan struct{}
}

func (a *batchTestAnalyzer) Analyze(ctx context.Context, reference, prompt string) (string, error) {
	return a.AnalyzeBatch(ctx, []vision.ImageInput{{Number: 1, Reference: reference}}, prompt)
}

func (a *batchTestAnalyzer) AnalyzeBatch(ctx context.Context, images []vision.ImageInput, prompt string) (string, error) {
	a.mu.Lock()
	a.batches = append(a.batches, append([]vision.ImageInput(nil), images...))
	a.prompts = append(a.prompts, prompt)
	a.active++
	if a.active > a.peak {
		a.peak = a.active
	}
	limit, status, block := a.limit, a.status, a.block
	started := a.started
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.active--
		a.mu.Unlock()
	}()
	if limit > 0 && len(images) > limit {
		return "", &vision.HTTPStatusError{StatusCode: http.StatusRequestEntityTooLarge}
	}
	if status > 0 {
		return "", &vision.HTTPStatusError{StatusCode: status}
	}
	if started != nil {
		started <- struct{}{}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "joint visual analysis", nil
}

func (a *failOnceAnalyzer) Analyze(context.Context, string, string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.calls == 1 {
		return "", errors.New("transient failure")
	}
	return "Visible text: retry\nVisual description: success", nil
}

func (a *testAnalyzer) Analyze(ctx context.Context, ref, focus string) (string, error) {
	a.mu.Lock()
	a.refs = append(a.refs, ref)
	a.focus = append(a.focus, focus)
	started, continueC, err := a.started, a.continueC, a.err
	a.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if continueC != nil {
		select {
		case <-continueC:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if err != nil {
		return "", err
	}
	return "Visible text: " + ref + "\nVisual description: focus=" + focus, nil
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.ParseYAML([]byte(`
target_models: [deepseek-v4-flash, deepseek-v4-pro]
vision_model: gpt-5.6-luna
language: zh
request_timeout_seconds: 2
emergency_max_images_per_request: 256
max_inflight_vision_requests: 4
max_request_bytes: 1048576
max_image_reference_bytes: 1048576
max_response_bytes: 1048576
max_result_chars: 20000
`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newTestRuntime(t *testing.T, analyzer *testAnalyzer) *Runtime {
	t.Helper()
	r := NewRuntime(func(*config.Config) (vision.Analyzer, error) {
		return analyzer, nil
	})
	r.Reconfigure(testConfig(t))
	return r
}

func makeRequest(model, source, path, body string) pluginapi.RequestInterceptRequest {
	return pluginapi.RequestInterceptRequest{
		SourceFormat: source,
		ToFormat:     "openai-response",
		Model:        model,
		Body:         []byte(body),
		Headers:      http.Header{"X-Test": []string{"keep"}},
		Metadata:     map[string]any{"request_path": path},
	}
}

func TestHandleRewritesAllSupportedImagesWithFocus(t *testing.T) {
	analyzer := &testAnalyzer{}
	r := newTestRuntime(t, analyzer)
	defer r.Shutdown()
	body := `{"input":[{"role":"user","content":[{"type":"input_text","text":"first focus"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"},{"type":"input_text","text":"second focus"},{"type":"input_image","image_url":"https://example.com/two.png"}]},{"type":"function_call_output","output":[{"type":"input_image","image_url":"https://example.com/three.png"}]}]}`
	resp, err := r.Handle(makeRequest("deepseek-v4-pro", "openai-response", "/v1/responses", body))
	if err != nil || resp.Terminate || string(resp.Body) == body {
		t.Fatalf("response err=%v terminate=%v body=%s", err, resp.Terminate, resp.Body)
	}
	if strings.Contains(string(resp.Body), "input_image") || strings.Contains(string(resp.Body), "data:image") {
		t.Fatalf("rewritten body retained image: %s", resp.Body)
	}
	if !strings.Contains(string(resp.Body), "[Images 1, 2 — Joint visual analysis]") || strings.Count(string(resp.Body), "[Images 1 — Joint visual analysis]") != 1 {
		t.Fatalf("missing replacement blocks: %s", resp.Body)
	}
	analyzer.mu.Lock()
	defer analyzer.mu.Unlock()
	if len(analyzer.focus) != 3 {
		t.Fatalf("analyzer calls=%d", len(analyzer.focus))
	}
	for _, focus := range analyzer.focus {
		if focus == "" {
			t.Fatal("missing per-image focus hint")
		}
	}
}

func TestHandleTracesExactConfiguredLimitWithoutContent(t *testing.T) {
	type diagnostic struct {
		callbackID string
		level      string
		message    string
		fields     map[string]any
	}
	var got diagnostic
	r := NewRuntime(func(*config.Config) (vision.Analyzer, error) {
		return &testAnalyzer{}, nil
	}, func(callbackID, level, message string, fields map[string]any) {
		got = diagnostic{callbackID: callbackID, level: level, message: message, fields: fields}
	})
	r.Reconfigure(testConfig(t))
	defer r.Shutdown()

	body := bytes.Repeat([]byte("x"), 1048577)
	req := makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", string(body))
	req.Metadata[HostCallbackIDMetadataKey] = "callback-123"
	resp, err := r.Handle(req)
	if err != nil || !resp.Terminate || resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("response = %#v, err=%v", resp, err)
	}
	var public struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp.ResponseBody, &public); err != nil {
		t.Fatal(err)
	}
	if public.Error.Message != "request body exceeds configured limit: found 1048577 bytes, limit 1048576" {
		t.Fatalf("public message = %q", public.Error.Message)
	}
	if got.callbackID != "callback-123" || got.level != "warn" || got.message == "" {
		t.Fatalf("diagnostic = %#v", got)
	}
	if got.fields["limit_kind"] != "request_body" || got.fields["actual"] != len(body) || got.fields["maximum"] != 1048576 {
		t.Fatalf("diagnostic fields = %#v", got.fields)
	}
	for _, value := range got.fields {
		if text, ok := value.(string); ok && strings.Contains(text, "xxxx") {
			t.Fatal("diagnostic leaked request content")
		}
	}
}

func TestPublicLimitMessagesAreSpecific(t *testing.T) {
	tests := map[responses.LimitKind]string{
		responses.LimitRequestBody:    "request body exceeds configured limit",
		responses.LimitImageReference: "image reference exceeds configured limit",
		responses.LimitImageCount:     "request contains too many images",
		responses.LimitVLMResult:      "vision result exceeds configured limit",
	}
	for limit, want := range tests {
		if got := publicLimitMessage(limit, 0, 0); got != want {
			t.Errorf("publicLimitMessage(%q) = %q, want %q", limit, got, want)
		}
	}
}

func TestDiagnosticPanicCannotChangeLimitResponse(t *testing.T) {
	r := NewRuntime(func(*config.Config) (vision.Analyzer, error) {
		return &testAnalyzer{}, nil
	}, func(string, string, string, map[string]any) {
		panic("diagnostic unavailable")
	})
	r.Reconfigure(testConfig(t))
	defer r.Shutdown()

	body := bytes.Repeat([]byte("x"), 1048577)
	req := makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", string(body))
	resp, err := r.Handle(req)
	if err != nil || !resp.Terminate || resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("response changed after diagnostic panic: %#v, err=%v", resp, err)
	}
}

func TestFullContextTraceExplainsPromptGroups(t *testing.T) {
	root := filepath.Join(t.TempDir(), "deepseek-vision-trace")
	sink := tracelog.New(tracelog.Options{Root: root, MaxTotalBytes: 1 << 20, MaxEventBytes: 1 << 20})
	sink.Configure(true)
	defer sink.Close()

	analyzer := &batchTestAnalyzer{}
	r := NewRuntimeWithTrace(func(*config.Config) (vision.Analyzer, error) { return analyzer, nil }, sink)
	cfg := testConfig(t)
	cfg.TraceEnabled = true
	r.Reconfigure(cfg)
	defer r.Shutdown()

	body := `{"input":[` +
		`{"role":"user","content":[{"type":"input_text","text":"SECRET MULTI-TURN CONTEXT"},{"type":"input_image","image_url":"https://example.com/repeated.png"},{"type":"input_image","image_url":"https://example.com/history.png"}]},` +
		`{"role":"user","content":[{"type":"input_text","text":"latest turn"},{"type":"input_image","image_url":"https://example.com/repeated.png"},{"type":"input_image","image_url":"https://example.com/current-a.png"},{"type":"input_image","image_url":"https://example.com/current-b.png"}]}` +
		`]}`
	req := makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", body)
	req.RequestID = "request-trace-1"
	req.TraceID = "trace-1"
	req.Headers.Set("Authorization", "Bearer sk-secret")
	req.Metadata["api_key"] = "metadata-secret"
	req.Metadata[HostCallbackIDMetadataKey] = "callback-secret"
	resp, err := r.Handle(req)
	if err != nil || resp.Terminate || strings.Contains(string(resp.Body), "input_image") {
		t.Fatalf("response=%#v err=%v", resp, err)
	}

	entries, err := os.ReadDir(filepath.Join(root, "requests"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("trace bundles=%v err=%v", entries, err)
	}
	bundle := filepath.Join(root, "requests", entries[0].Name())
	inbound, err := os.ReadFile(filepath.Join(bundle, "10-inbound-body.json"))
	if err != nil || string(inbound) != body || !strings.Contains(string(inbound), "SECRET MULTI-TURN CONTEXT") {
		t.Fatalf("inbound trace missing full context: err=%v body=%s", err, inbound)
	}
	metadata, err := os.ReadFile(filepath.Join(bundle, "00-metadata.json"))
	if err != nil || strings.Contains(string(metadata), "sk-secret") || strings.Contains(string(metadata), "metadata-secret") || strings.Contains(string(metadata), "callback-secret") || !strings.Contains(string(metadata), "[REDACTED]") {
		t.Fatalf("metadata redaction failed: err=%v metadata=%s", err, metadata)
	}
	discovery, err := os.ReadFile(filepath.Join(bundle, "20-discovery.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"image_blocks": 5`, `"unique_image_references": 4`, `"duplicate_image_blocks": 1`, `"last_image_item_blocks": 3`, `"earlier_image_blocks": 2`, `"id": 1`, `"id": 2`, `https://example.com/repeated.png`} {
		if !strings.Contains(string(discovery), want) {
			t.Fatalf("discovery trace missing %s: %s", want, discovery)
		}
	}
	analyzer.mu.Lock()
	batches := append([][]vision.ImageInput(nil), analyzer.batches...)
	analyzer.mu.Unlock()
	if len(batches) != 2 || len(batches[0]) != 2 || len(batches[1]) != 3 {
		t.Fatalf("VLM batches = %#v", batches)
	}
}

func TestHandleBatchesOnePromptAndSplitsOnlyOn413(t *testing.T) {
	analyzer := &batchTestAnalyzer{limit: 2}
	r := NewRuntime(func(*config.Config) (vision.Analyzer, error) { return analyzer, nil })
	r.Reconfigure(testConfig(t))
	defer r.Shutdown()
	body := `{"input":[{"role":"user","content":[{"type":"input_text","text":"compare all three"},{"type":"input_image","image_url":"https://example.com/1.png"},{"type":"input_image","image_url":"https://example.com/2.png"},{"type":"input_image","image_url":"https://example.com/3.png"}]}]}`
	resp, err := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", body))
	if err != nil || resp.Terminate || strings.Contains(string(resp.Body), "input_image") {
		t.Fatalf("response=%#v err=%v", resp, err)
	}
	analyzer.mu.Lock()
	batches := append([][]vision.ImageInput(nil), analyzer.batches...)
	analyzer.mu.Unlock()
	if len(batches) != 3 || len(batches[0]) != 3 || len(batches[1]) != 1 || len(batches[2]) != 2 {
		t.Fatalf("adaptive batches = %#v", batches)
	}
	if !strings.Contains(string(resp.Body), "[Images 1, 2, 3 — Joint visual analysis]") {
		t.Fatalf("missing grouped rewrite: %s", resp.Body)
	}
}

func TestAdaptiveBatchDoesNotSplitSingletonOrNon413(t *testing.T) {
	for _, test := range []struct {
		name     string
		images   string
		analyzer *batchTestAnalyzer
	}{
		{name: "singleton 413", images: `{"type":"input_image","image_url":"https://example.com/1.png"}`, analyzer: &batchTestAnalyzer{limit: 0, status: http.StatusRequestEntityTooLarge}},
		{name: "multi-image 429", images: `{"type":"input_image","image_url":"https://example.com/1.png"},{"type":"input_image","image_url":"https://example.com/2.png"}`, analyzer: &batchTestAnalyzer{status: http.StatusTooManyRequests}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := NewRuntime(func(*config.Config) (vision.Analyzer, error) { return test.analyzer, nil })
			r.Reconfigure(testConfig(t))
			defer r.Shutdown()
			body := `{"input":[{"role":"user","content":[` + test.images + `]}]}`
			resp, err := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", body))
			if err != nil || !resp.Terminate || resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("response=%#v err=%v", resp, err)
			}
			test.analyzer.mu.Lock()
			calls := len(test.analyzer.batches)
			test.analyzer.mu.Unlock()
			if calls != 1 {
				t.Fatalf("unexpected split/retry calls=%d", calls)
			}
		})
	}
}

func TestVisionRequestConcurrencyIsGlobalAcrossClientRequests(t *testing.T) {
	started := make(chan struct{}, 8)
	block := make(chan struct{})
	analyzer := &batchTestAnalyzer{started: started, block: block}
	r := NewRuntime(func(*config.Config) (vision.Analyzer, error) { return analyzer, nil })
	cfg := testConfig(t)
	cfg.MaxInflightVisionRequests = 2
	r.Reconfigure(cfg)
	defer r.Shutdown()

	requestBody := func(prefix string) string {
		return `{"input":[` +
			`{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/` + prefix + `-1.png"}]},` +
			`{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/` + prefix + `-2.png"}]}` +
			`]}`
	}
	done := make(chan pluginapi.RequestInterceptResponse, 2)
	for _, prefix := range []string{"a", "b"} {
		go func(prefix string) {
			resp, _ := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", requestBody(prefix)))
			done <- resp
		}(prefix)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("two global vision slots were not occupied")
		}
	}
	select {
	case <-started:
		t.Fatal("a third host vision request exceeded the global limit")
	case <-time.After(100 * time.Millisecond):
	}
	close(block)
	for i := 0; i < 2; i++ {
		select {
		case resp := <-done:
			if resp.Terminate {
				t.Fatalf("response terminated: %#v", resp)
			}
		case <-time.After(time.Second):
			t.Fatal("request did not finish after releasing vision slots")
		}
	}
	analyzer.mu.Lock()
	peak, calls := analyzer.peak, len(analyzer.batches)
	analyzer.mu.Unlock()
	if peak != 2 || calls != 4 {
		t.Fatalf("peak=%d calls=%d", peak, calls)
	}
}

func TestVisionLimiterAppliesLowerReconfigureToInflightWork(t *testing.T) {
	limiter := newVisionLimiter(2)
	if err := limiter.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := limiter.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	limiter.SetLimit(1)
	acquired := make(chan struct{})
	go func() {
		if limiter.Acquire(context.Background()) == nil {
			close(acquired)
		}
	}()
	limiter.Release()
	select {
	case <-acquired:
		t.Fatal("lowered limit admitted work while one old callback was still active")
	case <-time.After(50 * time.Millisecond):
	}
	limiter.Release()
	select {
	case <-acquired:
		limiter.Release()
	case <-time.After(time.Second):
		t.Fatal("waiting callback was not admitted after inflight work drained")
	}
}

func TestTraceFilesystemFailureCannotChangeRequest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	sink := tracelog.New(tracelog.Options{Root: root})
	sink.Configure(true)
	if sink.Enabled() {
		t.Fatal("invalid trace sink unexpectedly enabled")
	}
	analyzer := &testAnalyzer{}
	r := NewRuntimeWithTrace(func(*config.Config) (vision.Analyzer, error) { return analyzer, nil }, sink)
	cfg := testConfig(t)
	cfg.TraceEnabled = true
	r.Reconfigure(cfg)
	defer r.Shutdown()
	body := `{"input":[{"role":"user","content":[{"type":"input_text","text":"continue without trace"},{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`
	resp, err := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", body))
	if err != nil || resp.Terminate || strings.Contains(string(resp.Body), "input_image") {
		t.Fatalf("trace failure changed response=%#v err=%v", resp, err)
	}
}

func TestHandleRewritesVisibleHistoryAndCurrentImages(t *testing.T) {
	analyzer := &testAnalyzer{}
	r := newTestRuntime(t, analyzer)
	defer r.Shutdown()
	historyRef := "https://example.com/history.png"
	currentRef := "https://example.com/current.png"
	body := `{"previous_response_id":"resp_history","input":[` +
		`{"role":"user","content":[{"type":"input_text","text":"history"},{"type":"input_image","image_url":"` + historyRef + `"}]},` +
		`{"role":"user","content":[{"type":"input_text","text":"current"},{"type":"input_image","image_url":"` + currentRef + `"}]}]}`
	resp, err := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", body))
	if err != nil || resp.Terminate {
		t.Fatalf("response=%#v err=%v", resp, err)
	}
	rewritten := string(resp.Body)
	if strings.Contains(rewritten, historyRef) || strings.Contains(rewritten, currentRef) || !strings.Contains(rewritten, "resp_history") {
		t.Fatalf("history rewrite = %s", rewritten)
	}
	analyzer.mu.Lock()
	callCount := len(analyzer.refs)
	analyzer.mu.Unlock()
	if callCount != 2 {
		t.Fatalf("analyzer calls=%d", callCount)
	}
}

func TestHandleDeduplicatesWithinRequestAndCachesAcrossRequests(t *testing.T) {
	analyzer := &batchTestAnalyzer{}
	r := NewRuntime(func(*config.Config) (vision.Analyzer, error) { return analyzer, nil })
	r.Reconfigure(testConfig(t))
	defer r.Shutdown()
	reference := "data:image/png;base64,AAAA"
	body := `{"input":[{"role":"user","content":[{"type":"input_text","text":"same focus"},{"type":"input_image","image_url":"` + reference + `"},{"type":"input_image","image_url":"` + reference + `"}]}]}`
	request := makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", body)
	for i := 0; i < 2; i++ {
		resp, err := r.Handle(request)
		if err != nil || resp.Terminate || strings.Contains(string(resp.Body), "input_image") {
			t.Fatalf("request %d response=%#v err=%v", i, resp, err)
		}
	}
	analyzer.mu.Lock()
	calls := len(analyzer.batches)
	analyzer.mu.Unlock()
	if calls != 1 {
		t.Fatalf("analyzer calls=%d, want one unique analysis", calls)
	}

	// Reconfigure publishes a fresh generation-local cache.
	r.Reconfigure(testConfig(t))
	if resp, err := r.Handle(request); err != nil || resp.Terminate {
		t.Fatalf("post-reconfigure response=%#v err=%v", resp, err)
	}
	analyzer.mu.Lock()
	calls = len(analyzer.batches)
	analyzer.mu.Unlock()
	if calls != 2 {
		t.Fatalf("post-reconfigure analyzer calls=%d, want cache reset", calls)
	}
}

func TestHandleDeduplicatesIdenticalPromptGroupsWithShiftedGlobalNumbers(t *testing.T) {
	analyzer := &batchTestAnalyzer{}
	r := NewRuntime(func(*config.Config) (vision.Analyzer, error) { return analyzer, nil })
	r.Reconfigure(testConfig(t))
	defer r.Shutdown()
	group := `{"role":"user","content":[{"type":"input_text","text":"same prompt"},{"type":"input_image","image_url":"https://example.com/shared.png"}]}`
	body := `{"input":[` + group + `,` + group + `]}`
	resp, err := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", body))
	if err != nil || resp.Terminate || strings.Contains(string(resp.Body), "input_image") {
		t.Fatalf("response=%#v err=%v", resp, err)
	}
	analyzer.mu.Lock()
	calls := len(analyzer.batches)
	analyzer.mu.Unlock()
	if calls != 1 {
		t.Fatalf("identical prompt groups produced %d VLM calls", calls)
	}
	if strings.Count(string(resp.Body), "[Images 1 — Joint visual analysis]") != 2 {
		t.Fatalf("both prompt groups were not rewritten with group-local labels: %s", resp.Body)
	}
}

func TestHandleCacheSeparatesDifferentFocusPrompts(t *testing.T) {
	analyzer := &testAnalyzer{}
	r := newTestRuntime(t, analyzer)
	defer r.Shutdown()
	reference := "https://example.com/shared.png"
	for _, focus := range []string{"first focus", "second focus"} {
		body := `{"input":[{"role":"user","content":[{"type":"input_text","text":"` + focus + `"},{"type":"input_image","image_url":"` + reference + `"}]}]}`
		resp, err := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", body))
		if err != nil || resp.Terminate {
			t.Fatalf("focus %q response=%#v err=%v", focus, resp, err)
		}
	}
	analyzer.mu.Lock()
	calls := len(analyzer.refs)
	analyzer.mu.Unlock()
	if calls != 2 {
		t.Fatalf("different focus prompts shared cache; calls=%d", calls)
	}
}

func TestHandleDoesNotCacheAnalyzerFailures(t *testing.T) {
	analyzer := &failOnceAnalyzer{}
	r := NewRuntime(func(*config.Config) (vision.Analyzer, error) { return analyzer, nil })
	r.Reconfigure(testConfig(t))
	defer r.Shutdown()
	body := `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`
	request := makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", body)
	first, err := r.Handle(request)
	if err != nil || !first.Terminate || first.StatusCode != http.StatusBadGateway {
		t.Fatalf("first response=%#v err=%v", first, err)
	}
	second, err := r.Handle(request)
	if err != nil || second.Terminate || strings.Contains(string(second.Body), "input_image") {
		t.Fatalf("second response=%#v err=%v", second, err)
	}
	analyzer.mu.Lock()
	calls := analyzer.calls
	analyzer.mu.Unlock()
	if calls != 2 {
		t.Fatalf("analyzer calls=%d, failed result was cached", calls)
	}
}

func TestHandleAllowsCacheToBeDisabled(t *testing.T) {
	analyzer := &testAnalyzer{}
	r := NewRuntime(func(*config.Config) (vision.Analyzer, error) { return analyzer, nil })
	cfg := testConfig(t)
	cfg.AnalysisCacheSize = 0
	r.Reconfigure(cfg)
	defer r.Shutdown()
	body := `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`
	request := makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", body)
	for i := 0; i < 2; i++ {
		resp, err := r.Handle(request)
		if err != nil || resp.Terminate {
			t.Fatalf("request %d response=%#v err=%v", i, resp, err)
		}
	}
	analyzer.mu.Lock()
	calls := len(analyzer.refs)
	analyzer.mu.Unlock()
	if calls != 2 {
		t.Fatalf("analyzer calls=%d, disabled cache reused a result", calls)
	}
}

func TestHandleGateUsesFinalModelAndExactPath(t *testing.T) {
	analyzer := &testAnalyzer{}
	r := newTestRuntime(t, analyzer)
	defer r.Shutdown()
	body := `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`
	cases := []struct {
		name    string
		req     pluginapi.RequestInterceptRequest
		rewrite bool
	}{
		{"target", makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", body), true},
		{"non-target", makeRequest("other", "openai-response", "/v1/responses", body), false},
		{"compact", makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses/compact", body), false},
		{"wrong-source", makeRequest("deepseek-v4-flash", "openai-request", "/v1/responses", body), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := r.Handle(tc.req)
			if err != nil || resp.Terminate || (tc.rewrite == (string(resp.Body) == body)) {
				t.Fatalf("response=%#v err=%v", resp, err)
			}
		})
	}
}

func TestHandleMapsPlannerAndAnalyzerFailures(t *testing.T) {
	t.Run("malformed", func(t *testing.T) {
		r := newTestRuntime(t, &testAnalyzer{})
		defer r.Shutdown()
		resp, _ := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", `{`))
		if !resp.Terminate || resp.StatusCode != http.StatusBadRequest || len(resp.Body) != 0 {
			t.Fatalf("response=%#v", resp)
		}
	})
	t.Run("unsupported", func(t *testing.T) {
		r := newTestRuntime(t, &testAnalyzer{})
		defer r.Shutdown()
		body := `{"input":[{"role":"user","content":[{"type":"input_image","file_id":"file_1"}]}]}`
		resp, _ := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", body))
		if !resp.Terminate || resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("response=%#v", resp)
		}
	})
	t.Run("analyzer", func(t *testing.T) {
		r := newTestRuntime(t, &testAnalyzer{err: errors.New("secret provider detail")})
		defer r.Shutdown()
		body := `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`
		resp, _ := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", body))
		if !resp.Terminate || resp.StatusCode != http.StatusBadGateway || strings.Contains(string(resp.ResponseBody), "secret") {
			t.Fatalf("response=%#v", resp)
		}
	})
}

func TestRuntimeShutdownPreservesInflightAndFailsClosedForNewRequests(t *testing.T) {
	analyzer := &testAnalyzer{started: make(chan struct{}, 1), continueC: make(chan struct{})}
	r := newTestRuntime(t, analyzer)
	body := `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`
	done := make(chan pluginapi.RequestInterceptResponse, 1)
	go func() {
		resp, _ := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", body))
		done <- resp
	}()
	select {
	case <-analyzer.started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	r.Shutdown()
	close(analyzer.continueC)
	if resp := <-done; resp.Terminate {
		t.Fatalf("in-flight request terminated: %#v", resp)
	}
	resp, _ := r.Handle(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", body))
	if !resp.Terminate || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("new request did not fail closed: %#v", resp)
	}
}

func TestUnavailableFallbackStaysScopedToTargets(t *testing.T) {
	body := `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`
	target, _ := HandleUnavailable(makeRequest("deepseek-v4-flash", "openai-response", "/v1/responses", body), "deepseek-v4-flash")
	if !target.Terminate {
		t.Fatal("target image request passed through")
	}
	other, _ := HandleUnavailable(makeRequest("other", "openai-response", "/v1/responses", body), "deepseek-v4-flash")
	if other.Terminate || string(other.Body) != body {
		t.Fatalf("non-target response=%#v", other)
	}
}
