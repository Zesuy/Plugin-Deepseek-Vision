package vision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/tracelog"
)

var (
	ErrHostExecutorUnavailable = errors.New("host model executor is unavailable")
	ErrInvalidResponse         = errors.New("invalid visual model response")
	ErrEmptyResponse           = errors.New("visual model returned empty response")
	ErrResponseTooLarge        = errors.New("visual model response exceeds size limit")
	ErrEmptyImageBatch         = errors.New("visual model image batch is empty")
)

type HTTPStatusError struct{ StatusCode int }

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("host model execution returned status %d", e.StatusCode)
}

func IsPayloadTooLarge(err error) bool {
	var status *HTTPStatusError
	return errors.As(err, &status) && status.StatusCode == http.StatusRequestEntityTooLarge
}

type HostExecuteFunc func(context.Context, pluginapi.HostModelExecutionRequest, string) (pluginapi.HostModelExecutionResponse, error)

type HostOptions struct {
	Model            string
	MaxResponseBytes int64
	Language         string
	Execute          HostExecuteFunc
}

type HostClient struct {
	opts HostOptions
}

type hostCallbackIDKey struct{}

func WithHostCallbackID(ctx context.Context, callbackID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if callbackID == "" {
		return ctx
	}
	return context.WithValue(ctx, hostCallbackIDKey{}, callbackID)
}

func HostCallbackID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(hostCallbackIDKey{}).(string)
	return value
}

func NewHostClient(opts HostOptions) (*HostClient, error) {
	if opts.Execute == nil {
		return nil, ErrHostExecutorUnavailable
	}
	if opts.Model == "" {
		opts.Model = "gpt-5.6-luna"
	}
	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = 4 << 20
	}
	opts.Language = NormalizeLanguage(opts.Language)
	return &HostClient{opts: opts}, nil
}

func (c *HostClient) Analyze(ctx context.Context, imageReference, focusHint string) (string, error) {
	return c.AnalyzeBatch(ctx, []ImageInput{{Number: 1, Reference: imageReference}}, focusHint)
}

