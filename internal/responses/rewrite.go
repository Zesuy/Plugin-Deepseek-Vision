package responses

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

const omittedImageReference = "[image reference omitted]"

const downstreamVisionNotice = `[Vision preprocessing notice]
The target model cannot inspect image attachments directly. The images in this prompt have already been analyzed by a vision model. Use the joint visual analysis below as their visual content. Do not call view_image or other image/file tools to reopen these already analyzed attachments.`

type groupedRewriteResult struct {
	group PromptGroup
	text  string
}

var (
	// Generated VLM text is untrusted. These patterns remove source-like
	// tokens even when they are not byte-for-byte equal to an input reference.
	rawDataImagePattern = regexp.MustCompile(`(?i)data:(?:/|\\/)?image(?:/|\\/)[a-z0-9][a-z0-9.+-]*(?:;[^,\s]*)*,[^\s<>"'` + "`" + `]+(?:\r?\n[ \t]*[a-z0-9+/_=%-]{8,})*`)
	// Models sometimes percent-encode only the scheme, only the slash, or
	// alternate escape casing. Match all mixed forms before the generated text
	// is sent back to a non-vision downstream.
	encodedDataImagePattern = regexp.MustCompile(`(?i)data(?:%3a|:)image(?:%2f|/)[^\s<>"'` + "`" + `]+`)
	// Credential-bearing URLs are never useful visual transcription and may
	// contain secrets. Ordinary unrelated HTTP(S) document URLs are preserved.
	credentialHTTPURLPattern = regexp.MustCompile(`(?i)https?:(?:(?:/|\\/){2})[^\s<>"'` + "`" + `/@]+(?::[^\s<>"'` + "`" + `/@]*)?@[^\s<>"'` + "`" + `]+`)
	codexImageWrapperPattern = regexp.MustCompile(`(?i)^<image\b[^>]*\bpath="([^"]+)"[^>]*>$`)
)

// Rewrite replaces every discovered image with one input_text block. It first
// validates every result, then operates on a fresh tree, so a missing or bad
// result can never produce a partially rewritten body.
func (p *Plan) Rewrite(results []ImageResult) ([]byte, error) {
	if p == nil {
		return nil, plannerError(ErrorMalformedRequest, 400, "nil response plan", "body")
	}
	if len(p.images) == 0 {
		return append([]byte(nil), p.original...), nil
	}
	if len(results) != len(p.images) {
		return nil, plannerError(ErrorMissingResult, 502, "one VLM result is required for each image", "input")
	}
	for i, result := range results {
		if strings.TrimSpace(result.VisibleText) == "" && strings.TrimSpace(result.VisualDescription) == "" {
			return nil, plannerError(ErrorInvalidResult, 502, fmt.Sprintf("VLM result %d is empty", i+1), "input")
		}
		resultChars := runeLen(result.VisibleText) + runeLen(result.VisualDescription)
		if p.options.MaxResultChars > 0 && resultChars > p.options.MaxResultChars {
			return nil, limitExceeded(LimitVLMResult, resultChars, p.options.MaxResultChars, "VLM result exceeds configured limit", "input")
		}
	}

	root, err := cloneJSON(p.root)
	if err != nil {
		return nil, plannerError(ErrorRewriteVerification, 500, "failed to clone request body", "body")
	}
	object, ok := root.(map[string]any)
	if !ok {
		return nil, malformed("Responses request must be a JSON object", "body")
	}

	resultByLocation := make(map[locationKey]ImageResult, len(p.images))
	for i, image := range p.images {
		resultByLocation[makeLocationKey(image.location)] = redactImageReferences(results[i], p.images)
	}
	replaced := 0
	if items, ok := object["input"].([]any); ok {
		for inputIndex, item := range items {
			itemObject, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if content, ok := itemObject["content"].([]any); ok {
				for blockIndex, block := range content {
					blockObject, ok := block.(map[string]any)
					if !ok {
						continue
					}
					if typeName, _ := blockObject["type"].(string); typeName != "input_image" {
						continue
					}
					location := imageLocation{kind: locationContent, input: inputIndex, block: blockIndex}
					result, ok := resultByLocation[makeLocationKey(location)]
					if !ok {
						return nil, plannerError(ErrorRewriteVerification, 500, "discovered image location changed before rewrite", locationPath(location))
					}
					content[blockIndex] = replacementBlock(p.images[replaced].Number, result)
					replaced++
				}
			}
			if itemType, _ := itemObject["type"].(string); itemType == "function_call_output" {
				if output, ok := itemObject["output"].([]any); ok {
					for blockIndex, block := range output {
						blockObject, ok := block.(map[string]any)
						if !ok {
							continue
						}
						if typeName, _ := blockObject["type"].(string); typeName != "input_image" {
							continue
						}
						location := imageLocation{kind: locationFunctionOutput, input: inputIndex, block: blockIndex}
						result, ok := resultByLocation[makeLocationKey(location)]
						if !ok {
							return nil, plannerError(ErrorRewriteVerification, 500, "discovered image location changed before rewrite", locationPath(location))
						}
						output[blockIndex] = replacementBlock(p.images[replaced].Number, result)
						replaced++
					}
				}
			}
		}
	}
	if replaced != len(p.images) {
		return nil, plannerError(ErrorRewriteVerification, 500, "not all discovered images were rewritten", "input")
	}

	body, err := json.Marshal(root)
	if err != nil {
		return nil, plannerError(ErrorRewriteVerification, 500, "rewritten request could not be encoded", "body")
	}
	// This second discovery is structural (it does not inspect text contents)
	// and protects the executor boundary if this package is changed later.
	check, err := Discover(body, p.options)
	if err != nil {
		return nil, plannerError(ErrorRewriteVerification, 500, "rewritten request failed verification", "body")
	}
	if check.HasImages() {
		return nil, plannerError(ErrorRewriteVerification, 500, "rewritten request still contains an input_image", "body")
	}
	return body, nil
}

