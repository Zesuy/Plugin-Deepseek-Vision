// Package config parses and publishes immutable deepseek-vision configuration
// snapshots. The host owns enabled/priority, model routing, credentials,
// provider transport, and retry policy.
package config

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultVisionModel               = "gpt-5.6-luna"
	DefaultLanguage                  = "zh"
	DefaultRequestTimeoutSec         = 120
	DefaultEmergencyMaxImages        = 256
	DefaultMaxInflightVisionRequests = 4
	DefaultMaxRequestBytes           = 20 * 1024 * 1024
	DefaultMaxImageRefBytes          = 15 * 1024 * 1024
	DefaultMaxResponseBytes          = 4 * 1024 * 1024
	DefaultMaxResultChars            = 20_000
	DefaultAnalysisCacheSize         = 128
	DefaultAnalysisCacheTTL          = 15 * 60
	DefaultURLCacheTTL               = 2 * 60
	// MaxRequestBytesLimit is the largest raw Responses body accepted by
	// configuration. The native ABI separately accounts for envelope overhead.
	MaxRequestBytesLimit = 32 * 1024 * 1024
)

// Config is an immutable, validated runtime snapshot. Callers must treat
// TargetModels as read-only.
type Config struct {
	TargetModels                 []string
	VisionModel                  string
	Language                     string
	RequestTimeout               time.Duration
	EmergencyMaxImagesPerRequest int
	MaxInflightVisionRequests    int
	MaxRequestBytes              int
	MaxImageReferenceBytes       int
	MaxResponseBytes             int
	MaxResultChars               int
	AnalysisCacheSize            int
	AnalysisCacheTTL             time.Duration
	URLAnalysisCacheTTL          time.Duration
	TraceEnabled                 bool
}

// rawConfig mirrors the active YAML contract. Deprecated fields remain
// decodable for a smooth upgrade from the pre-host client, but their values
// are never interpreted and cannot influence execution.
type rawConfig struct {
	Enabled  *bool     `yaml:"enabled"`
	Priority *int      `yaml:"priority"`
	Store    yaml.Node `yaml:"store"`

	TargetModels                 []string `yaml:"target_models"`
	VisionModel                  string   `yaml:"vision_model"`
	Language                     string   `yaml:"language"`
	RequestTimeoutSeconds        int      `yaml:"request_timeout_seconds"`
	EmergencyMaxImagesPerRequest int      `yaml:"emergency_max_images_per_request"`
	MaxInflightVisionRequests    int      `yaml:"max_inflight_vision_requests"`
	MaxRequestBytes              int      `yaml:"max_request_bytes"`
	MaxImageReferenceBytes       int      `yaml:"max_image_reference_bytes"`
	MaxResponseBytes             int      `yaml:"max_response_bytes"`
	MaxResultChars               int      `yaml:"max_result_chars"`
	AnalysisCacheSize            int      `yaml:"analysis_cache_size"`
	AnalysisCacheTTL             int      `yaml:"analysis_cache_ttl_seconds"`
	URLAnalysisCacheTTL          int      `yaml:"analysis_url_cache_ttl_seconds"`
	TraceEnabled                 bool     `yaml:"trace_enabled"`

	// Deprecated host-client fields, accepted and unconditionally ignored.
	VisionBackend         yaml.Node `yaml:"vision_backend"`
	VisionBaseURL         yaml.Node `yaml:"vision_base_url"`
	VisionAPIKeyEnv       yaml.Node `yaml:"vision_api_key_env"`
	PerCallTimeoutSeconds yaml.Node `yaml:"per_call_timeout_seconds"`
	RetryMaxAttempts      yaml.Node `yaml:"retry_max_attempts"`
	MaxConcurrency        yaml.Node `yaml:"max_concurrency"`
	CacheSize             yaml.Node `yaml:"cache_size"`
	CacheTTLSeconds       yaml.Node `yaml:"cache_ttl_seconds"`
	MaxImagesPerRequest   yaml.Node `yaml:"max_images_per_request"`
}

