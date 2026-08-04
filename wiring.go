package main

import (
	"errors"

	"github.com/zesuy/Plugin-Deepseek-Vision/internal/config"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/interceptor"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/vision"
)

var pluginRuntime = interceptor.NewRuntime(buildVisionAnalyzer, emitHostDiagnostic)
var hostVisionExecute vision.HostExecuteFunc = executeHostModel

func reconfigureRuntimeWithConfig(cfg *config.Config) {
	pluginRuntime.Reconfigure(cfg)
	SetAfterAuthHandler(pluginRuntime.Handle)
}

func shutdownRuntime() {
	pluginRuntime.Shutdown()
}

func buildVisionAnalyzer(cfg *config.Config) (vision.Analyzer, error) {
	if cfg == nil {
		return nil, errors.New("vision configuration is unavailable")
	}
	return vision.NewHostClient(vision.HostOptions{
		Model:            cfg.VisionModel,
		MaxResponseBytes: int64(cfg.MaxResponseBytes),
		Language:         cfg.Language,
		Execute:          hostVisionExecute,
	})
}
