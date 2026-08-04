// Package tracelog owns the opt-in full-context debug trace. It is deliberately
// separate from the host logger: trace payloads may contain complete user
// requests, image data URIs and model output.
package tracelog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	DefaultRoot           = "logs/deepseek-vision-trace"
	DefaultMaxTotalBytes  = int64(1 << 30)
	DefaultMaxEventBytes  = int64(64 << 20)
	DefaultEventBackups   = 3
	traceDirectoryMode    = 0o700
	traceFileMode         = 0o600
	eventsFileName        = "events.jsonl"
	requestsDirectoryName = "requests"
)

type Options struct {
	Root          string
	MaxTotalBytes int64
	MaxEventBytes int64
	EventBackups  int
	ReportError   func(error)
}

type Sink struct {
	mu           sync.Mutex
	root         string
	maxTotal     int64
	maxEvents    int64
	eventBackups int
	reportError  func(error)
	events       *os.File
	enabled      bool
	reported     bool
	epoch        uint64
	sequence     uint64
	active       map[string]int
}

type RequestMeta struct {
	RequestID        string
	TraceID          string
	ConfigGeneration uint64
}

type Session struct {
	sink             *Sink
	epoch            uint64
	bundle           string
	directory        string
	requestID        string
	traceID          string
	configGeneration uint64
	closeOnce        sync.Once
}

type eventRecord struct {
	Timestamp        string `json:"timestamp"`
	Event            string `json:"event"`
	RequestID        string `json:"request_id,omitempty"`
	TraceID          string `json:"trace_id,omitempty"`
	ConfigGeneration uint64 `json:"config_generation,omitempty"`
	Bundle           string `json:"bundle,omitempty"`
	Data             any    `json:"data,omitempty"`
}

func New(options Options) *Sink {
	root := strings.TrimSpace(options.Root)
	if root == "" {
		root = DefaultRoot
	}
	maxTotal := options.MaxTotalBytes
	if maxTotal <= 0 {
		maxTotal = DefaultMaxTotalBytes
	}
	maxEvents := options.MaxEventBytes
	if maxEvents <= 0 {
		maxEvents = DefaultMaxEventBytes
	}
	backups := options.EventBackups
	if backups <= 0 {
		backups = DefaultEventBackups
	}
	return &Sink{
		root:         root,
		maxTotal:     maxTotal,
		maxEvents:    maxEvents,
		eventBackups: backups,
		reportError:  options.ReportError,
		active:       make(map[string]int),
	}
}

func (s *Sink) Configure(enabled bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.epoch++
	s.reported = false
	s.closeEventsLocked()
	s.enabled = false
	if !enabled {
		s.mu.Unlock()
		return
	}
	err := s.openLocked()
	if err == nil {
		s.enabled = true
	}
	reporter, report := s.failLocked(err)
	s.mu.Unlock()
	if report {
		safeReport(reporter, err)
	}
}

func (s *Sink) Enabled() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enabled
}

func (s *Sink) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.epoch++
	s.enabled = false
	s.closeEventsLocked()
	s.mu.Unlock()
}

func (s *Sink) Start(meta RequestMeta) *Session {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if !s.enabled || s.events == nil {
		s.mu.Unlock()
		return nil
	}
	s.sequence++
	bundle := fmt.Sprintf("%s-%06d-%s", time.Now().UTC().Format("20060102T150405.000000000Z"), s.sequence, safeName(meta.RequestID))
	directory := filepath.Join(s.root, requestsDirectoryName, bundle)
	err := os.Mkdir(directory, traceDirectoryMode)
	if err == nil {
		err = os.Chmod(directory, traceDirectoryMode)
	}
	if err != nil {
		err = fmt.Errorf("create trace request bundle: %w", err)
		reporter, report := s.failLocked(err)
		s.mu.Unlock()
		if report {
			safeReport(reporter, err)
		}
		return nil
	}
	s.active[bundle]++
	session := &Session{
		sink:             s,
		epoch:            s.epoch,
		bundle:           bundle,
		directory:        directory,
		requestID:        meta.RequestID,
		traceID:          meta.TraceID,
		configGeneration: meta.ConfigGeneration,
	}
	s.mu.Unlock()
	return session
}

