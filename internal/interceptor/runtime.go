// Package interceptor implements the fail-closed AfterAuth request pipeline.
// It owns the VLM service lifetime while keeping the ABI and plugin wiring in
// the root package deliberately small.
package interceptor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/config"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/preprocess"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/responses"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/vision"
)

var ErrRuntimeUnavailable = errors.New("vision interceptor runtime is unavailable")

const HostCallbackIDMetadataKey = "__deepseek_vision_host_callback_id"

// ServiceFactory constructs a service for one immutable configuration
// generation. The factory is called lazily, only after a request has passed
// the gate and contains at least one supported image.
type ServiceFactory func(*config.Config, uint64, preprocess.Limiter) (*preprocess.Service, error)

type serviceEntry struct {
	cfg         *config.Config
	generation  uint64
	fingerprint string
	factory     ServiceFactory
	limiter     preprocess.Limiter

	mu      sync.Mutex
	initMu  sync.Mutex
	service *preprocess.Service
	active  int
	retired bool
	closed  bool
}

func (e *serviceEntry) getService() (*preprocess.Service, error) {
	e.initMu.Lock()
	defer e.initMu.Unlock()
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, ErrRuntimeUnavailable
	}
	if e.service != nil {
		s := e.service
		e.mu.Unlock()
		return s, nil
	}
	e.mu.Unlock()

	service, err := e.factory(e.cfg, e.generation, e.limiter)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		closeService(service)
		return nil, ErrRuntimeUnavailable
	}
	if e.service == nil {
		e.service = service
		service = nil
	}
	result := e.service
	e.mu.Unlock()
	if service != nil {
		closeService(service)
	}
	return result, nil
}

func (e *serviceEntry) release() {
	var service *preprocess.Service
	e.mu.Lock()
	if e.active > 0 {
		e.active--
	}
	if e.retired && e.active == 0 && !e.closed {
		e.closed = true
		service = e.service
		e.service = nil
	}
	e.mu.Unlock()
	if service != nil {
		closeService(service)
	}
}

func (e *serviceEntry) retire() {
	var service *preprocess.Service
	e.mu.Lock()
	e.retired = true
	if e.active == 0 && !e.closed {
		e.closed = true
		service = e.service
		e.service = nil
	}
	e.mu.Unlock()
	if service != nil {
		closeService(service)
	}
}

// Lease pins one configuration generation until Release. Reconfiguration
// retires the old entry, but an in-flight request may finish using its pinned
// immutable snapshot and service.
type Lease struct {
	entry *serviceEntry
	once  sync.Once
}

func (l *Lease) Config() *config.Config {
	if l == nil || l.entry == nil {
		return nil
	}
	return l.entry.cfg
}

func (l *Lease) Service() (*preprocess.Service, error) {
	if l == nil || l.entry == nil {
		return nil, ErrRuntimeUnavailable
	}
	return l.entry.getService()
}

func (l *Lease) Release() {
	if l == nil || l.entry == nil {
		return
	}
	l.once.Do(l.entry.release)
}

func closeService(service *preprocess.Service) {
	if service == nil {
		return
	}
	service.ClearCache()
	_ = service.Close()
}

// Runtime manages immutable generations and implements the plugin handler.
type Runtime struct {
	mu      sync.Mutex
	current *serviceEntry
	factory ServiceFactory
	limiter *dynamicLimiter
	// targetModels is retained across shutdown so the unavailable fallback can
	// preserve the same model gate. Without this snapshot, a shutdown would
	// conservatively reject image requests for every Responses model instead of
	// only the configured DeepSeek targets.
	targetModels []string
	generation   uint64
	shutdown     bool
}

func NewRuntime(factory ServiceFactory) *Runtime {
	defaults := config.Default()
	return &Runtime{
		factory:      factory,
		limiter:      newDynamicLimiter(),
		targetModels: append([]string(nil), defaults.TargetModels...),
	}
}

// Reconfigure publishes a new generation. Every explicit reconfigure retires
// the previous generation (even when values are unchanged), which guarantees
// cache invalidation and fresh environment-token resolution.
func (r *Runtime) Reconfigure(cfg *config.Config) {
	if r == nil {
		return
	}
	if cfg == nil {
		r.Shutdown()
		return
	}
	fp := fingerprint(cfg)
	r.mu.Lock()
	r.limiter.configure(cfg.MaxConcurrency)
	r.generation++
	r.targetModels = append([]string(nil), cfg.TargetModels...)
	entry := &serviceEntry{cfg: cfg, generation: r.generation, fingerprint: fp, factory: r.factory, limiter: r.limiter}
	old := r.current
	r.current = entry
	r.shutdown = false
	r.mu.Unlock()
	if old != nil {
		old.retire()
	}
}

// Shutdown is idempotent and prevents new leases. The active generation is
// retired and its service is closed after any in-flight request releases it.
func (r *Runtime) Shutdown() {
	if r == nil {
		return
	}
	r.mu.Lock()
	old := r.current
	r.current = nil
	r.shutdown = true
	r.limiter.shutdown()
	r.mu.Unlock()
	if old != nil {
		old.retire()
	}
}

func (r *Runtime) acquire() (*Lease, error) {
	if r == nil {
		return nil, ErrRuntimeUnavailable
	}
	r.mu.Lock()
	if r.shutdown || r.current == nil {
		r.mu.Unlock()
		return nil, ErrRuntimeUnavailable
	}
	e := r.current
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		r.mu.Unlock()
		return nil, ErrRuntimeUnavailable
	}
	e.active++
	e.mu.Unlock()
	r.mu.Unlock()
	return &Lease{entry: e}, nil
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

	lease, acquireErr := r.acquire()
	if acquireErr != nil {
		// Once a request has reached the image-interception seam, an unavailable
		// generation must not send the original image downstream. The host may
		// treat callback errors as pass-through, so return a concrete 502 for
		// targeted image-shaped Responses requests and retain passthrough for
		// unrelated models or requests that contain no image candidate.
		if unavailableImageRequest(req, r.targetModelSnapshot()) {
			return terminate(http.StatusBadGateway, "vision_preprocess_error", "vision preprocessing is unavailable"), nil
		}
		return resp, nil
	}
	defer lease.Release()
	cfg := lease.Config()
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

	service, serviceErr := lease.Service()
	if serviceErr != nil {
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
			result, analyzeErr := service.AnalyzeOne(ctx, preprocess.Image{Reference: images[i].Reference}, images[i].FocusHint)
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

func fingerprint(cfg *config.Config) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00", cfg.VisionBackend, cfg.VisionBaseURL, cfg.VisionModel, cfg.VisionAPIKeyEnv, vision.NormalizeLanguage(cfg.Language), strings.Join(cfg.TargetModels, "\x00"), cfg.RequestTimeout, cfg.PerCallTimeout, cfg.RetryMaxAttempts, cfg.MaxConcurrency, cfg.MaxImagesPerRequest, cfg.MaxRequestBytes, cfg.MaxImageReferenceBytes, cfg.MaxResponseBytes, cfg.MaxResultChars, cfg.CacheSize)
	fmt.Fprintf(h, "%d", cfg.CacheTTL)
	return hex.EncodeToString(h.Sum(nil))
}
