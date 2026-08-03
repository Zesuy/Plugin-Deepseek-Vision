// Package config parses and publishes immutable deepseek-vision configuration
// snapshots.  The host owns the enabled and priority switches; this package
// deliberately accepts (and ignores) those fields without exposing them as
// plugin settings.
package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	VisionBackendHost     = "host"
	VisionBackendExternal = "external"
	DefaultVisionBackend  = VisionBackendHost
	// DefaultVisionBaseURL is empty because host mode routes the vision request
	// through CLIProxyAPI and reuses the host's configured provider credentials.
	DefaultVisionBaseURL     = ""
	DefaultVisionModel       = "gpt-5.6-luna"
	DefaultVisionAPIKeyEnv   = "DEEPSEEK_VISION_API_KEY"
	DefaultLanguage          = "zh"
	DefaultRequestTimeoutSec = 120
	DefaultPerCallTimeoutSec = 60
	DefaultRetryMaxAttempts  = 3
	DefaultMaxConcurrency    = 4
	DefaultMaxImages         = 4
	DefaultMaxRequestBytes   = 20 * 1024 * 1024
	DefaultMaxImageRefBytes  = 15 * 1024 * 1024
	DefaultMaxResponseBytes  = 4 * 1024 * 1024
	DefaultMaxResultChars    = 20_000
	DefaultCacheSize         = 128
	DefaultCacheTTLSec       = 900
	// MaxRequestBytesLimit is the largest raw Responses body accepted by
	// configuration. The native ABI accounts separately for base64 expansion
	// and JSON envelope overhead before copying an interceptor RPC request.
	MaxRequestBytesLimit = 32 * 1024 * 1024
)

// Config is an immutable, validated runtime snapshot.  Callers must treat
// TargetModels as read-only; ParseYAML and Snapshot allocate independent
// backing storage before publishing a snapshot.
type Config struct {
	TargetModels           []string
	VisionBackend          string
	VisionBaseURL          string
	VisionModel            string
	VisionAPIKeyEnv        string
	Language               string
	RequestTimeout         time.Duration
	PerCallTimeout         time.Duration
	RetryMaxAttempts       int
	MaxConcurrency         int
	MaxImagesPerRequest    int
	MaxRequestBytes        int
	MaxImageReferenceBytes int
	MaxResponseBytes       int
	MaxResultChars         int
	CacheSize              int
	CacheTTL               time.Duration
}

// rawConfig mirrors config.example.yaml.  Timeout values are intentionally
// represented as seconds to keep the YAML contract simple and stable.
type rawConfig struct {
	// These switches are host-owned.  They are accepted for compatibility but
	// never affect plugin behavior.
	Enabled  *bool     `yaml:"enabled"`
	Priority *int      `yaml:"priority"`
	Store    yaml.Node `yaml:"store"`

	TargetModels           []string `yaml:"target_models"`
	VisionBackend          string   `yaml:"vision_backend"`
	VisionBaseURL          string   `yaml:"vision_base_url"`
	VisionModel            string   `yaml:"vision_model"`
	VisionAPIKeyEnv        string   `yaml:"vision_api_key_env"`
	Language               string   `yaml:"language"`
	RequestTimeoutSeconds  int      `yaml:"request_timeout_seconds"`
	PerCallTimeoutSeconds  int      `yaml:"per_call_timeout_seconds"`
	RetryMaxAttempts       int      `yaml:"retry_max_attempts"`
	MaxConcurrency         int      `yaml:"max_concurrency"`
	MaxImagesPerRequest    int      `yaml:"max_images_per_request"`
	MaxRequestBytes        int      `yaml:"max_request_bytes"`
	MaxImageReferenceBytes int      `yaml:"max_image_reference_bytes"`
	MaxResponseBytes       int      `yaml:"max_response_bytes"`
	MaxResultChars         int      `yaml:"max_result_chars"`
	CacheSize              int      `yaml:"cache_size"`
	CacheTTLSeconds        int      `yaml:"cache_ttl_seconds"`
}

