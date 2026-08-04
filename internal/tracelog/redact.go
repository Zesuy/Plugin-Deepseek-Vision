package tracelog

import (
	"net/http"
	"net/url"
	"strings"
)

// SensitiveName recognizes credential-bearing header and metadata keys across
// the common dash/underscore/dot/space spelling variants.
func SensitiveName(name string) bool {
	compact := strings.NewReplacer("-", "", "_", "", ".", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(name)))
	for _, marker := range []string{
		"authorization", "apikey", "accesstoken", "refreshtoken", "token",
		"clientsecret", "secret", "cookie", "credential", "password", "passwd",
	} {
		if strings.Contains(compact, marker) {
			return true
		}
	}
	return false
}

func RedactValues(values url.Values) url.Values {
	if len(values) == 0 {
		return nil
	}
	clone := make(url.Values, len(values))
	for key, entries := range values {
		if SensitiveName(key) {
			clone[key] = []string{"[REDACTED]"}
		} else {
			clone[key] = append([]string(nil), entries...)
		}
	}
	return clone
}

func RedactHeaders(headers http.Header) http.Header {
	if len(headers) == 0 {
		return nil
	}
	clone := make(http.Header, len(headers))
	for key, values := range headers {
		if SensitiveName(key) {
			clone[key] = []string{"[REDACTED]"}
		} else {
			clone[key] = append([]string(nil), values...)
		}
	}
	return clone
}

func RedactJSONLike(value any, key string) any {
	if SensitiveName(key) {
		return "[REDACTED]"
	}
	switch typed := value.(type) {
	case map[string]any:
		clone := make(map[string]any, len(typed))
		for childKey, childValue := range typed {
			clone[childKey] = RedactJSONLike(childValue, childKey)
		}
		return clone
	case map[string]string:
		clone := make(map[string]any, len(typed))
		for childKey, childValue := range typed {
			clone[childKey] = RedactJSONLike(childValue, childKey)
		}
		return clone
	case []any:
		clone := make([]any, len(typed))
		for index := range typed {
			clone[index] = RedactJSONLike(typed[index], key)
		}
		return clone
	default:
		return value
	}
}