type hostDocument struct {
	Enabled *bool                `yaml:"enabled"`
	Store   yaml.Node            `yaml:"store"`
	Configs map[string]yaml.Node `yaml:"configs"`
}

type wrappedDocument struct {
	Plugins hostDocument `yaml:"plugins"`
}

var current atomic.Pointer[Config]

func init() { current.Store(defaultConfig()) }

func Default() *Config { return defaultConfig() }

func Snapshot() *Config { return cloneConfig(current.Load()) }

func ConfigureYAML(raw []byte) error {
	cfg, err := ParseYAML(raw)
	if err != nil {
		return err
	}
	return PublishValidated(cfg)
}

func PublishValidated(cfg *Config) error {
	if cfg == nil {
		return errors.New("configuration must not be nil")
	}
	current.Store(cloneConfig(cfg))
	return nil
}

func Shutdown() { current.Store(nil) }

func cloneConfig(cfg *Config) *Config {
	if cfg == nil {
		return nil
	}
	clone := *cfg
	clone.TargetModels = append([]string(nil), cfg.TargetModels...)
	return &clone
}

// ParseYAML accepts either the direct plugin mapping or the full
// plugins.configs.deepseek-vision host wrapper.
func ParseYAML(raw []byte) (*Config, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return defaultConfig(), nil
	}
	var top yaml.Node
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&top); err != nil {
		return nil, fmt.Errorf("decode config YAML: %w", err)
	}
	for {
		var trailing yaml.Node
		err := dec.Decode(&trailing)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode trailing config YAML: %w", err)
		}
		if !emptyYAMLDocument(trailing) {
			return nil, errors.New("config YAML must contain exactly one non-empty document")
		}
	}
	if top.Kind == 0 || (top.Kind == yaml.DocumentNode && len(top.Content) == 0) {
		return defaultConfig(), nil
	}
	root := top
	if top.Kind == yaml.DocumentNode {
		root = *top.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("config YAML must be a mapping")
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "plugins" {
			continue
		}
		var wrapped wrappedDocument
		if err := decodeNodeStrict(root, &wrapped); err != nil {
			return nil, fmt.Errorf("decode host config: %w", err)
		}
		node, ok := wrapped.Plugins.Configs["deepseek-vision"]
		if !ok {
			return defaultConfig(), nil
		}
		return parseNode(node)
	}
	return parseNode(root)
}

func emptyYAMLDocument(node yaml.Node) bool {
	if node.Kind == 0 {
		return true
	}
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return true
		}
		node = *node.Content[0]
	}
	return node.Kind == yaml.ScalarNode && node.Tag == "!!null" && node.Value == ""
}

func parseNode(node yaml.Node) (*Config, error) {
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = *node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil, errors.New("plugin config must be a mapping")
	}
	if len(node.Content) == 0 {
		return defaultConfig(), nil
	}
	var raw rawConfig
	if err := decodeNodeStrict(node, &raw); err != nil {
		return nil, fmt.Errorf("decode plugin config: %w", err)
	}
	return validate(raw, nodeFields(node))
}

func nodeFields(node yaml.Node) map[string]bool {
	fields := make(map[string]bool)
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = *node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return fields
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		fields[node.Content[i].Value] = true
	}
	return fields
}

func decodeNodeStrict(node yaml.Node, dst any) error {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	if err := enc.Encode(&node); err != nil {
		return err
	}
	_ = enc.Close()
	dec := yaml.NewDecoder(strings.NewReader(b.String()))
	dec.KnownFields(true)
	return dec.Decode(dst)
}

