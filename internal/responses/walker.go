// Package responses contains the side-effect-free Responses body planner and
// rewriter used by the request interceptor. It intentionally knows nothing
// about HTTP or the VLM client: discovery happens first, and Rewrite is called
// only after every image has a successful VLM result.
package responses

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/zesuy/Plugin-Deepseek-Vision/internal/safety"
)

const (
	defaultMaxImages         = 256
	defaultMaxReferenceBytes = 15 * 1024 * 1024
	defaultMaxBodyBytes      = 20 * 1024 * 1024
	defaultMaxFocusChars     = 2000
	defaultMaxResultChars    = 20000
)

// Options controls request discovery and rewrite safety limits. A zero value
// uses conservative defaults. MaxFocusChars is a character (rune) limit;
// byte limits are measured in bytes of the UTF-8 request/reference.
type Options struct {
	MaxImages         int
	MaxReferenceBytes int
	MaxBodyBytes      int
	MaxFocusChars     int
	MaxResultChars    int
}

func (o Options) normalized() Options {
	if o.MaxImages <= 0 {
		o.MaxImages = defaultMaxImages
	}
	if o.MaxReferenceBytes <= 0 {
		o.MaxReferenceBytes = defaultMaxReferenceBytes
	}
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = defaultMaxBodyBytes
	}
	if o.MaxFocusChars <= 0 {
		o.MaxFocusChars = defaultMaxFocusChars
	}
	if o.MaxResultChars <= 0 {
		o.MaxResultChars = defaultMaxResultChars
	}
	return o
}

// Image describes one image discovered in a request. Number is stable and is
// one-based in original traversal order. Reference is safe to pass to the
// VLM client, while FocusHint contains nearby user text with the configured
// hard cap applied.
type Image struct {
	Number     int    `json:"number"`
	Reference  string `json:"reference"`
	FocusHint  string `json:"focus_hint"`
	InputIndex int    `json:"input_index"`
	BlockIndex int    `json:"block_index"`
	Source     string `json:"source"`

	location imageLocation
}

// PromptGroup is the unit sent to the visual model. Images that belong to the
// same Responses content/output item stay together so the model can preserve
// comparisons and relationships expressed by one prompt.
type PromptGroup struct {
	ID         int     `json:"id"`
	InputIndex int     `json:"input_index"`
	Source     string  `json:"source"`
	Prompt     string  `json:"prompt"`
	Images     []Image `json:"images"`

	locationKind locationKind
}

type imageLocation struct {
	kind      locationKind
	input     int
	block     int
	globalPos int
}

// locationKey omits globalPos, which is only used while selecting focus
// hints. Array coordinates are the immutable identity used during rewrite.
type locationKey struct {
	kind  locationKind
	input int
	block int
}

func makeLocationKey(location imageLocation) locationKey {
	return locationKey{kind: location.kind, input: location.input, block: location.block}
}

type locationKind uint8

const (
	locationContent locationKind = iota + 1
	locationFunctionOutput
)

// ImageResult is the structured output required to replace one image. Both
// fields are rendered into the frozen single-block template.
type ImageResult struct {
	VisibleText       string
	VisualDescription string
}

// Plan is an immutable discovery result. A Plan may be rewritten repeatedly;
// each call works on a fresh JSON tree and never mutates the original body.
type Plan struct {
	original   []byte
	root       any
	images     []Image
	groups     []PromptGroup
	options    Options
	inputItems int
}

