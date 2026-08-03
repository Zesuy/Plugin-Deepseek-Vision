package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/zesuy/Plugin-Deepseek-Vision/internal/cache"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/config"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/interceptor"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/preprocess"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/vision"
)

var pluginRuntime = interceptor.NewRuntime(buildVisionService)
var hostVisionExecute vision.HostExecuteFunc = executeHostModel

func reconfigureRuntimeWithConfig(cfg *config.Config) {
	pluginRuntime.Reconfigure(cfg)
	SetAfterAuthHandler(pluginRuntime.Handle)
}

func shutdownRuntime() {
	pluginRuntime.Shutdown()
}

func buildVisionService(cfg *config.Config, generation uint64, limiter preprocess.Limiter) (*preprocess.Service, error) {
	if cfg == nil {
		return nil, errors.New("vision configuration is unavailable")
	}
	var analyzer vision.Analyzer
	var err error
	if cfg.VisionBackend == config.VisionBackendExternal {
		token := strings.TrimSpace(os.Getenv(cfg.VisionAPIKeyEnv))
		if token == "" {
			return nil, errors.New("vision API key environment variable is unavailable")
		}
		analyzer, err = vision.NewClient(vision.Options{
			BaseURL:                cfg.VisionBaseURL,
			Model:                  cfg.VisionModel,
			Token:                  token,
			RequestTimeout:         cfg.PerCallTimeout,
			MaxResponseBytes:       int64(cfg.MaxResponseBytes),
			MaxResultChars:         cfg.MaxResultChars,
			MaxAttempts:            cfg.RetryMaxAttempts,
			MaxImageReferenceBytes: cfg.MaxImageReferenceBytes,
			ConfigGeneration:       fmt.Sprintf("%d", generation),
			Language:               cfg.Language,
		})
	} else {
		analyzer, err = vision.NewHostClient(vision.HostOptions{
			Model:                  cfg.VisionModel,
			MaxResponseBytes:       int64(cfg.MaxResponseBytes),
			MaxResultChars:         cfg.MaxResultChars,
			MaxImageReferenceBytes: cfg.MaxImageReferenceBytes,
			Language:               cfg.Language,
			Execute:                hostVisionExecute,
		})
	}
	if err != nil {
		return nil, err
	}
	service, err := preprocess.NewService(preprocess.Options{
		Analyzer:               analyzer,
		Cache:                  cache.NewLRU(cfg.CacheSize, cfg.CacheTTL),
		MaxConcurrency:         cfg.MaxConcurrency,
		MaxImages:              cfg.MaxImagesPerRequest,
		MaxImageReferenceBytes: cfg.MaxImageReferenceBytes,
		MaxResultChars:         cfg.MaxResultChars,
		Model:                  cfg.VisionModel,
		PromptVersion:          vision.DefaultPrompt(),
		ConfigGeneration:       fmt.Sprintf("%d", generation),
		Language:               cfg.Language,
		Limiter:                limiter,
	})
	if err != nil {
		if closer, ok := analyzer.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
		return nil, err
	}
	return service, nil
}
