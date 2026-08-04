package main

import (
	"errors"
	"fmt"

	"github.com/zesuy/Plugin-Deepseek-Vision/internal/config"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/interceptor"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/tracelog"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/vision"
)

var fullContextTrace = tracelog.New(tracelog.Options{
	Root: tracelog.DefaultRoot,
	ReportError: func(err error) {
		emitHostDiagnostic("", "warn", fmt.Sprintf("deepseek-vision full-context trace disabled after write failure: %v", err), nil)
	},
})
var pluginRuntime = interceptor.NewRuntimeWithTrace(buildVisionAnalyzer, fullContextTrace, emitHostDiagnostic)
var hostVisionExecute vision.HostExecuteFunc = executeHostModel

func reconfigureRuntimeWithConfig(cfg *config.Config) {
	fullContextTrace.Configure(cfg != nil && cfg.TraceEnabled)
	pluginRuntime.Reconfigure(cfg)
	SetAfterAuthHandler(pluginRuntime.Handle)
}

func shutdownRuntime() {
	pluginRuntime.Shutdown()
	fullContextTrace.Close()
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