type hostDocument struct {
	Enabled *bool                `yaml:"enabled"`
	Store   yaml.Node            `yaml:"store"`
	Configs map[string]yaml.Node `yaml:"configs"`
}

type wrappedDocument struct {
	Plugins hostDocument `yaml:"plugins"`
}

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var current atomic.Pointer[Config]

func init() {
	current.Store(defaultConfig())
}

// Default returns a fresh default snapshot.
func Default() *Config { return defaultConfig() }

// Snapshot returns the currently published immutable snapshot.  It returns
// nil after Shutdown and before the first successful configuration if a host
// explicitly shuts the plugin down.
func Snapshot() *Config { return cloneConfig(current.Load()) }

// ConfigureYAML validates a complete lifecycle config and atomically publishes
// it.  The previous snapshot remains active if parsing or validation fails.
func ConfigureYAML(raw []byte) error {
	cfg, err := ParseYAML(raw)
	if err != nil {
		return err
	}
	return PublishValidated(cfg)
}

// PublishValidated atomically publishes an already validated immutable configuration
// snapshot. Lifecycle callers use this after validating and installing their
// runtime generation so the runtime gate and management snapshot have a
// deliberate publication order.
func PublishValidated(cfg *Config) error {
	if cfg == nil {
		return errors.New("configuration must not be nil")
	}
	current.Store(cloneConfig(cfg))
	return nil
}

// Shutdown clears the active snapshot.  It is safe to call repeatedly.
func Shutdown() { current.Store(nil) }

func cloneConfig(cfg *Config) *Config {
	if cfg == nil {
		return nil
	}
	clone := *cfg
	clone.TargetModels = append([]string(nil), cfg.TargetModels...)
	return &clone
}

// ParseYAML parses direct plugin configuration, or the host wrapper
// plugins.configs.deepseek-vision used by management tooling.
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
	var dec yaml.Decoder
	// Decoder.Decode cannot consume a Node directly.  Encode the already
	// parsed node in memory, then decode with KnownFields enabled so typos are
	// rejected rather than silently ignored.
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	if err := enc.Encode(&node); err != nil {
		return err
	}
	_ = enc.Close()
	dec = *yaml.NewDecoder(strings.NewReader(b.String()))
	dec.KnownFields(true)
	return dec.Decode(dst)
}