func (c *HostClient) AnalyzeBatch(ctx context.Context, images []ImageInput, promptContext string) (string, error) {
	if c == nil || c.opts.Execute == nil {
		return "", ErrHostExecutorUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := context.Cause(ctx); err != nil {
		return "", err
	}
	if len(images) == 0 {
		return "", ErrEmptyImageBatch
	}
	session := tracelog.FromContext(ctx)
	job := tracelog.JobFromContext(ctx)
	prefix := fmt.Sprintf("40-vlm-job-%03d-images-%s", job.ID, imageNumberToken(images))
	referenceBytes := 0
	imageNumbers := make([]int, len(images))
	for i := range images {
		referenceBytes += len(images[i].Reference)
		imageNumbers[i] = images[i].Number
	}
	if session != nil {
		session.JSON(prefix+"-metadata.json", map[string]any{
			"job_id": job.ID, "image_numbers": imageNumbers,
			"images": images, "reference_bytes": referenceBytes,
			"prompt_context": promptContext, "vision_model": c.opts.Model, "language": c.opts.Language,
		})
	}
	content := []requestContent{{Type: "input_text", Text: BuildPrompt(promptContext, c.opts.Language)}}
	for i := range images {
		content = append(content,
			requestContent{Type: "input_text", Text: fmt.Sprintf("Image %d:", i+1)},
			requestContent{Type: "input_image", ImageURL: images[i].Reference},
		)
	}
	payload := requestPayload{
		Model: c.opts.Model, Input: []requestInput{{Role: "user", Content: content}},
		MaxOutputTokens: 4096,
		Stream:          false,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	started := time.Now()
	if session != nil {
		session.Artifact(prefix+"-request.json", body)
		session.Event("vlm_call_started", map[string]any{
			"job_id": job.ID, "image_numbers": imageNumbers,
			"request_bytes": len(body), "reference_bytes": referenceBytes, "image_count": len(images),
		})
	}
	hostRequest := pluginapi.HostModelExecutionRequest{
		EntryProtocol: "openai-response",
		ExitProtocol:  "openai-response",
		Model:         c.opts.Model,
		Stream:        false,
		Body:          body,
		Headers:       http.Header{"Content-Type": []string{"application/json"}},
	}
	if session != nil {
		session.JSON(prefix+"-request-metadata.json", map[string]any{
			"entry_protocol": hostRequest.EntryProtocol, "exit_protocol": hostRequest.ExitProtocol,
			"model": hostRequest.Model, "stream": hostRequest.Stream,
			"headers": tracelog.RedactHeaders(hostRequest.Headers), "query": tracelog.RedactValues(hostRequest.Query), "alt": hostRequest.Alt,
		})
	}
	response, err := c.opts.Execute(ctx, hostRequest, HostCallbackID(ctx))
	if err != nil {
		if session != nil {
			session.Event("vlm_call_finished", map[string]any{
				"job_id": job.ID, "duration_ms": time.Since(started).Milliseconds(),
				"outcome": "executor_error", "error": err.Error(),
			})
		}
		return "", ErrHostExecutorUnavailable
	}
	if session != nil {
		session.Artifact(prefix+"-response.json", response.Body)
		session.JSON(prefix+"-response-metadata.json", map[string]any{
			"status_code": response.StatusCode, "headers": tracelog.RedactHeaders(response.Headers),
			"body_bytes": len(response.Body), "duration_ms": time.Since(started).Milliseconds(),
		})
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if session != nil {
			session.Event("vlm_call_finished", map[string]any{
				"job_id": job.ID, "duration_ms": time.Since(started).Milliseconds(),
				"outcome": "http_error", "status_code": response.StatusCode,
			})
		}
		return "", &HTTPStatusError{StatusCode: response.StatusCode}
	}
	if int64(len(response.Body)) > c.opts.MaxResponseBytes {
		if session != nil {
			session.Event("vlm_call_finished", map[string]any{
				"job_id": job.ID, "duration_ms": time.Since(started).Milliseconds(),
				"outcome": "response_too_large", "response_bytes": len(response.Body),
			})
		}
		return "", ErrResponseTooLarge
	}
	text, err := parseText(response.Body)
	if err != nil {
		if session != nil {
			session.Event("vlm_call_finished", map[string]any{
				"job_id": job.ID, "duration_ms": time.Since(started).Milliseconds(),
				"outcome": "parse_error", "error": err.Error(),
			})
		}
		return "", err
	}
	if session != nil {
		session.Artifact(prefix+"-parsed-result.txt", []byte(text))
		session.Event("vlm_call_finished", map[string]any{
			"job_id": job.ID, "duration_ms": time.Since(started).Milliseconds(),
			"outcome": "success", "response_bytes": len(response.Body), "result_chars": len([]rune(text)),
		})
	}
	return text, nil
}

func imageNumberToken(images []ImageInput) string {
	if len(images) == 0 {
		return "none"
	}
	if len(images) > 6 {
		return fmt.Sprintf("%d-%d-count-%d", images[0].Number, images[len(images)-1].Number, len(images))
	}
	parts := make([]string, len(images))
	for i := range images {
		parts[i] = fmt.Sprintf("%d", images[i].Number)
	}
	return strings.Join(parts, "-")
}

type requestPayload struct {
	Model           string         `json:"model"`
	Input           []requestInput `json:"input"`
	MaxOutputTokens int            `json:"max_output_tokens"`
	Stream          bool           `json:"stream"`
}

type requestInput struct {
	Role    string           `json:"role"`
	Content []requestContent `json:"content"`
}

type requestContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// parseText owns only the stable Responses output boundary selected for the
// host callback. Provider-specific response translation remains in CLIProxyAPI.
func parseText(data []byte) (string, error) {
	var raw struct {
		OutputText string            `json:"output_text"`
		Output     []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", ErrInvalidResponse
	}
	if text := strings.TrimSpace(raw.OutputText); text != "" {
		return text, nil
	}
	var parts []string
	for _, item := range raw.Output {
		var message struct {
			Type    string            `json:"type"`
			Content []json.RawMessage `json:"content"`
		}
		if json.Unmarshal(item, &message) != nil || message.Type == "reasoning" {
			continue
		}
		for _, content := range message.Content {
			var block struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(content, &block) == nil && block.Type == "output_text" && strings.TrimSpace(block.Text) != "" {
				parts = append(parts, strings.TrimSpace(block.Text))
			}
		}
	}
	if len(parts) == 0 {
		return "", ErrEmptyResponse
	}
	return strings.Join(parts, "\n"), nil
}
