package vision

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var (
	ErrHostExecutorUnavailable = errors.New("host model executor is unavailable")
	ErrInvalidResponse         = errors.New("invalid visual model response")
	ErrEmptyResponse           = errors.New("visual model returned empty response")
	ErrResponseTooLarge        = errors.New("visual model response exceeds size limit")
)

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
	if c == nil || c.opts.Execute == nil {
		return "", ErrHostExecutorUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := context.Cause(ctx); err != nil {
		return "", err
	}
	payload := requestPayload{
		Model: c.opts.Model,
		Input: []requestInput{{Role: "user", Content: []requestContent{
			{Type: "input_text", Text: BuildPrompt(focusHint, c.opts.Language)},
			{Type: "input_image", ImageURL: imageReference},
		}}},
		MaxOutputTokens: 4096,
		Stream:          false,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	response, err := c.opts.Execute(ctx, pluginapi.HostModelExecutionRequest{
		EntryProtocol: "openai-response",
		ExitProtocol:  "openai-response",
		Model:         c.opts.Model,
		Stream:        false,
		Body:          body,
		Headers:       http.Header{"Content-Type": []string{"application/json"}},
	}, HostCallbackID(ctx))
	if err != nil {
		return "", ErrHostExecutorUnavailable
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", errors.New("host model execution failed")
	}
	if int64(len(response.Body)) > c.opts.MaxResponseBytes {
		return "", ErrResponseTooLarge
	}
	text, err := parseText(response.Body)
	if err != nil {
		return "", err
	}
	return text, nil
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
