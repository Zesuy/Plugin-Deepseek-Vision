package vision

import (
	"context"
)

// Analyzer is the backward-compatible single-image seam used by simple test
// adapters. Production analyzers should also implement BatchAnalyzer.
type Analyzer interface {
	Analyze(ctx context.Context, imageReference, focusHint string) (string, error)
}

// ImageInput identifies one image inside a prompt-level visual request.
type ImageInput struct {
	Number    int    `json:"number"`
	Reference string `json:"reference"`
}

// BatchAnalyzer is implemented by analyzers that can preserve relationships
// between all images attached to one prompt in a single model request.
type BatchAnalyzer interface {
	AnalyzeBatch(ctx context.Context, images []ImageInput, prompt string) (string, error)
}