// RewriteText is a convenience adapter for analyzers that return one plain
// string per image. Each string is parsed with ParseVLMText and then passed
// through the same all-or-nothing Rewrite path.
func (p *Plan) RewriteText(results []string) ([]byte, error) {
	structured := make([]ImageResult, len(results))
	for i, result := range results {
		structured[i] = ParseVLMText(result)
	}
	return p.Rewrite(structured)
}

// RewriteGroupsText replaces every image with a lightweight numbered marker
// and appends one joint visual analysis to the originating prompt item. This
// preserves image order without duplicating a large model result per image.
func (p *Plan) RewriteGroupsText(results []string) ([]byte, error) {
	if p == nil {
		return nil, plannerError(ErrorMalformedRequest, 400, "nil response plan", "body")
	}
	if len(p.groups) == 0 {
		return append([]byte(nil), p.original...), nil
	}
	if len(results) != len(p.groups) {
		return nil, plannerError(ErrorMissingResult, 502, "one VLM result is required for each prompt group", "input")
	}
	byLocation := make(map[locationKey]groupedRewriteResult, len(p.groups))
	for i := range p.groups {
		text := strings.TrimSpace(redactGroupText(results[i], p.images))
		if text == "" {
			return nil, plannerError(ErrorInvalidResult, 502, fmt.Sprintf("VLM group result %d is empty", i+1), "input")
		}
		if p.options.MaxResultChars > 0 && runeLen(text) > p.options.MaxResultChars {
			return nil, limitExceeded(LimitVLMResult, runeLen(text), p.options.MaxResultChars, "VLM result exceeds configured limit", "input")
		}
		group := p.groups[i]
		byLocation[locationKey{kind: group.locationKind, input: group.InputIndex, block: -1}] = groupedRewriteResult{group: group, text: text}
	}

	root, err := cloneJSON(p.root)
	if err != nil {
		return nil, plannerError(ErrorRewriteVerification, 500, "failed to clone request body", "body")
	}
	object, ok := root.(map[string]any)
	if !ok {
		return nil, malformed("Responses request must be a JSON object", "body")
	}
	replaced := 0
	var analyzedAttachmentPaths []string
	if items, ok := object["input"].([]any); ok {
		for inputIndex, item := range items {
			itemObject, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if content, ok := itemObject["content"].([]any); ok {
				updated, count, paths, rewriteErr := rewritePromptGroupBlocks(content, inputIndex, locationContent, byLocation)
				if rewriteErr != nil {
					return nil, rewriteErr
				}
				itemObject["content"] = updated
				replaced += count
				analyzedAttachmentPaths = append(analyzedAttachmentPaths, paths...)
			}
			if itemType, _ := itemObject["type"].(string); itemType == "function_call_output" {
				if output, ok := itemObject["output"].([]any); ok {
					updated, count, paths, rewriteErr := rewritePromptGroupBlocks(output, inputIndex, locationFunctionOutput, byLocation)
					if rewriteErr != nil {
						return nil, rewriteErr
					}
					itemObject["output"] = updated
					replaced += count
					analyzedAttachmentPaths = append(analyzedAttachmentPaths, paths...)
				}
			}
		}
	}
	if replaced != len(p.images) {
		return nil, plannerError(ErrorRewriteVerification, 500, "not all discovered images were rewritten", "input")
	}
	redactAnalyzedAttachmentPaths(root, analyzedAttachmentPaths)
	body, err := json.Marshal(root)
	if err != nil {
		return nil, plannerError(ErrorRewriteVerification, 500, "rewritten request could not be encoded", "body")
	}
	check, err := Discover(body, p.options)
	if err != nil || check.HasImages() {
		return nil, plannerError(ErrorRewriteVerification, 500, "rewritten request still contains an input_image", "body")
	}
	return body, nil
}

