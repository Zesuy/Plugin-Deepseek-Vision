package interceptor

import (
	"errors"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/responses"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/tracelog"
)

type cacheTraceRecord struct {
	GroupID      int    `json:"group_id"`
	ImageNumbers []int  `json:"image_numbers"`
	Disposition  string `json:"disposition"`
	JobID        int    `json:"job_id,omitempty"`
	CacheKey     string `json:"cache_key"`
}

type cacheTracePlan struct {
	Groups              []cacheTraceRecord `json:"groups"`
	CacheHits           int                `json:"cache_hits"`
	RequestDeduplicates int                `json:"request_deduplicates"`
	VLMJobs             int                `json:"vlm_jobs"`
}

func (r *Runtime) startFullTrace(state *runtimeState, req pluginapi.RequestInterceptRequest) *tracelog.Session {
	if r == nil || r.trace == nil || state == nil || state.cfg == nil || !state.cfg.TraceEnabled {
		return nil
	}
	session := r.trace.Start(tracelog.RequestMeta{
		RequestID: req.RequestID, TraceID: req.TraceID, ConfigGeneration: state.generation,
	})
	if session == nil {
		return nil
	}
	safeMetadata := tracelog.RedactJSONLike(req.Metadata, "metadata")
	if metadataMap, ok := safeMetadata.(map[string]any); ok {
		if _, exists := metadataMap[HostCallbackIDMetadataKey]; exists {
			metadataMap[HostCallbackIDMetadataKey] = "[REDACTED]"
		}
	}
	metadata := map[string]any{
		"request": map[string]any{
			"request_id": req.RequestID, "trace_id": req.TraceID,
			"source_format": req.SourceFormat, "to_format": req.ToFormat,
			"model": req.Model, "requested_model": req.RequestedModel,
			"stream": req.Stream, "body_bytes": len(req.Body),
			"headers":  tracelog.RedactHeaders(req.Headers),
			"metadata": safeMetadata,
		},
		"config": map[string]any{
			"generation":    state.generation,
			"target_models": append([]string(nil), state.cfg.TargetModels...),
			"vision_model":  state.cfg.VisionModel, "language": state.cfg.Language,
			"request_timeout":                  state.cfg.RequestTimeout.String(),
			"emergency_max_images_per_request": state.cfg.EmergencyMaxImagesPerRequest,
			"max_inflight_vision_requests":     state.cfg.MaxInflightVisionRequests,
			"max_request_bytes":                state.cfg.MaxRequestBytes,
			"max_image_reference_bytes":        state.cfg.MaxImageReferenceBytes,
			"max_response_bytes":               state.cfg.MaxResponseBytes,
			"max_result_chars":                 state.cfg.MaxResultChars,
			"analysis_cache_size":              state.cfg.AnalysisCacheSize,
			"analysis_cache_ttl":               state.cfg.AnalysisCacheTTL.String(),
			"analysis_url_cache_ttl":           state.cfg.URLAnalysisCacheTTL.String(),
			"trace_enabled":                    state.cfg.TraceEnabled,
		},
	}
	session.JSON("00-metadata.json", metadata)
	session.Artifact("10-inbound-body.json", req.Body)
	session.Event("request_received", map[string]any{
		"body_bytes": len(req.Body), "model": req.Model, "stream": req.Stream,
	})
	return session
}

func (r *Runtime) safeStartFullTrace(state *runtimeState, req pluginapi.RequestInterceptRequest) (session *tracelog.Session) {
	defer func() {
		if recover() != nil {
			session = nil
		}
	}()
	return r.startFullTrace(state, req)
}

func traceDiscovery(session *tracelog.Session, plan *responses.Plan) {
	if session == nil || plan == nil {
		return
	}
	images := plan.Images()
	session.JSON("20-discovery.json", map[string]any{
		"summary": plan.ImageCountDetails(),
		"images":  images,
		"groups":  plan.Groups(),
	})
	session.Event("discovery_completed", plan.ImageCountDetails())
}

func traceDiscoveryError(session *tracelog.Session, err error) {
	if session == nil || err == nil {
		return
	}
	var planner *responses.Error
	if !errors.As(err, &planner) {
		session.JSON("20-discovery-error.json", map[string]any{"error": err.Error()})
		session.Event("discovery_failed", map[string]any{"error": err.Error()})
		return
	}
	details := map[string]any{
		"kind": planner.Kind, "status_code": planner.StatusCode,
		"message": planner.Message, "path": planner.Path,
		"limit_kind": planner.Limit, "actual": planner.Actual, "maximum": planner.Maximum,
	}
	if planner.ImageCount != nil {
		details["image_count"] = planner.ImageCount
	}
	eventDetails := make(map[string]any, len(details))
	for key, value := range details {
		eventDetails[key] = value
	}
	if len(planner.DebugImages) > 0 {
		details["images"] = planner.DebugImages
	}
	session.JSON("20-discovery-error.json", details)
	session.Event("discovery_rejected", eventDetails)
}

func traceCache(session *tracelog.Session, plan cacheTracePlan) {
	if session == nil {
		return
	}
	session.JSON("30-cache-plan.json", plan)
	session.Event("cache_plan", map[string]any{
		"cache_hits":           plan.CacheHits,
		"request_deduplicates": plan.RequestDeduplicates,
		"vlm_jobs":             plan.VLMJobs,
	})
}

func traceRewrite(session *tracelog.Session, original, rewritten []byte, replacements int) {
	if session == nil || len(rewritten) == 0 {
		return
	}
	session.Artifact("50-rewritten-body.json", rewritten)
	session.Event("rewrite_completed", map[string]any{
		"input_bytes": len(original), "output_bytes": len(rewritten), "replacements": replacements,
	})
}

func traceProcessingError(session *tracelog.Session, event string, err error) {
	if session == nil || err == nil {
		return
	}
	data := map[string]any{"error": err.Error()}
	session.JSON("80-processing-error.json", map[string]any{"event": event, "error": err.Error()})
	session.Event(event, data)
}

func traceResult(session *tracelog.Session, started time.Time, resp pluginapi.RequestInterceptResponse) {
	if session == nil {
		return
	}
	result := map[string]any{
		"duration_ms": time.Since(started).Milliseconds(),
		"terminate":   resp.Terminate, "status_code": resp.StatusCode,
		"body_bytes": len(resp.Body), "response_body_bytes": len(resp.ResponseBody),
		"headers":          tracelog.RedactHeaders(resp.Headers),
		"response_headers": tracelog.RedactHeaders(resp.ResponseHeaders),
	}
	if len(resp.ResponseBody) > 0 {
		result["response_body"] = string(resp.ResponseBody)
	}
	session.JSON("90-result.json", result)
	session.Event("interceptor_completed", result)
	session.Close()
}

func safeTraceResult(session *tracelog.Session, started time.Time, resp pluginapi.RequestInterceptResponse) {
	defer func() { _ = recover() }()
	traceResult(session, started, resp)
}
