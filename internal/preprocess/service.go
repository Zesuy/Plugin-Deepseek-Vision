// Package preprocess coordinates bounded, one-call-per-image VLM analysis.
package preprocess

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/zesuy/Plugin-Deepseek-Vision/internal/cache"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/safety"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/vision"
)

var (
	ErrServiceClosed = errors.New("vision preprocessing service is closed")
	ErrNoAnalyzer    = errors.New("vision analyzer is not configured")
	ErrAnalyzerPanic = errors.New("visual analyzer failed")
)

type Image struct {
	Reference string
}

// Limiter can be shared by multiple Service generations so hot reconfigure
// does not temporarily multiply the global number of analyzer calls.
type Limiter interface {
	Acquire(context.Context) error
	Release()
}

type Options struct {
	Analyzer               vision.Analyzer
	Cache                  *cache.Cache
	CacheCapacity          int
	CacheTTL               time.Duration
	MaxConcurrency         int
	MaxImages              int
	MaxImageReferenceBytes int
	MaxResultChars         int
	Model                  string
	PromptVersion          string
	ConfigGeneration       string
	Language               string
	Limiter                Limiter
}

type call struct {
	done   chan struct{}
	result string
	err    error
}

type Service struct {
	analyzer      vision.Analyzer
	cache         *cache.Cache
	limits        safety.Limits
	sem           chan struct{}
	limiter       Limiter
	model         string
	promptVersion string
	generation    string
	language      string
	rootCtx       context.Context
	cancelRoot    context.CancelFunc

	mu        sync.Mutex
	closed    bool
	inflight  map[string]*call
	workers   sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

func NewService(opts Options) (*Service, error) {
	if opts.Analyzer == nil {
		return nil, ErrNoAnalyzer
	}
	if opts.MaxConcurrency <= 0 {
		opts.MaxConcurrency = 4
	}
	if opts.MaxImages <= 0 {
		opts.MaxImages = 4
	}
	if opts.MaxImageReferenceBytes <= 0 {
		opts.MaxImageReferenceBytes = 16 << 20
	}
	if opts.MaxResultChars <= 0 {
		opts.MaxResultChars = 20000
	}
	if opts.Cache == nil && opts.CacheCapacity > 0 {
		opts.Cache = cache.NewLRU(opts.CacheCapacity, opts.CacheTTL)
	}
	rootCtx, cancelRoot := context.WithCancel(context.Background())
	return &Service{
		analyzer:      opts.Analyzer,
		cache:         opts.Cache,
		limits:        safety.Limits{MaxImages: opts.MaxImages, MaxImageReferenceBytes: opts.MaxImageReferenceBytes, MaxResultChars: opts.MaxResultChars},
		sem:           make(chan struct{}, opts.MaxConcurrency),
		limiter:       opts.Limiter,
		model:         opts.Model,
		promptVersion: opts.PromptVersion,
		generation:    opts.ConfigGeneration,
		language:      vision.NormalizeLanguage(opts.Language),
		rootCtx:       rootCtx,
		cancelRoot:    cancelRoot,
		inflight:      make(map[string]*call),
	}, nil
}

// AnalyzeAll runs one VLM analysis for each image and preserves input order.
// Any failure cancels sibling work and returns no partial results.
func (s *Service) AnalyzeAll(ctx context.Context, images []Image, focusHint string) ([]string, error) {
	if s == nil {
		return nil, ErrServiceClosed
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, ErrServiceClosed
	}
	if err := s.limits.ValidateImageCount(len(images)); err != nil {
		return nil, err
	}
	if len(images) == 0 {
		return []string{}, nil
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make([]string, len(images))
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	for i := range images {
		idx := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := s.AnalyzeOne(child, images[idx], focusHint)
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				errMu.Unlock()
				return
			}
			results[idx] = result
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return results, nil
}

// Process is an alias useful to interceptor adapters.
func (s *Service) Process(ctx context.Context, images []Image, focusHint string) ([]string, error) {
	return s.AnalyzeAll(ctx, images, focusHint)
}

// AnalyzeImages is an explicit alias used by request interceptors.
func (s *Service) AnalyzeImages(ctx context.Context, images []Image, focusHint string) ([]string, error) {
	return s.AnalyzeAll(ctx, images, focusHint)
}

func (s *Service) AnalyzeOne(ctx context.Context, image Image, focusHint string) (string, error) {
	// Lifecycle state takes precedence over input validation. Once Close has
	// returned, callers must receive a stable ErrServiceClosed regardless of
	// whether the supplied image reference is malformed or over a limit.
	if s == nil {
		return "", ErrServiceClosed
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return "", ErrServiceClosed
	}
	if err := s.limits.ValidateImageReference(image.Reference); err != nil {
		return "", err
	}
	// The analyzer bounds focusHint at 2000 runes. Use the normalized prompt in
	// the key so distinct oversized hints that produce the same request still
	// coalesce and cache together.
	key := cache.Key(image.Reference, s.model, vision.BuildPrompt(focusHint, s.language), s.language, s.promptVersion, s.generation)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", ErrServiceClosed
	}
	if s.cache != nil {
		if result, ok := s.cache.Get(key); ok {
			s.mu.Unlock()
			return result, nil
		}
	}
	if existing, ok := s.inflight[key]; ok {
		s.mu.Unlock()
		return waitForCall(ctx, existing)
	}
	current := &call{done: make(chan struct{})}
	s.inflight[key] = current
	s.workers.Add(1)
	workCtx := vision.WithHostCallbackID(s.rootCtx, vision.HostCallbackID(ctx))
	go s.produce(workCtx, key, current, image.Reference, focusHint)
	s.mu.Unlock()
	return waitForCall(ctx, current)
}

func waitForCall(ctx context.Context, current *call) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-current.done:
		return current.result, current.err
	}
}

func (s *Service) produce(ctx context.Context, key string, current *call, reference, focusHint string) {
	defer s.workers.Done()
	result, err := s.safeRun(ctx, reference, focusHint)

	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, key)
	if s.closed {
		// A worker finishing after shutdown may not publish a cached value or a
		// success to any waiter. Close waits until this publication completes.
		result = ""
		err = ErrServiceClosed
	} else if err == nil && s.cache != nil {
		s.cache.Set(key, result)
	}
	current.result, current.err = result, err
	close(current.done)
}

func (s *Service) safeRun(ctx context.Context, reference, focusHint string) (result string, err error) {
	defer func() {
		if recover() != nil {
			result = ""
			err = ErrAnalyzerPanic
		}
	}()
	return s.run(ctx, reference, focusHint)
}

func (s *Service) run(ctx context.Context, reference, focusHint string) (string, error) {
	if s.limiter != nil {
		if err := s.limiter.Acquire(ctx); err != nil {
			return "", err
		}
		defer s.limiter.Release()
	} else {
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	result, err := s.analyzer.Analyze(ctx, reference, focusHint)
	if err != nil {
		return "", fmt.Errorf("visual analysis failed: %w", err)
	}
	if err := s.limits.ValidateResult(result); err != nil {
		return "", err
	}
	return result, nil
}

func (s *Service) ClearCache() {
	if s != nil && s.cache != nil {
		s.cache.Clear()
	}
}

func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.cancelRoot()
		s.mu.Unlock()
		s.workers.Wait()
		if closer, ok := s.analyzer.(interface{ Close() error }); ok {
			s.closeErr = closer.Close()
		}
		if s.cache != nil {
			s.cache.Clear()
		}
	})
	return s.closeErr
}
