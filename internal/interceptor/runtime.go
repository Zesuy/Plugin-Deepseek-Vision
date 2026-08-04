// Package interceptor implements the fail-closed AfterAuth request pipeline.
package interceptor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/config"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/responses"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/safety"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/tracelog"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/vision"
)

var ErrRuntimeUnavailable = errors.New("vision interceptor runtime is unavailable")

const HostCallbackIDMetadataKey = "__deepseek_vision_host_callback_id"

// AnalyzerFactory constructs the thin host-model adapter for one immutable
// configuration snapshot. It is initialized lazily only for eligible images.
type AnalyzerFactory func(*config.Config) (vision.Analyzer, error)

// DiagnosticFunc emits bounded, content-free runtime diagnostics through the
// host. callbackID lets CLIProxyAPI attach its request ID when available.
type DiagnosticFunc func(callbackID, level, message string, fields map[string]any)

type runtimeState struct {
	cfg           *config.Config
	factory       AnalyzerFactory
	cache         *analysisCache
	visionLimiter *visionLimiter
	generation    uint64
	once          sync.Once
	analyzer      vision.Analyzer
	err           error
}

type visionLimiter struct {
	mu       sync.Mutex
	limit    int
	inflight int
	changed  chan struct{}
}

func newVisionLimiter(limit int) *visionLimiter {
	if limit < 1 {
		limit = 1
	}
	return &visionLimiter{limit: limit, changed: make(chan struct{})}
}

func (l *visionLimiter) SetLimit(limit int) {
	if l == nil {
		return
	}
	if limit < 1 {
		limit = 1
	}
	l.mu.Lock()
	l.limit = limit
	l.signalLocked()
	l.mu.Unlock()
}