func (s *Session) Event(name string, data any) {
	if s == nil || s.sink == nil || strings.TrimSpace(name) == "" {
		return
	}
	sink := s.sink
	sink.mu.Lock()
	if !sink.enabled || sink.events == nil || sink.epoch != s.epoch {
		sink.mu.Unlock()
		return
	}
	if err := sink.rotateEventsLocked(); err != nil {
		reporter, report := sink.failLocked(err)
		sink.mu.Unlock()
		if report {
			safeReport(reporter, err)
		}
		return
	}
	record := eventRecord{
		Timestamp:        time.Now().UTC().Format(time.RFC3339Nano),
		Event:            strings.TrimSpace(name),
		RequestID:        s.requestID,
		TraceID:          s.traceID,
		ConfigGeneration: s.configGeneration,
		Bundle:           filepath.ToSlash(filepath.Join(requestsDirectoryName, s.bundle)),
		Data:             data,
	}
	raw, err := json.Marshal(record)
	if err == nil {
		raw = append(raw, '\n')
		_, err = sink.events.Write(raw)
	}
	if err != nil {
		err = fmt.Errorf("write trace event: %w", err)
	}
	reporter, report := sink.failLocked(err)
	sink.mu.Unlock()
	if report {
		safeReport(reporter, err)
	}
}

func (s *Session) Artifact(name string, data []byte) string {
	if s == nil || s.sink == nil || !validArtifactName(name) {
		return ""
	}
	sink := s.sink
	sink.mu.Lock()
	if !sink.enabled || sink.epoch != s.epoch {
		sink.mu.Unlock()
		return ""
	}
	path := filepath.Join(s.directory, name)
	err := writePrivateFile(path, data)
	if err != nil {
		err = fmt.Errorf("write trace artifact %s: %w", name, err)
	}
	reporter, report := sink.failLocked(err)
	sink.mu.Unlock()
	if report {
		safeReport(reporter, err)
	}
	if err != nil {
		return ""
	}
	return filepath.ToSlash(filepath.Join(requestsDirectoryName, s.bundle, name))
}

func (s *Session) JSON(name string, value any) string {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		s.fail(fmt.Errorf("encode trace artifact %s: %w", name, err))
		return ""
	}
	raw = append(raw, '\n')
	return s.Artifact(name, raw)
}

func (s *Session) fail(err error) {
	if s == nil || s.sink == nil || err == nil {
		return
	}
	sink := s.sink
	sink.mu.Lock()
	if sink.epoch != s.epoch {
		sink.mu.Unlock()
		return
	}
	reporter, report := sink.failLocked(err)
	sink.mu.Unlock()
	if report {
		safeReport(reporter, err)
	}
}

func (s *Session) Close() {
	if s == nil || s.sink == nil {
		return
	}
	s.closeOnce.Do(func() {
		sink := s.sink
		sink.mu.Lock()
		if sink.active[s.bundle] > 1 {
			sink.active[s.bundle]--
		} else {
			delete(sink.active, s.bundle)
		}
		err := sink.pruneBundlesLocked()
		reporter, report := sink.failLocked(err)
		sink.mu.Unlock()
		if report {
			safeReport(reporter, err)
		}
	})
}

func (s *Sink) openLocked() error {
	if err := ensurePrivateDirectory(s.root); err != nil {
		return fmt.Errorf("protect trace directory: %w", err)
	}
	if err := ensurePrivateDirectory(filepath.Join(s.root, requestsDirectoryName)); err != nil {
		return fmt.Errorf("create trace requests directory: %w", err)
	}
	eventsPath := filepath.Join(s.root, eventsFileName)
	if err := rejectSymlink(eventsPath); err != nil {
		return err
	}
	if err := s.rotateEventsLocked(); err != nil {
		return err
	}
	events, err := os.OpenFile(eventsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, traceFileMode)
	if err != nil {
		return fmt.Errorf("open trace event stream: %w", err)
	}
	if err := events.Chmod(traceFileMode); err != nil {
		_ = events.Close()
		return fmt.Errorf("protect trace event stream: %w", err)
	}
	s.events = events
	return nil
}

