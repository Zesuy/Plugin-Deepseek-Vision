package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static void clear_host_api(void) {
	stored_host = NULL;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/zesuy/Plugin-Deepseek-Vision/internal/config"
)

const (
	// maxABIEnvelopeOverhead reserves bounded space for the JSON fields around
	// Body (headers, metadata, model IDs, and tracing fields). Body itself is a
	// []byte and therefore expands to base64 in the lifecycle RPC JSON.
	maxABIEnvelopeOverhead uint64 = 4 << 20
	maxConfiguredBodyBytes uint64 = config.MaxRequestBytesLimit
	maxABIRequestBytes     uint64 = ((maxConfiguredBodyBytes + 2) / 3 * 4) + maxABIEnvelopeOverhead
	// maxABIBudgetBytes bounds the amount of raw RPC memory admitted before the
	// request is decoded.  A request at the larger configuration ceiling would
	// otherwise be copied by C.GoBytes and then expanded again by JSON/base64
	// decoding before any per-request limiter can run.
	// Keep the effective process budget below the configured 100 MiB logical
	// body ceiling because the ABI envelope, JSON decoder and rewrite tree each
	// create additional representations of the body after admission. One normal
	// 20 MiB body still fits, while several near-limit copies cannot pile up.
	maxABIBudgetBytes      uint64 = 32 << 20
	maxABIInFlightRequests int64  = 4
	maxCIntValue           uint64 = 1<<31 - 1
	maxHostCallbackBytes          = 16 << 20
)

var (
	errABIRequestTooLarge       = errors.New("ABI request exceeds the plugin hard limit")
	errABIRequestLengthOverflow = errors.New("ABI request length exceeds C.int")
	errABIAdmissionBusy         = errors.New("ABI request memory budget is exhausted")
)

type abiAdmissionFailure string

const (
	abiAdmissionOK            abiAdmissionFailure = ""
	abiAdmissionRequestBudget abiAdmissionFailure = "request_budget"
	abiAdmissionRequestCount  abiAdmissionFailure = "request_count"
	abiAdmissionProcessBudget abiAdmissionFailure = "process_budget"
)

var (
	abiInFlightRequests atomic.Int64
	abiInFlightBytes    atomic.Uint64
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(1)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		_ = writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	methodName := C.GoString(method)
	checkedLength, err := checkedABIRequestLength(uint64(requestLen))
	if err != nil {
		traceABIRejection("abi_request_limit", uint64(requestLen))
		raw, returnCode := oversizedRequestResult(methodName, err)
		if !writeResponse(response, raw) {
			return 1
		}
		return C.int(returnCode)
	}
	if admissionFailure := acquireABIAdmission(uint64(checkedLength)); admissionFailure != abiAdmissionOK {
		traceABIRejection(string(admissionFailure), uint64(checkedLength))
		raw, returnCode := oversizedRequestResult(methodName, errABIAdmissionBusy)
		if !writeResponse(response, raw) {
			return 1
		}
		return C.int(returnCode)
	}
	defer releaseABIAdmission(uint64(checkedLength))
	if checkedLength > 0 && request == nil {
		_ = writeResponse(response, errorEnvelope("invalid_request", "request pointer is required for a non-empty request"))
		return 1
	}
	var requestBytes []byte
	if checkedLength > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(checkedLength))
	}
	raw, err := handleMethod(methodName, requestBytes)
	if err != nil {
		_ = writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	if !writeResponse(response, raw) {
		return 1
	}
	return 0
}

func acquireABIAdmission(length uint64) abiAdmissionFailure {
	if length > maxABIBudgetBytes {
		return abiAdmissionRequestBudget
	}
	for {
		current := abiInFlightRequests.Load()
		if current >= maxABIInFlightRequests {
			return abiAdmissionRequestCount
		}
		if abiInFlightRequests.CompareAndSwap(current, current+1) {
			break
		}
	}
	for {
		current := abiInFlightBytes.Load()
		if current > maxABIBudgetBytes-length {
			abiInFlightRequests.Add(-1)
			return abiAdmissionProcessBudget
		}
		if !abiInFlightBytes.CompareAndSwap(current, current+length) {
			continue
		}
		return abiAdmissionOK
	}
}

func traceABIRejection(reason string, requestBytes uint64) {
	defer func() { _ = recover() }()
	message := fmt.Sprintf(
		"deepseek-vision RPC rejected limit_kind=%s abi_request_bytes=%d abi_request_limit_bytes=%d abi_process_budget_bytes=%d abi_in_flight_bytes=%d abi_in_flight_requests=%d",
		reason, requestBytes, maxABIRequestBytes, maxABIBudgetBytes, abiInFlightBytes.Load(), abiInFlightRequests.Load(),
	)
	emitHostDiagnostic("", "warn", message, map[string]any{
		"limit_kind":               reason,
		"abi_request_bytes":        requestBytes,
		"abi_request_limit_bytes":  maxABIRequestBytes,
		"abi_process_budget_bytes": maxABIBudgetBytes,
		"abi_in_flight_bytes":      abiInFlightBytes.Load(),
		"abi_in_flight_requests":   abiInFlightRequests.Load(),
	})
}

func releaseABIAdmission(length uint64) {
	abiInFlightBytes.Add(^uint64(length - 1))
	abiInFlightRequests.Add(-1)
}

func base64EncodedLength(length uint64) uint64 {
	return (length + 2) / 3 * 4
}

func oversizedInterceptorEnvelope(method string) ([]byte, bool) {
	if method != pluginabi.MethodRequestInterceptBefore && method != pluginabi.MethodRequestInterceptAfter {
		return nil, false
	}
	raw, err := terminateJSON(413, "request_too_large", "intercepted request exceeds the plugin ABI size limit")
	if err != nil {
		// terminateJSON marshals a fixed JSON-compatible response and cannot fail
		// for these arguments. Treat an impossible encoder failure as unwritable.
		return nil, true
	}
	return raw, true
}

func oversizedRequestResult(method string, lengthErr error) ([]byte, int) {
	if raw, terminate := oversizedInterceptorEnvelope(method); terminate {
		return raw, 0
	}
	return errorEnvelope("request_too_large", lengthErr.Error()), 1
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	shutdownPlugin()
	C.clear_host_api()
}

func callHost(method string, payload any) (json.RawMessage, error) {
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal host callback payload: %w", err)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, errors.New("allocate host callback payload")
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}
	callCode := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	if response.ptr != nil {
		defer C.free_host_buffer(response.ptr, response.len)
	}
	if uint64(response.len) > maxHostCallbackBytes || uint64(response.len) > maxCIntValue {
		return nil, errors.New("host callback response exceeds size limit")
	}
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if callCode != 0 || len(rawResponse) == 0 {
		return nil, errors.New("host callback failed")
	}
	var env envelope
	if err := json.Unmarshal(rawResponse, &env); err != nil {
		return nil, errors.New("host callback returned an invalid envelope")
	}
	if !env.OK {
		return nil, errors.New("host callback failed")
	}
	return append(json.RawMessage(nil), env.Result...), nil
}

func checkedABIRequestLength(length uint64) (int, error) {
	if length > maxCIntValue {
		return 0, errABIRequestLengthOverflow
	}
	if length > maxABIRequestBytes {
		return 0, errABIRequestTooLarge
	}
	return int(length), nil
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) bool {
	if response == nil || len(raw) == 0 {
		return false
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return false
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
	return true
}