func (l *visionLimiter) Acquire(ctx context.Context) error {
	if l == nil {
		return ErrRuntimeUnavailable
	}
	for {
		l.mu.Lock()
		if l.inflight < l.limit {
			l.inflight++
			l.mu.Unlock()
			return nil
		}
		changed := l.changed
		l.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

func (l *visionLimiter) Release() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if l.inflight > 0 {
		l.inflight--
	}
	l.signalLocked()
	l.mu.Unlock()
}

func (l *visionLimiter) Snapshot() (inflight, limit int) {
	if l == nil {
		return 0, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inflight, l.limit
}

func (l *visionLimiter) signalLocked() {
	close(l.changed)
	l.changed = make(chan struct{})
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
	mu         sync.Mutex
	current    *runtimeState
	factory    AnalyzerFactory
	diagnostic DiagnosticFunc
	trace      *tracelog.Sink
	limiter    *visionLimiter
	generation uint64
	// targetModels is retained across shutdown so the unavailable fallback can
	// preserve the same model gate. Without this snapshot, a shutdown would
	// conservatively reject image requests for every Responses model instead of
	// only the configured DeepSeek targets.
	targetModels []string
	shutdown     bool
}

func NewRuntime(factory AnalyzerFactory, diagnostics ...DiagnosticFunc) *Runtime {
	return NewRuntimeWithTrace(factory, nil, diagnostics...)
}

func NewRuntimeWithTrace(factory AnalyzerFactory, trace *tracelog.Sink, diagnostics ...DiagnosticFunc) *Runtime {
	defaults := config.Default()
	runtime := &Runtime{
		factory:      factory,
		trace:        trace,
		limiter:      newVisionLimiter(defaults.MaxInflightVisionRequests),
		targetModels: append([]string(nil), defaults.TargetModels...),
	}
	if len(diagnostics) > 0 {
		runtime.diagnostic = diagnostics[0]
	}
	return runtime
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
	r.generation++
	r.limiter.SetLimit(cfg.MaxInflightVisionRequests)
	r.targetModels = append([]string(nil), cfg.TargetModels...)
	r.current = &runtimeState{
		cfg: cfg, factory: r.factory, cache: newAnalysisCache(cfg.AnalysisCacheSize),
		visionLimiter: r.limiter, generation: r.generation,
	}
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
	started := time.Now()
	var traceSession *tracelog.Session
	defer func() {
		if recovered := recover(); recovered != nil {
			resp = terminate(http.StatusBadGateway, "vision_preprocess_error", "vision preprocessing failed")
		}
		safeTraceResult(traceSession, started, resp)
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
	traceSession = r.safeStartFullTrace(state, req)
	baseCtx := context.Background()
	if callbackID, _ := req.Metadata[HostCallbackIDMetadataKey].(string); callbackID != "" {
		baseCtx = vision.WithHostCallbackID(baseCtx, callbackID)
	}
	baseCtx = tracelog.WithSession(baseCtx, traceSession)
	ctx, cancel := context.WithTimeout(baseCtx, cfg.RequestTimeout)
	defer cancel()

	plan, planErr := responses.Discover(req.Body, responses.Options{
		MaxImages:         cfg.EmergencyMaxImagesPerRequest,
		MaxReferenceBytes: cfg.MaxImageReferenceBytes,
		MaxBodyBytes:      cfg.MaxRequestBytes,
		MaxResultChars:    cfg.MaxResultChars,
	})
	if ctx.Err() != nil {
		return terminate(http.StatusBadGateway, "vision_preprocess_error", "vision preprocessing deadline exceeded"), nil
	}
	if planErr != nil {
		traceDiscoveryError(traceSession, planErr)
		r.tracePlannerLimit(ctx, state, planErr)
		return terminateForError(planErr), nil
	}
	if !plan.HasImages() {
		traceDiscovery(traceSession, plan)
		return resp, nil
	}

	images := plan.Images()
	groups := plan.Groups()
	traceDiscovery(traceSession, plan)
	results := make([]string, len(groups))
	type analysisJob struct {
		id           int
		key          string
		ttl          time.Duration
		images       []vision.ImageInput
		prompt       string
		indexes      []int
		imageNumbers []int
	}
	jobsByKey := make(map[string]*analysisJob, len(groups))
	jobs := make([]*analysisJob, 0, len(groups))
	cachePlan := cacheTracePlan{Groups: make([]cacheTraceRecord, 0, len(groups))}
	for i := range groups {
		groupImages := visionInputs(groups[i].Images)
		key, ttl := analysisGroupCacheKey(groupImages, cfg.VisionModel, cfg.Language, groups[i].Prompt, cfg.AnalysisCacheTTL, cfg.URLAnalysisCacheTTL)
		imageNumbers := make([]int, len(groupImages))
		for j := range groupImages {
			imageNumbers[j] = groupImages[j].Number
		}
		if cached, ok := state.cache.Get(key); ok {
			results[i] = cached
			cachePlan.CacheHits++
			cachePlan.Groups = append(cachePlan.Groups, cacheTraceRecord{GroupID: groups[i].ID, ImageNumbers: imageNumbers, Disposition: "cache_hit", CacheKey: key})
			continue
		}
		if existing := jobsByKey[key]; existing != nil {
			existing.indexes = append(existing.indexes, i)
			cachePlan.RequestDeduplicates++
			cachePlan.Groups = append(cachePlan.Groups, cacheTraceRecord{GroupID: groups[i].ID, ImageNumbers: imageNumbers, Disposition: "request_deduplicated", JobID: existing.id, CacheKey: key})
			continue
		}
		job := &analysisJob{id: len(jobs) + 1, key: key, ttl: ttl, images: groupImages, prompt: groups[i].Prompt, indexes: []int{i}, imageNumbers: imageNumbers}
		jobsByKey[key] = job
		jobs = append(jobs, job)
		cachePlan.Groups = append(cachePlan.Groups, cacheTraceRecord{GroupID: groups[i].ID, ImageNumbers: imageNumbers, Disposition: "vlm_job", JobID: job.id, CacheKey: key})
	}
	cachePlan.VLMJobs = len(jobs)
	traceCache(traceSession, cachePlan)
	if len(jobs) == 0 {
		rewritten, rewriteErr := plan.RewriteGroupsText(results)
		if rewriteErr != nil {
			traceProcessingError(traceSession, "rewrite_failed", rewriteErr)
			r.tracePlannerLimit(ctx, state, rewriteErr)
			return terminateForError(rewriteErr), nil
		}
		traceRewrite(traceSession, req.Body, rewritten, len(images))
		return pluginapi.RequestInterceptResponse{Headers: req.Headers, Body: rewritten}, nil
	}
	analyzer, analyzerErr := state.getAnalyzer()
	if analyzerErr != nil {
		return terminate(http.StatusBadGateway, "vision_preprocess_error", "vision preprocessing failed"), nil
	}
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	jobQueue := make(chan *analysisJob)
	workerCount := cfg.MaxInflightVisionRequests
	if workerCount > len(jobs) {
		workerCount = len(jobs)
	}
	for worker := 0; worker < workerCount; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobQueue {
				jobCtx := tracelog.WithJob(ctx, tracelog.Job{ID: job.id, ImageNumbers: job.imageNumbers})
				result, analyzeErr := safeAnalyzeGroup(jobCtx, state.visionLimiter, analyzer, job.images, job.prompt, cfg.MaxResultChars)
				if analyzeErr != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = analyzeErr
					}
					errMu.Unlock()
					cancel()
					continue
				}
				state.cache.Set(job.key, result, job.ttl)
				for _, index := range job.indexes {
					results[index] = result
				}
			}
		}()
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			break
		}
		jobQueue <- job
	}
	close(jobQueue)
	wg.Wait()
	errMu.Lock()
	hasErr := firstErr != nil
	errMu.Unlock()
	if hasErr {
		return terminate(http.StatusBadGateway, "vision_preprocess_error", "vision preprocessing failed"), nil
	}
	rewritten, rewriteErr := plan.RewriteGroupsText(results)
	if rewriteErr != nil {
		traceProcessingError(traceSession, "rewrite_failed", rewriteErr)
		r.tracePlannerLimit(ctx, state, rewriteErr)
		return terminateForError(rewriteErr), nil
	}
	traceRewrite(traceSession, req.Body, rewritten, len(images))
	return pluginapi.RequestInterceptResponse{Headers: req.Headers, Body: rewritten}, nil
}

