// Package interceptor implements the fail-closed AfterAuth request pipeline.
package interceptor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/config"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/responses"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/safety"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/vision"
)

var ErrRuntimeUnavailable = errors.New("vision interceptor runtime is unavailable")

const HostCallbackIDMetadataKey = "__deepseek_vision_host_callback_id"

// AnalyzerFactory constructs the thin host-model adapter for one immutable
// configuration snapshot. It is initialized lazily only for eligible images.
type AnalyzerFactory func(*config.Config) (vision.Analyzer, error)

type runtimeState struct {
	cfg      *config.Config
	factory  AnalyzerFactory
	once     sync.Once
	analyzer vision.Analyzer
	err      error
}

func (s *runtimeState) getAnalyzer() (vision.Analyzer, error) {
	if s == nil || s.factory == nil {
		return nil, ErrRuntimeUnavailable
	}
	s.once.Do(func() {
		s.analyzer, s.err = s.factory(s.cfg)
		if s.analyzer == nil && s.err == nil {
			s.err = ErrRuntimeUnavailable
		}
	})
	return s.analyzer, s.err
}

// Runtime manages immutable generations and implements the plugin handler.
type Runtime struct {
	mu      sync.Mutex
	current *runtimeState
	factory AnalyzerFactory
	// targetModels is retained across shutdown so the unavailable fallback can
	// preserve the same model gate. Without this snapshot, a shutdown would
	// conservatively reject image requests for every Responses model instead of
	// only the configured DeepSeek targets.
	targetModels []string
	shutdown     bool
}

func NewRuntime(factory AnalyzerFactory) *Runtime {
	defaults := config.Default()
	return &Runtime{
		factory:      factory,
		targetModels: append([]string(nil), defaults.TargetModels...),
	}
}

// Reconfigure atomically publishes a new immutable snapshot. In-flight calls
// retain their previous state through ordinary Go references.
func (r *Runtime) Reconfigure(cfg *config.Config) {
	if r == nil {
		return
	}
	if cfg == nil {
		r.Shutdown()
		return
	}
	r.mu.Lock()
	r.targetModels = append([]string(nil), cfg.TargetModels...)
	r.current = &runtimeState{cfg: cfg, factory: r.factory}
	r.shutdown = false
	r.mu.Unlock()
}

// Shutdown is idempotent and prevents new requests from acquiring a state.
func (r *Runtime) Shutdown() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.current = nil
	r.shutdown = true
	r.mu.Unlock()
}

func (r *Runtime) acquire() (*runtimeState, error) {
	if r == nil {
		return nil, ErrRuntimeUnavailable
	}
	r.mu.Lock()
	if r.shutdown || r.current == nil {
		r.mu.Unlock()
		return nil, ErrRuntimeUnavailable
	}
	state := r.current
	r.mu.Unlock()
	return state, nil
}

// Handle is intentionally fail-closed: it always returns a concrete response
// and never returns a processing error to the host, because host adapters are
// allowed to fail open when callback errors are returned.
func (r *Runtime) Handle(req pluginapi.RequestInterceptRequest) (resp pluginapi.RequestInterceptResponse, err error) {
	resp = passthrough(req)
	defer func() {
		if recovered := recover(); recovered != nil {
			resp = terminate(http.StatusBadGateway, "vision_preprocess_error", "vision preprocessing failed")
		}
		err = nil
	}()

	state, acquireErr := r.acquire()
	if acquireErr != nil {
		// Once a request has reached the image-interception seam, an unavailable
		// generation must not send the original image downstream. The host may
		// treat callback errors as pass-through, so return a concrete 502 for
		// targeted image-shaped Responses requests and retain passthrough for
		// unrelated models and requests without image candidates.
		if unavailableImageRequest(req, r.targetModelSnapshot()) {
			return terminate(http.StatusBadGateway, "vision_preprocess_error", "vision preprocessing is unavailable"), nil
		}
		return resp, nil
	}
	cfg := state.cfg
	if !eligible(req, cfg) {
		return resp, nil
	}
	baseCtx := context.Background()
	if callbackID, _ := req.Metadata[HostCallbackIDMetadataKey].(string); callbackID != "" {
		baseCtx = vision.WithHostCallbackID(baseCtx, callbackID)
	}
	ctx, cancel := context.WithTimeout(baseCtx, cfg.RequestTimeout)
	defer cancel()

	plan, planErr := responses.Discover(req.Body, responses.Options{
		MaxImages:         cfg.MaxImagesPerRequest,
		MaxReferenceBytes: cfg.MaxImageReferenceBytes,
		MaxBodyBytes:      cfg.MaxRequestBytes,
		MaxResultChars:    cfg.MaxResultChars,
	})
	if ctx.Err() != nil {
		return terminate(http.StatusBadGateway, "vision_preprocess_error", "vision preprocessing deadline exceeded"), nil
	}
	if planErr != nil {
		return terminateForError(planErr), nil
	}
	if !plan.HasImages() {
		return resp, nil
	}

	analyzer, analyzerErr := state.getAnalyzer()
	if analyzerErr != nil {
		return terminate(http.StatusBadGateway, "vision_preprocess_error", "vision preprocessing failed"), nil
	}
	images := plan.Images()
	results := make([]string, len(images))
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	for i := range images {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, analyzeErr := safeAnalyze(ctx, analyzer, images[i].Reference, images[i].FocusHint, cfg.MaxResultChars)
			if analyzeErr != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = analyzeErr
				}
				errMu.Unlock()
				cancel()
				return
			}
			results[i] = result
		}()
	}
	wg.Wait()
	errMu.Lock()
	hasErr := firstErr != nil
	errMu.Unlock()
	if hasErr {
		return terminate(http.StatusBadGateway, "vision_preprocess_error", "vision preprocessing failed"), nil
	}
	rewritten, rewriteErr := plan.RewriteText(results)
	if rewriteErr != nil {
		return terminateForError(rewriteErr), nil
	}
	return pluginapi.RequestInterceptResponse{Headers: req.Headers, Body: rewritten}, nil
}