func validate(raw rawConfig, present map[string]bool) (*Config, error) {
	cfg := defaultRaw()
	if raw.TargetModels != nil || present["target_models"] {
		cfg.TargetModels = raw.TargetModels
	}
	if raw.VisionModel != "" || present["vision_model"] {
		cfg.VisionModel = raw.VisionModel
	}
	if raw.Language != "" || present["language"] {
		cfg.Language = raw.Language
	}
	if raw.RequestTimeoutSeconds != 0 || present["request_timeout_seconds"] {
		cfg.RequestTimeoutSeconds = raw.RequestTimeoutSeconds
	}
	if raw.EmergencyMaxImagesPerRequest != 0 || present["emergency_max_images_per_request"] {
		cfg.EmergencyMaxImagesPerRequest = raw.EmergencyMaxImagesPerRequest
	}
	if raw.MaxInflightVisionRequests != 0 || present["max_inflight_vision_requests"] {
		cfg.MaxInflightVisionRequests = raw.MaxInflightVisionRequests
	}
	if raw.MaxRequestBytes != 0 || present["max_request_bytes"] {
		cfg.MaxRequestBytes = raw.MaxRequestBytes
	}
	if raw.MaxImageReferenceBytes != 0 || present["max_image_reference_bytes"] {
		cfg.MaxImageReferenceBytes = raw.MaxImageReferenceBytes
	}
	if raw.MaxResponseBytes != 0 || present["max_response_bytes"] {
		cfg.MaxResponseBytes = raw.MaxResponseBytes
	}
	if raw.MaxResultChars != 0 || present["max_result_chars"] {
		cfg.MaxResultChars = raw.MaxResultChars
	}
	if raw.AnalysisCacheSize != 0 || present["analysis_cache_size"] {
		cfg.AnalysisCacheSize = raw.AnalysisCacheSize
	}
	if raw.AnalysisCacheTTL != 0 || present["analysis_cache_ttl_seconds"] {
		cfg.AnalysisCacheTTL = raw.AnalysisCacheTTL
	}
	if raw.URLAnalysisCacheTTL != 0 || present["analysis_url_cache_ttl_seconds"] {
		cfg.URLAnalysisCacheTTL = raw.URLAnalysisCacheTTL
	}
	if present["trace_enabled"] {
		cfg.TraceEnabled = raw.TraceEnabled
	}
	models, err := canonicalTargetModels(cfg.TargetModels)
	if err != nil {
		return nil, err
	}
	cfg.TargetModels = models
	if err := validateRaw(cfg); err != nil {
		return nil, err
	}
	return &Config{
		TargetModels:                 append([]string(nil), cfg.TargetModels...),
		VisionModel:                  strings.TrimSpace(cfg.VisionModel),
		Language:                     strings.TrimSpace(cfg.Language),
		RequestTimeout:               time.Duration(cfg.RequestTimeoutSeconds) * time.Second,
		EmergencyMaxImagesPerRequest: cfg.EmergencyMaxImagesPerRequest,
		MaxInflightVisionRequests:    cfg.MaxInflightVisionRequests,
		MaxRequestBytes:              cfg.MaxRequestBytes,
		MaxImageReferenceBytes:       cfg.MaxImageReferenceBytes,
		MaxResponseBytes:             cfg.MaxResponseBytes,
		MaxResultChars:               cfg.MaxResultChars,
		AnalysisCacheSize:            cfg.AnalysisCacheSize,
		AnalysisCacheTTL:             time.Duration(cfg.AnalysisCacheTTL) * time.Second,
		URLAnalysisCacheTTL:          time.Duration(cfg.URLAnalysisCacheTTL) * time.Second,
		TraceEnabled:                 cfg.TraceEnabled,
	}, nil
}

func canonicalTargetModels(models []string) ([]string, error) {
	if len(models) == 0 {
		return nil, errors.New("target_models must contain at least one model")
	}
	canonical := make([]string, len(models))
	seen := make(map[string]struct{}, len(models))
	for i, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			return nil, fmt.Errorf("target_models[%d] must not be empty", i)
		}
		if _, ok := seen[model]; ok {
			return nil, fmt.Errorf("target_models contains duplicate model %q", model)
		}
		seen[model] = struct{}{}
		canonical[i] = model
	}
	return canonical, nil
}