func (s *Sink) rotateEventsLocked() error {
	current := filepath.Join(s.root, eventsFileName)
	info, err := os.Stat(current)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat trace event stream: %w", err)
	}
	if info.Size() < s.maxEvents {
		return nil
	}
	if s.events != nil {
		if err := s.events.Close(); err != nil {
			return fmt.Errorf("close trace event stream for rotation: %w", err)
		}
		s.events = nil
	}
	oldest := fmt.Sprintf("%s.%d", current, s.eventBackups)
	if err := os.Remove(oldest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove oldest trace event backup: %w", err)
	}
	for index := s.eventBackups - 1; index >= 1; index-- {
		from := fmt.Sprintf("%s.%d", current, index)
		to := fmt.Sprintf("%s.%d", current, index+1)
		if err := os.Rename(from, to); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("rotate trace event backup: %w", err)
		}
	}
	if err := os.Rename(current, current+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rotate trace event stream: %w", err)
	}
	if s.enabled {
		events, openErr := os.OpenFile(current, os.O_CREATE|os.O_APPEND|os.O_WRONLY, traceFileMode)
		if openErr != nil {
			return fmt.Errorf("reopen trace event stream: %w", openErr)
		}
		s.events = events
	}
	return nil
}

func (s *Sink) pruneBundlesLocked() error {
	requestsRoot := filepath.Join(s.root, requestsDirectoryName)
	entries, err := os.ReadDir(requestsRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read trace bundles: %w", err)
	}
	type bundleInfo struct {
		name    string
		path    string
		modTime time.Time
		size    int64
	}
	var bundles []bundleInfo
	var total int64
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(requestsRoot, entry.Name())
		size, sizeErr := directorySize(path)
		if sizeErr != nil {
			return fmt.Errorf("measure trace bundle: %w", sizeErr)
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return fmt.Errorf("stat trace bundle: %w", infoErr)
		}
		total += size
		bundles = append(bundles, bundleInfo{name: entry.Name(), path: path, modTime: info.ModTime(), size: size})
	}
	if total <= s.maxTotal {
		return nil
	}
	sort.Slice(bundles, func(i, j int) bool {
		if bundles[i].modTime.Equal(bundles[j].modTime) {
			return bundles[i].name < bundles[j].name
		}
		return bundles[i].modTime.Before(bundles[j].modTime)
	})
	for _, bundle := range bundles {
		if total <= s.maxTotal {
			break
		}
		if s.active[bundle.name] > 0 {
			continue
		}
		if err := os.RemoveAll(bundle.path); err != nil {
			return fmt.Errorf("remove old trace bundle: %w", err)
		}
		total -= bundle.size
	}
	return nil
}

func (s *Sink) failLocked(err error) (func(error), bool) {
	if err == nil || s.reported {
		return nil, false
	}
	s.reported = true
	s.enabled = false
	s.closeEventsLocked()
	return s.reportError, s.reportError != nil
}

func (s *Sink) closeEventsLocked() {
	if s.events != nil {
		_ = s.events.Close()
		s.events = nil
	}
}

func writePrivateFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, traceFileMode)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err == nil {
		err = file.Chmod(traceFileMode)
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	return err
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, traceDirectoryMode); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("trace path is not a real directory")
	}
	return os.Chmod(path, traceDirectoryMode)
}

func rejectSymlink(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect trace path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("trace path must not be a symbolic link")
	}
	return nil
}

func directorySize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			info, infoErr := entry.Info()
			if infoErr != nil {
				return infoErr
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func validArtifactName(name string) bool {
	return name != "" && filepath.Base(name) == name && name != "." && name != ".."
}

func safeName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "request"
	}
	var builder strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('_')
		}
		if builder.Len() >= 64 {
			break
		}
	}
	if builder.Len() == 0 {
		return "request"
	}
	return builder.String()
}

func safeReport(reporter func(error), err error) {
	if reporter == nil || err == nil {
		return
	}
	defer func() { _ = recover() }()
	reporter(err)
}