func validate(raw rawConfig, present ...map[string]bool) (*Config, error) {
	has := func(name string) bool {
		return len(present) > 0 && present[0] != nil && present[0][name]
	}
	cfg := defaultRaw()
	if raw.TargetModels != nil || has("target_models") {
		cfg.TargetModels = raw.TargetModels
	}
	if raw.VisionBackend != "" || has("vision_backend") {
		cfg.VisionBackend = raw.VisionBackend
	}
	if raw.VisionBaseURL != "" || has("vision_base_url") {
		cfg.VisionBaseURL = raw.VisionBaseURL
	}
	// Existing deployments predate vision_backend. A configured external URL is
	// an unambiguous compatibility signal; otherwise new/empty configs use host.
	if !has("vision_backend") && strings.TrimSpace(cfg.VisionBaseURL) != "" {
		cfg.VisionBackend = VisionBackendExternal
	}
	if raw.VisionModel != "" || has("vision_model") {
		cfg.VisionModel = raw.VisionModel
	}
	if raw.VisionAPIKeyEnv != "" || has("vision_api_key_env") {
		cfg.VisionAPIKeyEnv = raw.VisionAPIKeyEnv
	}
	if raw.Language != "" || has("language") {
		cfg.Language = raw.Language
	}
	if raw.RequestTimeoutSeconds != 0 || has("request_timeout_seconds") {
		cfg.RequestTimeoutSeconds = raw.RequestTimeoutSeconds
	}
	if raw.PerCallTimeoutSeconds != 0 || has("per_call_timeout_seconds") {
		cfg.PerCallTimeoutSeconds = raw.PerCallTimeoutSeconds
	}
	if raw.RetryMaxAttempts != 0 || has("retry_max_attempts") {
		cfg.RetryMaxAttempts = raw.RetryMaxAttempts
	}
	if raw.MaxConcurrency != 0 || has("max_concurrency") {
		cfg.MaxConcurrency = raw.MaxConcurrency
	}
	if raw.MaxImagesPerRequest != 0 || has("max_images_per_request") {
		cfg.MaxImagesPerRequest = raw.MaxImagesPerRequest
	}
	if raw.MaxRequestBytes != 0 || has("max_request_bytes") {
		cfg.MaxRequestBytes = raw.MaxRequestBytes
	}
	if raw.MaxImageReferenceBytes != 0 || has("max_image_reference_bytes") {
		cfg.MaxImageReferenceBytes = raw.MaxImageReferenceBytes
	}
	if raw.MaxResponseBytes != 0 || has("max_response_bytes") {
		cfg.MaxResponseBytes = raw.MaxResponseBytes
	}
	if raw.MaxResultChars != 0 || has("max_result_chars") {
		cfg.MaxResultChars = raw.MaxResultChars
	}
	if raw.CacheSize != 0 || has("cache_size") {
		cfg.CacheSize = raw.CacheSize
	}
	if raw.CacheTTLSeconds != 0 || has("cache_ttl_seconds") {
		cfg.CacheTTLSeconds = raw.CacheTTLSeconds
	}
	models, err := canonicalTargetModels(cfg.TargetModels)
	if err != nil {
		return nil, err
	}
	cfg.TargetModels = models
	if err := validateRaw(cfg); err != nil {
		return nil, err
	}
	publishedModels := append([]string(nil), cfg.TargetModels...)
	return &Config{
		TargetModels:           publishedModels,
		VisionBackend:          strings.ToLower(strings.TrimSpace(cfg.VisionBackend)),
		VisionBaseURL:          strings.TrimRight(strings.TrimSpace(cfg.VisionBaseURL), "/"),
		VisionModel:            strings.TrimSpace(cfg.VisionModel),
		VisionAPIKeyEnv:        strings.TrimSpace(cfg.VisionAPIKeyEnv),
		Language:               strings.TrimSpace(cfg.Language),
		RequestTimeout:         time.Duration(cfg.RequestTimeoutSeconds) * time.Second,
		PerCallTimeout:         time.Duration(cfg.PerCallTimeoutSeconds) * time.Second,
		RetryMaxAttempts:       cfg.RetryMaxAttempts,
		MaxConcurrency:         cfg.MaxConcurrency,
		MaxImagesPerRequest:    cfg.MaxImagesPerRequest,
		MaxRequestBytes:        cfg.MaxRequestBytes,
		MaxImageReferenceBytes: cfg.MaxImageReferenceBytes,
		MaxResponseBytes:       cfg.MaxResponseBytes,
		MaxResultChars:         cfg.MaxResultChars,
		CacheSize:              cfg.CacheSize,
		CacheTTL:               time.Duration(cfg.CacheTTLSeconds) * time.Second,
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
	backend := strings.ToLower(strings.TrimSpace(raw.VisionBackend))
	if backend != VisionBackendHost && backend != VisionBackendExternal {
		return errors.New("vision_backend must be host or external")
	}
	if backend == VisionBackendExternal {
		u, err := url.Parse(strings.TrimSpace(raw.VisionBaseURL))
		if err != nil || u.Scheme == "" || u.Host == "" || (!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) {
			return errors.New("vision_base_url must be an absolute http or https URL in external mode")
		}
		if u.User != nil {
			return errors.New("vision_base_url must not contain credentials")
		}
		if u.Hostname() == "" || !validPort(u.Port()) {
			return errors.New("vision_base_url must contain a valid host and port")
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return errors.New("vision_base_url must not contain query or fragment")
		}
		path := strings.TrimRight(u.Path, "/")
		if strings.HasSuffix(strings.ToLower(path), "/responses") {
			return errors.New("vision_base_url must be a base URL without /responses")
		}
	}
	if strings.TrimSpace(raw.VisionModel) == "" {
		return errors.New("vision_model must not be empty")
	}
	if !envNamePattern.MatchString(strings.TrimSpace(raw.VisionAPIKeyEnv)) {
		return errors.New("vision_api_key_env must be a valid environment variable name")
	}
	if strings.TrimSpace(raw.Language) == "" {
		return errors.New("language must not be empty")
	}
	if raw.RequestTimeoutSeconds < 1 || raw.RequestTimeoutSeconds > 3600 {
		return errors.New("request_timeout_seconds must be between 1 and 3600")
	}
	if raw.PerCallTimeoutSeconds < 1 || raw.PerCallTimeoutSeconds > 3600 {
		return errors.New("per_call_timeout_seconds must be between 1 and 3600")
	}
	if raw.PerCallTimeoutSeconds > raw.RequestTimeoutSeconds {
		return errors.New("per_call_timeout_seconds must not exceed request_timeout_seconds")
	}
	if raw.RetryMaxAttempts < 1 || raw.RetryMaxAttempts > 10 {
		return errors.New("retry_max_attempts must be between 1 and 10")
	}
	if raw.MaxConcurrency < 1 || raw.MaxConcurrency > 32 {
		return errors.New("max_concurrency must be between 1 and 32")
	}
	if raw.MaxImagesPerRequest < 1 || raw.MaxImagesPerRequest > 16 {
		return errors.New("max_images_per_request must be between 1 and 16")
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
	if raw.CacheSize < 0 || raw.CacheSize > 100_000 {
		return errors.New("cache_size must be between 0 and 100000")
	}
	if raw.CacheTTLSeconds < 1 || raw.CacheTTLSeconds > 7*24*3600 {
		return errors.New("cache_ttl_seconds must be between 1 and 604800")
	}
	return nil
}

func validPort(port string) bool {
	if port == "" {
		return true
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

type defaults = rawConfig

func defaultRaw() defaults {
	return defaults{
		TargetModels:           []string{"deepseek-v4-flash"},
		VisionBackend:          DefaultVisionBackend,
		VisionBaseURL:          DefaultVisionBaseURL,
		VisionModel:            DefaultVisionModel,
		VisionAPIKeyEnv:        DefaultVisionAPIKeyEnv,
		Language:               DefaultLanguage,
		RequestTimeoutSeconds:  DefaultRequestTimeoutSec,
		PerCallTimeoutSeconds:  DefaultPerCallTimeoutSec,
		RetryMaxAttempts:       DefaultRetryMaxAttempts,
		MaxConcurrency:         DefaultMaxConcurrency,
		MaxImagesPerRequest:    DefaultMaxImages,
		MaxRequestBytes:        DefaultMaxRequestBytes,
		MaxImageReferenceBytes: DefaultMaxImageRefBytes,
		MaxResponseBytes:       DefaultMaxResponseBytes,
		MaxResultChars:         DefaultMaxResultChars,
		CacheSize:              DefaultCacheSize,
		CacheTTLSeconds:        DefaultCacheTTLSec,
	}
}

func defaultConfig() *Config {
	d := defaultRaw()
	// Defaults use the host callback backend, so the plugin can reuse a vision
	// model and credential already configured in CLIProxyAPI.
	return &Config{
		TargetModels:           append([]string(nil), d.TargetModels...),
		VisionBackend:          d.VisionBackend,
		VisionBaseURL:          "",
		VisionModel:            d.VisionModel,
		VisionAPIKeyEnv:        d.VisionAPIKeyEnv,
		Language:               d.Language,
		RequestTimeout:         time.Duration(d.RequestTimeoutSeconds) * time.Second,
		PerCallTimeout:         time.Duration(d.PerCallTimeoutSeconds) * time.Second,
		RetryMaxAttempts:       d.RetryMaxAttempts,
		MaxConcurrency:         d.MaxConcurrency,
		MaxImagesPerRequest:    d.MaxImagesPerRequest,
		MaxRequestBytes:        d.MaxRequestBytes,
		MaxImageReferenceBytes: d.MaxImageReferenceBytes,
		MaxResponseBytes:       d.MaxResponseBytes,
		MaxResultChars:         d.MaxResultChars,
		CacheSize:              d.CacheSize,
		CacheTTL:               time.Duration(d.CacheTTLSeconds) * time.Second,
	}
}