// Discover parses a Responses request body and records every supported image
// in input[].content[] and function_call_output.output[]. The variadic form
// keeps the common Discover(body) call concise while allowing the interceptor
// to pass explicit limits.
func Discover(body []byte, options ...Options) (*Plan, error) {
	opt := Options{}.normalized()
	if len(options) > 0 {
		opt = options[0].normalized()
	}
	if len(body) > opt.MaxBodyBytes {
		return nil, limitExceeded(LimitRequestBody, len(body), opt.MaxBodyBytes, "request body exceeds configured limit", "body")
	}

	root, err := decodeJSON(body)
	if err != nil {
		return nil, malformed("request body is not valid JSON", "body")
	}
	object, ok := root.(map[string]any)
	if !ok {
		return nil, malformed("Responses request must be a JSON object", "body")
	}

	plan := &Plan{
		original: append([]byte(nil), body...),
		root:     root,
		options:  opt,
	}

	input, exists := object["input"]
	if !exists {
		return plan, nil
	}
	// A top-level string is a valid Responses shorthand and cannot contain an
	// image block. It is deliberately a passthrough.
	if _, ok := input.(string); ok {
		return plan, nil
	}
	items, ok := input.([]any)
	if !ok {
		return nil, malformed("input must be a string or array", "input")
	}
	plan.inputItems = len(items)

	var candidates []focusCandidate
	var pending []pendingImage
	globalPos := 0
	for inputIndex, item := range items {
		itemObject, ok := item.(map[string]any)
		if !ok {
			globalPos++
			continue
		}
		role, _ := itemObject["role"].(string)
		itemType, _ := itemObject["type"].(string)

		if rawContent, exists := itemObject["content"]; exists {
			content, ok := rawContent.([]any)
			if !ok {
				return nil, malformed("input.content must be an array", fmt.Sprintf("input[%d].content", inputIndex))
			}
			for blockIndex, block := range content {
				globalPos++
				blockObject, ok := block.(map[string]any)
				if !ok {
					return nil, malformed("input.content blocks must be objects", fmt.Sprintf("input[%d].content[%d]", inputIndex, blockIndex))
				}
				typeName, _ := blockObject["type"].(string)
				if role == "user" && typeName == "input_text" {
					if text, ok := blockObject["text"].(string); ok && strings.TrimSpace(text) != "" {
						candidates = append(candidates, focusCandidate{input: inputIndex, pos: globalPos, text: text})
					}
				}
				if typeName == "input_image" {
					image, err := discoverImage(blockObject, imageLocation{
						kind: locationContent, input: inputIndex, block: blockIndex, globalPos: globalPos,
					}, len(pending)+1, opt)
					if err != nil {
						return nil, err
					}
					pending = append(pending, image)
				}
			}
		}

		if itemType == "function_call_output" {
			if rawOutput, exists := itemObject["output"]; exists {
				// A string is the ordinary Responses function result shape and
				// cannot contain image blocks. Rich tool outputs may instead use an
				// array of input content blocks, which is where view_image results
				// are discovered and converted.
				if _, ok := rawOutput.(string); ok {
					continue
				}
				output, ok := rawOutput.([]any)
				if !ok {
					return nil, malformed("function_call_output.output must be a string or array", fmt.Sprintf("input[%d].output", inputIndex))
				}
				for blockIndex, block := range output {
					globalPos++
					blockObject, ok := block.(map[string]any)
					if !ok {
						return nil, malformed("function_call_output.output blocks must be objects", fmt.Sprintf("input[%d].output[%d]", inputIndex, blockIndex))
					}
					typeName, _ := blockObject["type"].(string)
					if typeName == "input_image" {
						image, err := discoverImage(blockObject, imageLocation{
							kind: locationFunctionOutput, input: inputIndex, block: blockIndex, globalPos: globalPos,
						}, len(pending)+1, opt)
						if err != nil {
							return nil, err
						}
						pending = append(pending, image)
					}
				}
			}
		}
	}
	for i := range pending {
		pending[i].FocusHint = chooseFocus(pending[i].location, candidates, opt.MaxFocusChars)
	}
	uniqueReferences := make(map[string]struct{}, len(pending))
	for i := range pending {
		uniqueReferences[pending[i].Reference] = struct{}{}
	}
	if len(uniqueReferences) > opt.MaxImages {
		debugImages := pendingImageValues(pending)
		return nil, imageCountExceeded(len(uniqueReferences), opt.MaxImages, summarizeImages(debugImages, len(items)), debugImages)
	}

	for i := range pending {
		plan.images = append(plan.images, pending[i].Image)
	}
	groupIndexes := make(map[locationKey]int)
	for i := range plan.images {
		image := plan.images[i]
		key := locationKey{kind: image.location.kind, input: image.InputIndex, block: -1}
		groupIndex, exists := groupIndexes[key]
		if !exists {
			groupIndex = len(plan.groups)
			groupIndexes[key] = groupIndex
			plan.groups = append(plan.groups, PromptGroup{
				ID: len(plan.groups) + 1, InputIndex: image.InputIndex,
				Source: image.Source, Prompt: groupPrompt(image, candidates, opt.MaxFocusChars),
				locationKind: image.location.kind,
			})
		}
		plan.groups[groupIndex].Images = append(plan.groups[groupIndex].Images, image)
	}
	return plan, nil
}

func groupPrompt(image Image, candidates []focusCandidate, maxChars int) string {
	var parts []string
	for i := range candidates {
		if candidates[i].input == image.InputIndex {
			parts = append(parts, strings.TrimSpace(candidates[i].text))
		}
	}
	if text := strings.TrimSpace(strings.Join(parts, "\n\n")); text != "" {
		return truncateRunes(text, maxChars)
	}
	return chooseFocus(image.location, candidates, maxChars)
}

type pendingImage struct {
	Image
}

func pendingImageValues(images []pendingImage) []Image {
	values := make([]Image, len(images))
	for i := range images {
		values[i] = images[i].Image
	}
	return values
}