func visionInputs(images []responses.Image) []vision.ImageInput {
	inputs := make([]vision.ImageInput, 0, len(images))
	for i := range images {
		inputs = append(inputs, vision.ImageInput{Number: images[i].Number, Reference: images[i].Reference})
	}
	return inputs
}

func safeAnalyzeGroup(ctx context.Context, limiter *visionLimiter, analyzer vision.Analyzer, images []vision.ImageInput, prompt string, maxResultChars int) (result string, err error) {
	defer func() {
		if recover() != nil {
			result = ""
			err = errors.New("visual analyzer failed")
		}
	}()
	result, err = analyzeGroupAdaptive(ctx, limiter, analyzer, images, prompt)
	if err != nil {
		return "", err
	}
	if err := (safety.Limits{MaxResultChars: maxResultChars}).ValidateResult(result); err != nil {
		return "", err
	}
	return result, nil
}

func analyzeGroupAdaptive(ctx context.Context, limiter *visionLimiter, analyzer vision.Analyzer, images []vision.ImageInput, prompt string) (string, error) {
	if len(images) == 0 {
		return "", errors.New("visual prompt group has no images")
	}
	if batch, ok := analyzer.(vision.BatchAnalyzer); ok {
		result, err := callWithVisionSlot(ctx, limiter, func() (string, error) {
			return batch.AnalyzeBatch(ctx, images, prompt)
		})
		if err == nil || !vision.IsPayloadTooLarge(err) || len(images) == 1 {
			return result, err
		}
		middle := len(images) / 2
		if session := tracelog.FromContext(ctx); session != nil {
			session.Event("vlm_batch_split", map[string]any{
				"reason": "upstream_413", "image_numbers": visionInputNumbers(images),
				"left_count": middle, "right_count": len(images) - middle,
			})
		}
		left, leftErr := analyzeGroupAdaptive(ctx, limiter, analyzer, images[:middle], prompt)
		if leftErr != nil {
			return "", leftErr
		}
		right, rightErr := analyzeGroupAdaptive(ctx, limiter, analyzer, images[middle:], prompt)
		if rightErr != nil {
			return "", rightErr
		}
		return left + "\n\n" + right, nil
	}
	parts := make([]string, 0, len(images))
	for i := range images {
		image := images[i]
		part, err := callWithVisionSlot(ctx, limiter, func() (string, error) {
			return analyzer.Analyze(ctx, image.Reference, prompt)
		})
		if err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("Image %d:\n%s", image.Number, strings.TrimSpace(part)))
	}
	return strings.Join(parts, "\n\n"), nil
}

func visionInputNumbers(images []vision.ImageInput) []int {
	numbers := make([]int, len(images))
	for i := range images {
		numbers[i] = images[i].Number
	}
	return numbers
}

