package vision

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/safety"
)

var ErrHostExecutorUnavailable = errors.New("host model executor is unavailable")

type HostExecuteFunc func(context.Context, pluginapi.HostModelExecutionRequest, string) (pluginapi.HostModelExecutionResponse, error)

type HostOptions struct {
	Model                  string
	MaxResponseBytes       int64
	MaxResultChars         int
	MaxImageReferenceBytes int
	Language               string
	Execute                HostExecuteFunc
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
	if opts.MaxResultChars <= 0 {
		opts.MaxResultChars = 20000
	}
	if opts.MaxImageReferenceBytes <= 0 {
		opts.MaxImageReferenceBytes = 16 << 20
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
	if err := safety.ValidateImageReference(imageReference, c.opts.MaxImageReferenceBytes); err != nil {
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
	if len([]rune(text)) > c.opts.MaxResultChars {
		return "", ErrResponseTooLarge
	}
	return text, nil
}

func (c *HostClient) Close() error { return nil }