func rewritePromptGroupBlocks(blocks []any, inputIndex int, kind locationKind, results map[locationKey]groupedRewriteResult) ([]any, int, []string, error) {
	key := locationKey{kind: kind, input: inputIndex, block: -1}
	result, hasGroup := results[key]
	if !hasGroup {
		return blocks, 0, nil, nil
	}
	paths := sanitizeAnalyzedAttachmentMetadata(blocks)
	imageIndex := 0
	for blockIndex, block := range blocks {
		blockObject, ok := block.(map[string]any)
		if !ok {
			continue
		}
		if typeName, _ := blockObject["type"].(string); typeName != "input_image" {
			continue
		}
		if imageIndex >= len(result.group.Images) {
			return nil, 0, nil, plannerError(ErrorRewriteVerification, 500, "prompt group image count changed before rewrite", locationPath(imageLocation{kind: kind, input: inputIndex, block: blockIndex}))
		}
		blocks[blockIndex] = map[string]any{
			"type": "input_text",
			"text": fmt.Sprintf("[Image %d — already analyzed; the target model cannot read this attachment directly. Use the joint visual analysis below and do not call view_image for it.]", imageIndex+1),
		}
		imageIndex++
	}
	if imageIndex != len(result.group.Images) {
		return nil, 0, nil, plannerError(ErrorRewriteVerification, 500, "prompt group image locations changed before rewrite", "input")
	}
	blocks = append(blocks, map[string]any{
		"type": "input_text",
		"text": RenderGroupResult(result.group, result.text),
	})
	return blocks, imageIndex, paths, nil
}

func RenderGroupResult(group PromptGroup, text string) string {
	numbers := make([]string, len(group.Images))
	for i := range group.Images {
		numbers[i] = fmt.Sprintf("%d", i+1)
	}
	return fmt.Sprintf("%s\n\n[Images %s — Joint visual analysis]\n\n%s", downstreamVisionNotice, strings.Join(numbers, ", "), strings.TrimSpace(text))
}