func summarizeImages(images []Image, inputItems int) ImageCountDetails {
	details := ImageCountDetails{InputItems: inputItems, ImageBlocks: len(images), LastImageItemIndex: -1}
	inputIndexes := make(map[int]struct{})
	references := make(map[string]struct{})
	for i := range images {
		image := images[i]
		inputIndexes[image.InputIndex] = struct{}{}
		references[image.Reference] = struct{}{}
		if image.InputIndex > details.LastImageItemIndex {
			details.LastImageItemIndex = image.InputIndex
		}
		switch image.Source {
		case "content":
			details.ContentImages++
		case "function_call_output":
			details.FunctionOutputImages++
		}
	}
	for i := range images {
		if images[i].InputIndex == details.LastImageItemIndex {
			details.LastImageItemBlocks++
		}
	}
	details.ImageInputItems = len(inputIndexes)
	details.UniqueImageReferences = len(references)
	details.DuplicateImageBlocks = details.ImageBlocks - details.UniqueImageReferences
	details.EarlierImageBlocks = details.ImageBlocks - details.LastImageItemBlocks
	return details
}

type focusCandidate struct {
	input int
	pos   int
	text  string
}

func chooseFocus(location imageLocation, candidates []focusCandidate, maxChars int) string {
	var best *focusCandidate
	bestDistance := int(^uint(0) >> 1)
	// Prefer text in the same user input item. This makes each image in a
	// multi-image message use the nearest preceding/following instruction.
	for i := range candidates {
		candidate := &candidates[i]
		distance := abs(candidate.pos - location.globalPos)
		if candidate.input != location.input {
			continue
		}
		if distance < bestDistance || (distance == bestDistance && candidate.pos < best.pos) {
			best, bestDistance = candidate, distance
		}
	}
	if best == nil {
		for i := range candidates {
			candidate := &candidates[i]
			distance := abs(candidate.pos - location.globalPos)
			if distance < bestDistance || (distance == bestDistance && candidate.pos < best.pos) {
				best, bestDistance = candidate, distance
			}
		}
	}
	if best == nil {
		return ""
	}
	return truncateRunes(strings.TrimSpace(best.text), maxChars)
}

func discoverImage(block map[string]any, location imageLocation, number int, opt Options) (pendingImage, error) {
	raw, hasURL := block["image_url"]
	if hasURL {
		reference, ok := raw.(string)
		if !ok {
			return pendingImage{}, malformed("input_image.image_url must be a string", locationPath(location))
		}
		if err := safety.ValidateImageReference(reference, opt.MaxReferenceBytes); err != nil {
			if errors.Is(err, safety.ErrImageReferenceTooLarge) {
				return pendingImage{}, limitExceeded(LimitImageReference, len(reference), opt.MaxReferenceBytes, "image reference exceeds configured limit", locationPath(location))
			}
			// Detailed validator failures are deliberately collapsed into the
			// stable public 422 contract; never include the source reference.
			return pendingImage{}, unsupported("image reference must be a valid HTTP(S) URL or data:image URI", locationPath(location))
		}
		return pendingImage{Image: Image{
			Number: number, Reference: reference,
			InputIndex: location.input, BlockIndex: location.block, Source: locationSource(location.kind),
			location: location,
		}}, nil
	}
	if _, hasFileID := block["file_id"]; hasFileID {
		return pendingImage{}, unsupported("file_id image references are not supported", locationPath(location))
	}
	return pendingImage{}, malformed("input_image requires image_url", locationPath(location))
}

func decodeJSON(body []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	return value, nil
}

func truncateRunes(value string, max int) string {
	if max <= 0 || utf8.RuneCountInString(value) <= max {
		return value
	}
	runes := []rune(value)
	return string(runes[:max])
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func locationPath(location imageLocation) string {
	if location.kind == locationFunctionOutput {
		return fmt.Sprintf("input[%d].output[%d]", location.input, location.block)
	}
	return fmt.Sprintf("input[%d].content[%d]", location.input, location.block)
}

func locationSource(kind locationKind) string {
	if kind == locationFunctionOutput {
		return "function_call_output"
	}
	return "content"
}

func (p *Plan) InputItemCount() int {
	if p == nil {
		return 0
	}
	return p.inputItems
}

func (p *Plan) ImageCountDetails() ImageCountDetails {
	if p == nil {
		return ImageCountDetails{LastImageItemIndex: -1}
	}
	return summarizeImages(p.images, p.inputItems)
}

// Images returns a copy of the discovered image metadata. The returned slice
// and its values may be freely modified by the caller.
func (p *Plan) Images() []Image {
	if p == nil || len(p.images) == 0 {
		return nil
	}
	images := make([]Image, len(p.images))
	copy(images, p.images)
	for i := range images {
		images[i].location = imageLocation{}
	}
	return images
}

// Groups returns a deep copy of the prompt-level visual work units.
func (p *Plan) Groups() []PromptGroup {
	if p == nil || len(p.groups) == 0 {
		return nil
	}
	groups := make([]PromptGroup, len(p.groups))
	for i := range p.groups {
		groups[i] = p.groups[i]
		groups[i].locationKind = 0
		groups[i].Images = append([]Image(nil), p.groups[i].Images...)
		for j := range groups[i].Images {
			groups[i].Images[j].location = imageLocation{}
		}
	}
	return groups
}

// HasImages reports whether discovery found at least one image.
func (p *Plan) HasImages() bool { return p != nil && len(p.images) > 0 }
