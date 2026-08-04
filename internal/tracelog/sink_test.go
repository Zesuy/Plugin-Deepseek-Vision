package tracelog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSinkWritesPrivateFullContextBundle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "trace")
	sink := New(Options{Root: root, MaxTotalBytes: 1 << 20, MaxEventBytes: 1 << 20})
	sink.Configure(true)
	if !sink.Enabled() {
		t.Fatal("trace sink was not enabled")
	}
	session := sink.Start(RequestMeta{RequestID: "request/unsafe", TraceID: "trace-1", ConfigGeneration: 7})
	if session == nil {
		t.Fatal("trace session is nil")
	}
	body := []byte(`{"input":[{"role":"user","content":[{"type":"input_text","text":"plaintext context"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`)
	artifact := session.Artifact("10-inbound-body.json", body)
	session.Event("request_received", map[string]any{"body_bytes": len(body)})
	session.Close()
	sink.Close()

	if artifact == "" || strings.Contains(artifact, "request/unsafe") {
		t.Fatalf("artifact path = %q", artifact)
	}
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(artifact)))
	if err != nil || string(raw) != string(body) {
		t.Fatalf("artifact = %q, err=%v", raw, err)
	}
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(artifact)))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("artifact mode = %v", info.Mode().Perm())
	}
	events, err := os.ReadFile(filepath.Join(root, eventsFileName))
	if err != nil || !strings.Contains(string(events), `"event":"request_received"`) || !strings.Contains(string(events), `"trace_id":"trace-1"`) {
		t.Fatalf("events = %s, err=%v", events, err)
	}
}

func TestSinkSerializesConcurrentEvents(t *testing.T) {
	root := filepath.Join(t.TempDir(), "trace")
	sink := New(Options{Root: root, MaxTotalBytes: 1 << 20, MaxEventBytes: 1 << 20})
	sink.Configure(true)
	session := sink.Start(RequestMeta{RequestID: "concurrent"})
	if session == nil {
		t.Fatal("trace session is nil")
	}
	const events = 64
	var wg sync.WaitGroup
	for index := 0; index < events; index++ {
		index := index
		wg.Add(1)
		go func() {
			defer wg.Done()
			session.Event("parallel", map[string]any{"index": index})
		}()
	}
	wg.Wait()
	session.Close()
	sink.Close()

	file, err := os.Open(filepath.Join(root, eventsFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	count := 0
	for scanner.Scan() {
		var record eventRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("invalid event line: %v", err)
		}
		count++
	}
	if err := scanner.Err(); err != nil || count != events {
		t.Fatalf("events=%d err=%v", count, err)
	}
}

func TestSinkFailureIsReportedAndNonFatal(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "not-a-directory")
	if err := os.WriteFile(root, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	var reported error
	sink := New(Options{Root: root, ReportError: func(err error) { reported = err }})
	sink.Configure(true)
	if sink.Enabled() || reported == nil || sink.Start(RequestMeta{}) != nil {
		t.Fatalf("enabled=%v reported=%v", sink.Enabled(), reported)
	}
	// A failed optional trace must remain safe to close and reconfigure off.
	sink.Close()
	sink.Configure(false)
}

func TestJSONEncodingFailureDisablesTraceAndReports(t *testing.T) {
	root := filepath.Join(t.TempDir(), "trace")
	var reported error
	sink := New(Options{Root: root, ReportError: func(err error) { reported = err }})
	sink.Configure(true)
	session := sink.Start(RequestMeta{RequestID: "bad-json"})
	if session == nil {
		t.Fatal("trace session is nil")
	}
	if artifact := session.JSON("unsupported.json", make(chan int)); artifact != "" {
		t.Fatalf("unsupported value created artifact %q", artifact)
	}
	if sink.Enabled() || reported == nil || !strings.Contains(reported.Error(), "encode trace artifact") {
		t.Fatalf("enabled=%v reported=%v", sink.Enabled(), reported)
	}
	session.Close()
}

func TestSinkHotToggleInvalidatesInFlightSession(t *testing.T) {
	root := filepath.Join(t.TempDir(), "trace")
	sink := New(Options{Root: root, MaxTotalBytes: 1 << 20, MaxEventBytes: 1 << 20})
	if sink.Start(RequestMeta{}) != nil {
		t.Fatal("disabled sink started a session")
	}
	sink.Configure(true)
	old := sink.Start(RequestMeta{RequestID: "old"})
	old.Event("before_disable", nil)
	sink.Configure(false)
	old.Event("after_disable", nil)
	old.Close()
	if sink.Start(RequestMeta{}) != nil {
		t.Fatal("disabled sink started a session after hot toggle")
	}
	sink.Configure(true)
	current := sink.Start(RequestMeta{RequestID: "current"})
	if current == nil {
		t.Fatal("re-enabled sink did not start a session")
	}
	current.Event("after_enable", nil)
	current.Close()
	sink.Close()

	raw, err := os.ReadFile(filepath.Join(root, eventsFileName))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "before_disable") || strings.Contains(text, "after_disable") || !strings.Contains(text, "after_enable") {
		t.Fatalf("events after hot toggles = %s", text)
	}
}

func TestSinkRotatesEventStream(t *testing.T) {
	root := filepath.Join(t.TempDir(), "trace")
	sink := New(Options{Root: root, MaxTotalBytes: 1 << 20, MaxEventBytes: 64, EventBackups: 2})
	sink.Configure(true)
	session := sink.Start(RequestMeta{RequestID: "rotation"})
	session.Event("first_large_event", map[string]any{"payload": strings.Repeat("x", 128)})
	session.Event("second_event", nil)
	session.Close()
	sink.Close()

	rotated, err := os.ReadFile(filepath.Join(root, eventsFileName+".1"))
	if err != nil || !strings.Contains(string(rotated), "first_large_event") {
		t.Fatalf("rotated events = %s, err=%v", rotated, err)
	}
	current, err := os.ReadFile(filepath.Join(root, eventsFileName))
	if err != nil || !strings.Contains(string(current), "second_event") {
		t.Fatalf("current events = %s, err=%v", current, err)
	}
}

func TestSinkPrunesOnlyCompleteInactiveBundles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "trace")
	sink := New(Options{Root: root, MaxTotalBytes: 32, MaxEventBytes: 1 << 20})
	sink.Configure(true)
	first := sink.Start(RequestMeta{RequestID: "first"})
	first.Artifact("payload.txt", []byte(strings.Repeat("a", 24)))
	first.Close()
	second := sink.Start(RequestMeta{RequestID: "second"})
	second.Artifact("payload.txt", []byte(strings.Repeat("b", 24)))
	second.Close()
	sink.Close()

	entries, err := os.ReadDir(filepath.Join(root, requestsDirectoryName))
	if err != nil || len(entries) != 1 || !strings.Contains(entries[0].Name(), "second") {
		t.Fatalf("remaining bundles=%v err=%v", entries, err)
	}
}