func validateRaw(raw rawConfig) error {
	if strings.TrimSpace(raw.VisionModel) == "" {
		return errors.New("vision_model must not be empty")
	}
	if strings.TrimSpace(raw.Language) == "" {
		return errors.New("language must not be empty")
	}
	if raw.RequestTimeoutSeconds < 1 || raw.RequestTimeoutSeconds > 3600 {
		return errors.New("request_timeout_seconds must be between 1 and 3600")
	}
	if raw.EmergencyMaxImagesPerRequest < 16 || raw.EmergencyMaxImagesPerRequest > 1024 {
		return errors.New("emergency_max_images_per_request must be between 16 and 1024")
	}
	if raw.MaxInflightVisionRequests < 1 || raw.MaxInflightVisionRequests > 16 {
		return errors.New("max_inflight_vision_requests must be between 1 and 16")
	}
	if raw.MaxRequestBytes < 1024 || raw.MaxRequestBytes > MaxRequestBytesLimit {
		return errors.New("max_request_bytes must be between 1024 and 33554432")
	}
	if raw.MaxImageReferenceBytes < 1024 || raw.MaxImageReferenceBytes > 16*1024*1024 {
		return errors.New("max_image_reference_bytes must be between 1024 and 16777216")
	}
	if raw.MaxResponseBytes < 1024 || raw.MaxResponseBytes > 8*1024*1024 {
		return errors.New("max_response_bytes must be between 1024 and 8388608")
	}
	if raw.MaxResultChars < 1 || raw.MaxResultChars > 100_000 {
		return errors.New("max_result_chars must be between 1 and 100000")
	}
	if raw.AnalysisCacheSize < 0 || raw.AnalysisCacheSize > 10_000 {
		return errors.New("analysis_cache_size must be between 0 and 10000")
	}
	if raw.AnalysisCacheTTL < 1 || raw.AnalysisCacheTTL > 7*24*3600 {
		return errors.New("analysis_cache_ttl_seconds must be between 1 and 604800")
	}
	if raw.URLAnalysisCacheTTL < 1 || raw.URLAnalysisCacheTTL > 7*24*3600 {
		return errors.New("analysis_url_cache_ttl_seconds must be between 1 and 604800")
	}
	return nil
}

type defaults = rawConfig

func defaultRaw() defaults {
	return defaults{
		TargetModels:                 []string{"deepseek-v4-flash"},
		VisionModel:                  DefaultVisionModel,
		Language:                     DefaultLanguage,
		RequestTimeoutSeconds:        DefaultRequestTimeoutSec,
		EmergencyMaxImagesPerRequest: DefaultEmergencyMaxImages,
		MaxInflightVisionRequests:    DefaultMaxInflightVisionRequests,
		MaxRequestBytes:              DefaultMaxRequestBytes,
		MaxImageReferenceBytes:       DefaultMaxImageRefBytes,
		MaxResponseBytes:             DefaultMaxResponseBytes,
		MaxResultChars:               DefaultMaxResultChars,
		AnalysisCacheSize:            DefaultAnalysisCacheSize,
		AnalysisCacheTTL:             DefaultAnalysisCacheTTL,
		URLAnalysisCacheTTL:          DefaultURLCacheTTL,
		TraceEnabled:                 false,
	}
}

func defaultConfig() *Config {
	d := defaultRaw()
	return &Config{
		TargetModels:                 append([]string(nil), d.TargetModels...),
		VisionModel:                  d.VisionModel,
		Language:                     d.Language,
		RequestTimeout:               time.Duration(d.RequestTimeoutSeconds) * time.Second,
		EmergencyMaxImagesPerRequest: d.EmergencyMaxImagesPerRequest,
		MaxInflightVisionRequests:    d.MaxInflightVisionRequests,
		MaxRequestBytes:              d.MaxRequestBytes,
		MaxImageReferenceBytes:       d.MaxImageReferenceBytes,
		MaxResponseBytes:             d.MaxResponseBytes,
		MaxResultChars:               d.MaxResultChars,
		AnalysisCacheSize:            d.AnalysisCacheSize,
		AnalysisCacheTTL:             time.Duration(d.AnalysisCacheTTL) * time.Second,
		URLAnalysisCacheTTL:          time.Duration(d.URLAnalysisCacheTTL) * time.Second,
		TraceEnabled:                 d.TraceEnabled,
	}
}