func safeAnalyze(ctx context.Context, analyzer vision.Analyzer, reference, focusHint string, maxResultChars int) (result string, err error) {
	defer func() {
		if recover() != nil {
			result = ""
			err = errors.New("visual analyzer failed")
		}
	}()
	result, err = analyzer.Analyze(ctx, reference, focusHint)
	if err != nil {
		return "", err
	}
	if err := (safety.Limits{MaxResultChars: maxResultChars}).ValidateResult(result); err != nil {
		return "", err
	}
	return result, nil
}

func (r *Runtime) targetModelSnapshot() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.targetModels...)
}

// HandleUnavailable is the lifecycle fallback used while no generation is
// configured (including after shutdown). It preserves non-target traffic but
// terminates image-shaped Responses requests for the configured target models
// so an absent callback handler cannot turn a security boundary into host-level
// passthrough.
func HandleUnavailable(req pluginapi.RequestInterceptRequest, targetModels ...string) (pluginapi.RequestInterceptResponse, error) {
	if len(targetModels) == 0 {
		targetModels = config.Default().TargetModels
	}
	if unavailableImageRequest(req, targetModels) {
		return terminate(http.StatusBadGateway, "vision_preprocess_error", "vision preprocessing is unavailable"), nil
	}
	return passthrough(req), nil
}

func eligible(req pluginapi.RequestInterceptRequest, cfg *config.Config) bool {
	if cfg == nil || req.SourceFormat != "openai-response" {
		return false
	}
	path, ok := req.Metadata["request_path"].(string)
	if !ok || path != "/v1/responses" || req.Model == "" {
		return false
	}
	return targetModelMatches(req.Model, cfg.TargetModels)
}

func passthrough(req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	return pluginapi.RequestInterceptResponse{Headers: req.Headers, Body: req.Body}
}

func unavailableImageRequest(req pluginapi.RequestInterceptRequest, targetModels []string) bool {
	if req.SourceFormat != "openai-response" || req.Model == "" || !targetModelMatches(req.Model, targetModels) {
		return false
	}
	path, ok := req.Metadata["request_path"].(string)
	if !ok || path != "/v1/responses" {
		return false
	}
	// Discovery is deliberately run even while unavailable: raw marker scans
	// can be bypassed with JSON escapes or whitespace, while the parser provides
	// the same fail-closed structural decision used by the normal path.
	plan, err := responses.Discover(req.Body)
	return err != nil || plan.HasImages()
}

func targetModelMatches(model string, targetModels []string) bool {
	if model == "" {
		return false
	}
	for _, target := range targetModels {
		if model == target {
			return true
		}
	}
	return false
}

func terminateForError(err error) pluginapi.RequestInterceptResponse {
	var planner *responses.Error
	if errors.As(err, &planner) {
		switch planner.StatusCode {
		case http.StatusBadRequest:
			return terminate(http.StatusBadRequest, "invalid_request_error", "invalid Responses request")
		case http.StatusRequestEntityTooLarge:
			return terminate(http.StatusRequestEntityTooLarge, "invalid_request_error", "request exceeds configured limit")
		case http.StatusUnprocessableEntity:
			return terminate(http.StatusUnprocessableEntity, "invalid_request_error", "unsupported image source")
		}
	}
	return terminate(http.StatusBadGateway, "vision_preprocess_error", "vision preprocessing failed")
}

func terminate(status int, typ, message string) pluginapi.RequestInterceptResponse {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"type": typ, "message": message}})
	return pluginapi.RequestInterceptResponse{
		Terminate:       true,
		StatusCode:      status,
		ResponseHeaders: http.Header{"Content-Type": []string{"application/json"}},
		ResponseBody:    body,
	}
}
