package responses

import "fmt"

// ErrorKind identifies the class of a request planning or rewrite failure.
// The kind is stable and may be used by the interceptor when constructing its
// public error response.
type ErrorKind string

// LimitKind identifies the exact bounded resource that rejected a request.
// It is safe to expose to diagnostics because it never contains user input.
type LimitKind string

const (
	ErrorMalformedRequest ErrorKind = "malformed_request"
	ErrorUnsupportedImage ErrorKind = "unsupported_image_source"
	// ErrorUnsupportedImageSource is the descriptive alias used by callers
	// that prefer the full contract wording.
	ErrorUnsupportedImageSource ErrorKind = ErrorUnsupportedImage
	ErrorLimitsExceeded         ErrorKind = "limits_exceeded"
	ErrorMissingResult          ErrorKind = "missing_result"
	ErrorInvalidResult          ErrorKind = "invalid_result"
	ErrorRewriteVerification    ErrorKind = "rewrite_verification"

	LimitRequestBody    LimitKind = "request_body"
	LimitImageReference LimitKind = "image_reference"
	LimitImageCount     LimitKind = "image_count"
	LimitVLMResult      LimitKind = "vlm_result"
)

// Error is a typed planner error. StatusCode is the HTTP status that should be
// returned when the error is surfaced before an upstream request is made.
type Error struct {
	Kind       ErrorKind
	StatusCode int
	Message    string
	Path       string
	Limit      LimitKind
	Actual     int
	Maximum    int
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Path == "" {
		return fmt.Sprintf("%s: %s", e.Kind, e.Message)
	}
	return fmt.Sprintf("%s at %s: %s", e.Kind, e.Path, e.Message)
}

func plannerError(kind ErrorKind, status int, message, path string) *Error {
	return &Error{Kind: kind, StatusCode: status, Message: message, Path: path}
}

func malformed(message, path string) *Error {
	return plannerError(ErrorMalformedRequest, 400, message, path)
}

func unsupported(message, path string) *Error {
	return plannerError(ErrorUnsupportedImage, 422, message, path)
}

func limits(message, path string) *Error {
	return plannerError(ErrorLimitsExceeded, 413, message, path)
}

func limitExceeded(limit LimitKind, actual, maximum int, message, path string) *Error {
	err := limits(message, path)
	err.Limit = limit
	err.Actual = actual
	err.Maximum = maximum
	return err
}