func callWithVisionSlot(ctx context.Context, limiter *visionLimiter, call func() (string, error)) (string, error) {
	queuedAt := time.Now()
	if err := limiter.Acquire(ctx); err != nil {
		if session := tracelog.FromContext(ctx); session != nil {
			session.Event("vision_slot_cancelled", map[string]any{"queue_ms": time.Since(queuedAt).Milliseconds(), "error": err.Error()})
		}
		return "", err
	}
	inflight, limit := limiter.Snapshot()
	if session := tracelog.FromContext(ctx); session != nil {
		session.Event("vision_slot_acquired", map[string]any{
			"queue_ms": time.Since(queuedAt).Milliseconds(), "inflight": inflight, "limit": limit,
		})
	}
	defer func() {
		limiter.Release()
		if session := tracelog.FromContext(ctx); session != nil {
			remaining, currentLimit := limiter.Snapshot()
			session.Event("vision_slot_released", map[string]any{"inflight": remaining, "limit": currentLimit})
		}
	}()
	return call()
}

func (r *Runtime) targetModelSnapshot() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.targetModels...)
}

func (r *Runtime) tracePlannerLimit(ctx context.Context, state *runtimeState, err error) {
	if r == nil || state == nil || r.diagnostic == nil {
		return
	}
	var planner *responses.Error
	if !errors.As(err, &planner) || planner.StatusCode != http.StatusRequestEntityTooLarge {
		return
	}
	fields := map[string]any{
		"limit_kind":                       string(planner.Limit),
		"actual":                           planner.Actual,
		"maximum":                          planner.Maximum,
		"config_generation":                state.generation,
		"max_request_bytes":                state.cfg.MaxRequestBytes,
		"max_image_reference_bytes":        state.cfg.MaxImageReferenceBytes,
		"emergency_max_images_per_request": state.cfg.EmergencyMaxImagesPerRequest,
		"max_inflight_vision_requests":     state.cfg.MaxInflightVisionRequests,
	}
	message := fmt.Sprintf(
		"deepseek-vision request rejected limit_kind=%s actual=%d maximum=%d config_generation=%d max_request_bytes=%d max_image_reference_bytes=%d emergency_max_images_per_request=%d",
		planner.Limit, planner.Actual, planner.Maximum, state.generation,
		state.cfg.MaxRequestBytes, state.cfg.MaxImageReferenceBytes, state.cfg.EmergencyMaxImagesPerRequest,
	)
	safeDiagnostic(r.diagnostic, vision.HostCallbackID(ctx), "warn", message, fields)
}

func safeDiagnostic(diagnostic DiagnosticFunc, callbackID, level, message string, fields map[string]any) {
	defer func() { _ = recover() }()
	diagnostic(callbackID, level, message, fields)
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
			return terminate(http.StatusRequestEntityTooLarge, "invalid_request_error", publicLimitMessage(planner.Limit, planner.Actual, planner.Maximum))
		case http.StatusUnprocessableEntity:
			return terminate(http.StatusUnprocessableEntity, "invalid_request_error", "unsupported image source")
		}
	}
	return terminate(http.StatusBadGateway, "vision_preprocess_error", "vision preprocessing failed")
}

func publicLimitMessage(limit responses.LimitKind, actual, maximum int) string {
	switch limit {
	case responses.LimitRequestBody:
		if actual > 0 && maximum > 0 {
			return fmt.Sprintf("request body exceeds configured limit: found %d bytes, limit %d", actual, maximum)
		}
		return "request body exceeds configured limit"
	case responses.LimitImageReference:
		if actual > 0 && maximum > 0 {
			return fmt.Sprintf("image reference exceeds configured limit: found %d bytes, limit %d", actual, maximum)
		}
		return "image reference exceeds configured limit"
	case responses.LimitImageCount:
		if actual > 0 && maximum > 0 {
			return fmt.Sprintf("request contains too many unique images: found %d, emergency limit %d", actual, maximum)
		}
		return "request contains too many images"
	case responses.LimitVLMResult:
		if actual > 0 && maximum > 0 {
			return fmt.Sprintf("vision result exceeds configured limit: found %d characters, limit %d", actual, maximum)
		}
		return "vision result exceeds configured limit"
	default:
		return "request exceeds configured limit"
	}
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
