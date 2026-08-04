package responses

import "fmt"

// ErrorKind identifies the class of a request planning or rewrite failure.
// The kind is stable and may be used by the interceptor when constructing its
// public error response.
type ErrorKind string

// LimitKind identifies the exact bounded resource that rejected a request.
// It is safe to expose to diagnostics because it never contains user input.
type LimitKind string

// ImageCountDetails is a content-free breakdown of image blocks across a
// multi-turn Responses input. References themselves are never stored here.
type ImageCountDetails struct {
	InputItems            int `json:"input_items"`
	ImageInputItems       int `json:"image_input_items"`
	ImageBlocks           int `json:"image_blocks"`
	UniqueImageReferences int `json:"unique_image_references"`
	DuplicateImageBlocks  int `json:"duplicate_image_blocks"`
	LastImageItemIndex    int `json:"last_image_item_index"`
	LastImageItemBlocks   int `json:"last_image_item_blocks"`
	EarlierImageBlocks    int `json:"earlier_image_blocks"`
	ContentImages         int `json:"content_images"`
	FunctionOutputImages  int `json:"function_output_images"`
}

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
	Kind        ErrorKind
	StatusCode  int
	Message     string
	Path        string
	Limit       LimitKind
	Actual      int
	Maximum     int
	ImageCount  *ImageCountDetails
	DebugImages []Image
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

func imageCountExceeded(actual, maximum int, details ImageCountDetails, images []Image) *Error {
	err := limitExceeded(LimitImageCount, actual, maximum, "request contains too many images", "input")
	err.ImageCount = &details
	err.DebugImages = append([]Image(nil), images...)
	return err
}