func sanitizeAnalyzedAttachmentMetadata(blocks []any) []string {
	var paths []string
	for _, block := range blocks {
		object, ok := block.(map[string]any)
		if !ok {
			continue
		}
		text, ok := object["text"].(string)
		if !ok {
			continue
		}
		match := codexImageWrapperPattern.FindStringSubmatch(strings.TrimSpace(text))
		if len(match) == 2 && match[1] != "" {
			backslashPath := strings.ReplaceAll(match[1], "/", `\`)
			paths = append(paths,
				match[1],
				strings.ReplaceAll(match[1], `\`, "/"),
				backslashPath,
				strings.ReplaceAll(backslashPath, `\`, `\\`),
			)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	for _, block := range blocks {
		object, ok := block.(map[string]any)
		if !ok {
			continue
		}
		text, ok := object["text"].(string)
		if !ok {
			continue
		}
		trimmed := strings.TrimSpace(text)
		if codexImageWrapperPattern.MatchString(trimmed) {
			object["text"] = "[Analyzed image attachment metadata omitted]"
			continue
		}
		if strings.EqualFold(trimmed, "</image>") {
			object["text"] = "[End analyzed image attachment]"
			continue
		}
		for _, path := range paths {
			if path != "" {
				text = strings.ReplaceAll(text, path, "[analyzed image attachment path omitted]")
			}
		}
		object["text"] = text
	}
	return paths
}

func redactAnalyzedAttachmentPaths(value any, paths []string) {
	if len(paths) == 0 {
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if text, ok := child.(string); ok {
				for _, path := range paths {
					if path != "" {
						text = strings.ReplaceAll(text, path, "[analyzed image attachment path omitted]")
					}
				}
				typed[key] = text
				continue
			}
			redactAnalyzedAttachmentPaths(child, paths)
		}
	case []any:
		for _, child := range typed {
			redactAnalyzedAttachmentPaths(child, paths)
		}
	}
}

func redactGroupText(text string, images []Image) string {
	result := ImageResult{VisualDescription: text}
	return redactImageReferences(result, images).VisualDescription
}

// redactImageReferences prevents a model that echoes the source reference
// from reintroducing an image URL/data URI into the executor-bound body. The
// source is replaced only in generated result text; unrelated request fields
// remain untouched.
func redactImageReferences(result ImageResult, images []Image) ImageResult {
	for _, image := range images {
		if image.Reference == "" {
			continue
		}
		for _, variant := range referenceVariants(image.Reference) {
			result.VisibleText = replaceFoldedReference(result.VisibleText, variant)
			result.VisualDescription = replaceFoldedReference(result.VisualDescription, variant)
		}
	}
	result.VisibleText = redactSourceTokens(result.VisibleText)
	result.VisualDescription = redactSourceTokens(result.VisualDescription)
	return result
}

func referenceVariants(reference string) []string {
	bases := []string{reference}
	if canonical, ok := canonicalHTTPReference(reference); ok && canonical != reference {
		bases = append(bases, canonical)
	}
	variants := make([]string, 0, len(bases)*5)
	for _, base := range bases {
		variants = append(variants,
			base,
			strings.ReplaceAll(base, "/", `\/`),
			url.QueryEscape(base),
			url.PathEscape(base),
		)
		if encoded, err := json.Marshal(base); err == nil && len(encoded) >= 2 {
			variants = append(variants, string(encoded[1:len(encoded)-1]))
		}
	}
	// Percent escapes are case-insensitive. Preserve ordinary characters while
	// adding the common lowercase-hex form used by model output.
	for _, variant := range append([]string(nil), variants...) {
		variants = append(variants, lowerPercentEscapes(variant))
	}
	seen := make(map[string]struct{}, len(variants))
	unique := variants[:0]
	for _, variant := range variants {
		if variant == "" {
			continue
		}
		if _, ok := seen[variant]; ok {
			continue
		}
		seen[variant] = struct{}{}
		unique = append(unique, variant)
	}
	return unique
}

// canonicalHTTPReference covers a common VLM normalization: URI scheme and
// DNS hostname are emitted in lowercase while the path, signed query and
// fragment retain their case-sensitive bytes. URL.String uses RawPath,
// RawQuery and RawFragment from the parsed value when available.
func canonicalHTTPReference(reference string) (string, bool) {
	u, err := url.Parse(reference)
	if err != nil || (!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) || u.Hostname() == "" {
		return "", false
	}
	u.Scheme = strings.ToLower(u.Scheme)
	hostname := strings.ToLower(u.Hostname())
	if strings.Contains(hostname, ":") {
		hostname = "[" + hostname + "]"
	}
	if port := u.Port(); port != "" {
		hostname += ":" + port
	}
	u.Host = hostname
	return u.String(), true
}

func lowerPercentEscapes(value string) string {
	bytes := []byte(value)
	for i := 0; i+2 < len(bytes); i++ {
		if bytes[i] != '%' {
			continue
		}
		bytes[i+1] = asciiLower(bytes[i+1])
		bytes[i+2] = asciiLower(bytes[i+2])
		i += 2
	}
	return string(bytes)
}

func asciiLower(value byte) byte {
	if value >= 'A' && value <= 'F' {
		return value + ('a' - 'A')
	}
	return value
}

// replaceFoldedReference replaces exact and line-wrapped forms. Only ASCII
// whitespace may be skipped, so ordinary prose that merely shares a prefix
// with a URL remains unchanged. Percent-escape hex digits are compared
// case-insensitively because their URI spellings represent the same octet.
func replaceFoldedReference(text, reference string) string {
	if text == "" || reference == "" {
		return text
	}
	var output strings.Builder
	remaining := text
	for len(remaining) > 0 {
		start := strings.IndexByte(remaining, reference[0])
		if start < 0 {
			output.WriteString(remaining)
			break
		}
		output.WriteString(remaining[:start])
		end, matched := matchIgnoringASCIIWhitespace(remaining[start:], reference)
		if !matched {
			output.WriteByte(remaining[start])
			remaining = remaining[start+1:]
			continue
		}
		output.WriteString(omittedImageReference)
		remaining = remaining[start+end:]
	}
	return output.String()
}

func matchIgnoringASCIIWhitespace(text, reference string) (int, bool) {
	i, j := 0, 0
	for i < len(text) && j < len(reference) {
		if isASCIIWhitespace(text[i]) {
			i++
			continue
		}
		if text[i] == '%' && reference[j] == '%' && i+2 < len(text) && j+2 < len(reference) &&
			isASCIIHex(text[i+1]) && isASCIIHex(text[i+2]) &&
			isASCIIHex(reference[j+1]) && isASCIIHex(reference[j+2]) {
			if asciiLower(text[i+1]) != asciiLower(reference[j+1]) || asciiLower(text[i+2]) != asciiLower(reference[j+2]) {
				return 0, false
			}
			i += 3
			j += 3
			continue
		}
		if text[i] != reference[j] {
			return 0, false
		}
		i++
		j++
	}
	return i, j == len(reference)
}

func isASCIIWhitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}

func isASCIIHex(value byte) bool {
	return (value >= '0' && value <= '9') || (value >= 'a' && value <= 'f') || (value >= 'A' && value <= 'F')
}

func redactSourceTokens(text string) string {
	text = rawDataImagePattern.ReplaceAllString(text, omittedImageReference)
	text = encodedDataImagePattern.ReplaceAllString(text, omittedImageReference)
	text = credentialHTTPURLPattern.ReplaceAllString(text, omittedImageReference)
	return redactPercentEncodedTokens(text)
}

// redactPercentEncodedTokens decodes only bounded candidate tokens while
// retaining the original text offsets for replacement. This catches mixed
// forms such as data%3A%69mage%2Fpng and percent-encoded credential URLs
// without globally unescaping ordinary prose or unrelated documentation URLs.
func redactPercentEncodedTokens(text string) string {
	const maxCandidateBytes = 4 << 20
	var out strings.Builder
	last := 0
	scan := 0
	changed := false
	for scan < len(text) {
		for scan < len(text) && isTokenDelimiter(text[scan]) {
			scan++
		}
		start := scan
		end := start
		for end < len(text) && !isTokenDelimiter(text[end]) {
			end++
		}
		coreStart, coreEnd := trimTokenPunctuation(text, start, end)
		if coreEnd > coreStart && coreEnd-coreStart <= maxCandidateBytes && strings.ContainsAny(text[coreStart:coreEnd], "%\\") {
			candidate := strings.ReplaceAll(text[coreStart:coreEnd], `\/`, "/")
			decoded := candidate
			valid := true
			for i := 0; i < 3; i++ {
				unescaped, err := url.PathUnescape(decoded)
				if err != nil {
					valid = false
					break
				}
				if unescaped == decoded {
					break
				}
				decoded = unescaped
			}
			if valid && (rawDataImagePattern.MatchString(decoded) || credentialHTTPURLPattern.MatchString(decoded)) {
				if !changed {
					out.Grow(len(text))
				}
				out.WriteString(text[last:coreStart])
				out.WriteString(omittedImageReference)
				out.WriteString(text[coreEnd:end])
				last = end
				changed = true
			}
		}
		if end <= scan {
			scan++
		} else {
			scan = end
		}
	}
	if !changed {
		return text
	}
	out.WriteString(text[last:])
	return out.String()
}

func isTokenDelimiter(value byte) bool {
	switch value {
	case ' ', '\t', '\r', '\n', '<', '>', '"', '\'', '`':
		return true
	default:
		return false
	}
}

func trimTokenPunctuation(text string, start, end int) (int, int) {
	for start < end && strings.ContainsRune("([{", rune(text[start])) {
		start++
	}
	for end > start && strings.ContainsRune(".,;:)]}", rune(text[end-1])) {
		end--
	}
	return start, end
}

func replacementBlock(number int, result ImageResult) map[string]any {
	return map[string]any{
		"type": "input_text",
		"text": RenderResult(number, result),
	}
}

// RenderResult applies the frozen replacement template. It is exported so the
// VLM owner can use the same rendering in tests without duplicating wording.
func RenderResult(number int, result ImageResult) string {
	return fmt.Sprintf("[Image %d — Visual analysis]\n\nVisible text:\n%s\n\nVisual description:\n%s",
		number, strings.TrimSpace(result.VisibleText), strings.TrimSpace(result.VisualDescription))
}

// ParseVLMText converts a conventional Luna response into the structured
// result expected by Rewrite. It accepts the frozen headings, a JSON object
// with visible_text/visual_description keys, or plain text (plain text is
// treated as the visual description while explicitly recording that no text
// was detected).
func ParseVLMText(text string) ImageResult {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ImageResult{}
	}
	var object struct {
		VisibleText       string `json:"visible_text"`
		VisualDescription string `json:"visual_description"`
	}
	if json.Unmarshal([]byte(trimmed), &object) == nil && (object.VisibleText != "" || object.VisualDescription != "") {
		return ImageResult{VisibleText: object.VisibleText, VisualDescription: object.VisualDescription}
	}
	visibleMarker := "Visible text:"
	descriptionMarker := "Visual description:"
	visibleIndex := strings.Index(trimmed, visibleMarker)
	descriptionIndex := strings.Index(trimmed, descriptionMarker)
	if visibleIndex >= 0 && descriptionIndex > visibleIndex {
		visible := strings.TrimSpace(trimmed[visibleIndex+len(visibleMarker) : descriptionIndex])
		description := strings.TrimSpace(trimmed[descriptionIndex+len(descriptionMarker):])
		return ImageResult{VisibleText: visible, VisualDescription: description}
	}
	return ImageResult{VisibleText: "(no visible text detected)", VisualDescription: trimmed}
}

func cloneJSON(value any) (any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return decodeJSON(encoded)
}

func runeLen(value string) int { return len([]rune(value)) }
