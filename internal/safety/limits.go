// Package safety contains hard limits and validation helpers used by the
// image preprocessing pipeline.  These checks intentionally fail closed: an
// unknown or unbounded image reference is never sent to an upstream service.
package safety

import (
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/url"
	"strings"
)

var (
	ErrEmptyImageReference       = errors.New("image reference is empty")
	ErrImageReferenceTooLarge    = errors.New("image reference exceeds size limit")
	ErrUnsupportedImageReference = errors.New("image reference must be an http(s) URL or image data URI")
	ErrResultTooLarge            = errors.New("visual result exceeds size limit")
)

// Limits are hard safety limits. A zero value means that the corresponding
// check is disabled; callers should normally provide explicit non-zero values.
type Limits struct {
	MaxImages              int
	MaxImageReferenceBytes int
	MaxResultChars         int
}

func (l Limits) ValidateImageCount(n int) error {
	if n < 0 {
		return errors.New("image count is negative")
	}
	if l.MaxImages > 0 && n > l.MaxImages {
		return fmt.Errorf("image count %d exceeds limit %d", n, l.MaxImages)
	}
	return nil
}

// ValidateImageReference accepts only a network URL or a data URI carrying an
// image. file://, javascript://, credentials and other schemes are rejected.
func (l Limits) ValidateImageReference(ref string) error {
	return ValidateImageReference(ref, l.MaxImageReferenceBytes)
}

// ValidateImageReference validates the complete external image reference and
// applies an optional byte limit. It is the shared validator for discovery and
// the final host-model callback boundary.
func ValidateImageReference(ref string, maxBytes int) error {
	if strings.TrimSpace(ref) == "" {
		return ErrEmptyImageReference
	}
	if maxBytes > 0 && len(ref) > maxBytes {
		return ErrImageReferenceTooLarge
	}
	if len(ref) >= len("data:") && strings.EqualFold(ref[:len("data:")], "data:") {
		return validateDataImageReference(ref)
	}
	u, err := url.ParseRequestURI(ref)
	if err != nil || u.Opaque != "" ||
		(!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) ||
		u.Host == "" || u.Hostname() == "" || u.User != nil {
		return ErrUnsupportedImageReference
	}
	// Literal private, loopback, link-local and unspecified addresses are not
	// valid remote image sources. Blocking them here prevents the plugin from
	// handing an obvious SSRF target to a VLM that fetches URL references. DNS
	// names remain subject to the deployment's egress/allowlist policy because
	// resolving them in this synchronous validator would introduce rebinding
	// races and unpredictable latency.
	hostname := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if strings.Contains(hostname, "%") || isAlternateNumericHost(hostname) {
		return ErrUnsupportedImageReference
	}
	if ip := net.ParseIP(hostname); ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()) {
		return ErrUnsupportedImageReference
	}
	return nil
}

func isAlternateNumericHost(hostname string) bool {
	if hostname == "" {
		return false
	}
	parts := strings.Split(hostname, ".")
	for _, part := range parts {
		if part == "" {
			return false
		}
		if strings.HasPrefix(part, "0x") || strings.HasPrefix(part, "0X") {
			continue
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	// Bare decimal and dotted numeric hostnames are interpreted as IPv4 by
	// several HTTP stacks (for example 2130706433 or 0177.0.0.1 can map to
	// 127.0.0.1). Reject them rather than relying on resolver interpretation.
	return true
}

func validateDataImageReference(ref string) error {
	if strings.IndexFunc(ref, func(r rune) bool { return r <= 0x20 || r == 0x7f }) >= 0 {
		return ErrUnsupportedImageReference
	}
	comma := strings.IndexByte(ref, ',')
	if comma <= len("data:") || comma == len(ref)-1 {
		return ErrUnsupportedImageReference
	}
	header := ref[len("data:"):comma]
	payload := ref[comma+1:]
	parts := strings.Split(header, ";")
	if len(parts) == 0 || parts[0] == "" {
		return ErrUnsupportedImageReference
	}

	isBase64 := false
	mimeParts := []string{parts[0]}
	for i, part := range parts[1:] {
		if strings.EqualFold(part, "base64") {
			if isBase64 || i != len(parts[1:])-1 {
				return ErrUnsupportedImageReference
			}
			isBase64 = true
			continue
		}
		mimeParts = append(mimeParts, part)
	}
	mediaType, _, err := mime.ParseMediaType(strings.Join(mimeParts, ";"))
	mediaType = strings.ToLower(mediaType)
	if err != nil || !strings.HasPrefix(mediaType, "image/") || len(mediaType) == len("image/") || mediaType == "image/*" {
		return ErrUnsupportedImageReference
	}
	decoded, err := url.PathUnescape(payload)
	if err != nil {
		return ErrUnsupportedImageReference
	}
	if isBase64 {
		if _, err = base64.StdEncoding.Strict().DecodeString(decoded); err != nil {
			if _, rawErr := base64.RawStdEncoding.Strict().DecodeString(decoded); rawErr != nil {
				return ErrUnsupportedImageReference
			}
		}
	}
	return nil
}

func (l Limits) ValidateResult(text string) error {
	if l.MaxResultChars > 0 && len([]rune(text)) > l.MaxResultChars {
		return ErrResultTooLarge
	}
	return nil
}
