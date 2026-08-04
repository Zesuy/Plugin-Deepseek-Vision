package responses

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverAndRewriteContentImage(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-flash","input":[{"role":"user","content":[{"type":"input_text","text":"Describe this screenshot."},{"type":"input_image","image_url":"data:image/png;base64,AAECAw=="}]}]}`)
	plan, err := Discover(body)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.HasImages() || len(plan.Images()) != 1 {
		t.Fatalf("expected one image, got %#v", plan.Images())
	}
	if got := plan.Images()[0].FocusHint; got != "Describe this screenshot." {
		t.Fatalf("focus hint = %q", got)
	}
	rewritten, err := plan.Rewrite([]ImageResult{{VisibleText: "Hello", VisualDescription: "A terminal window."}})
	if err != nil {
		t.Fatal(err)
	}
	text := string(rewritten)
	if strings.Contains(text, "input_image") || strings.Contains(text, "data:image/png") {
		t.Fatalf("rewritten body retained image: %s", text)
	}
	if !strings.Contains(text, "[Image 1 — Visual analysis]") || !strings.Contains(text, "Visible text:\\nHello") {
		t.Fatalf("replacement template missing: %s", text)
	}
	second, err := Discover(rewritten)
	if err != nil {
		t.Fatal(err)
	}
	if second.HasImages() {
		t.Fatal("rewritten body is not idempotent")
	}
	if got, err := second.Rewrite(nil); err != nil || string(got) != text {
		t.Fatalf("idempotent rewrite changed body: %q, %v", got, err)
	}
}

func TestPromptGroupBatchesImagesAndRewritesOnce(t *testing.T) {
	body := []byte(`{"input":[{"role":"user","content":[{"type":"input_text","text":"compare"},{"type":"input_image","image_url":"https://example.com/one.png"},{"type":"input_text","text":"focus on differences"},{"type":"input_image","image_url":"https://example.com/two.png"}]}]}`)
	plan, err := Discover(body)
	if err != nil {
		t.Fatal(err)
	}
	groups := plan.Groups()
	if len(groups) != 1 || len(groups[0].Images) != 2 || groups[0].Prompt != "compare\n\nfocus on differences" {
		t.Fatalf("groups = %#v", groups)
	}
	rewritten, err := plan.RewriteGroupsText([]string{"Image 1 is a form. Image 2 is the completed form. The second follows the first."})
	if err != nil {
		t.Fatal(err)
	}
	text := string(rewritten)
	if strings.Contains(text, "input_image") || strings.Contains(text, "example.com/") {
		t.Fatalf("group rewrite retained an image: %s", text)
	}
	if strings.Count(text, "Joint visual analysis") != 1 || !strings.Contains(text, "[Image 1 — already analyzed") || !strings.Contains(text, "[Image 2 — already analyzed") || !strings.Contains(text, "target model cannot inspect image attachments directly") {
		t.Fatalf("group analysis was not inserted exactly once: %s", text)
	}
}

func TestGroupedRewriteRemovesCodexAttachmentPathsAndDiscouragesReopen(t *testing.T) {
	body := []byte(`{"input":[{"role":"user","content":[` +
		`{"type":"input_text","text":"# Files mentioned by the user:\n\n## shot.png: C:/Users/demo/AppData/Local/Temp/shot.png\n## My request:\nDescribe it"},` +
		`{"type":"input_text","text":"<image name=[Image #1] path=\"C:\\Users\\demo\\AppData\\Local\\Temp\\shot.png\">"},` +
		`{"type":"input_image","image_url":"data:image/png;base64,AAAA"},` +
		`{"type":"input_text","text":"</image>"}` +
		`]},` +
		`{"role":"assistant","content":[{"type":"output_text","text":"Historical note: C:/Users/demo/AppData/Local/Temp/shot.png"}]},` +
		`{"type":"function_call","name":"view_image","arguments":"{\"path\":\"C:\\\\Users\\\\demo\\\\AppData\\\\Local\\\\Temp\\\\shot.png\"}"}` +
		`]}`)
	plan, err := Discover(body)
	if err != nil {
		t.Fatal(err)
	}
	rewritten, err := plan.RewriteGroupsText([]string{"A settings dialog."})
	if err != nil {
		t.Fatal(err)
	}
	text := string(rewritten)
	for _, forbidden := range []string{"C:/Users/", `C:\\Users\\`, `path=\"`, "input_image"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("rewritten attachment retained %q: %s", forbidden, text)
		}
	}
	for _, required := range []string{"Describe it", "Historical note", "analyzed image attachment path omitted", "target model cannot inspect image attachments directly", "do not call view_image"} {
		if !strings.Contains(text, required) {
			t.Fatalf("rewritten attachment missing %q: %s", required, text)
		}
	}
}

func TestFunctionOutputImagesFormTheirOwnPromptGroup(t *testing.T) {
	body := []byte(`{"input":[{"role":"user","content":[{"type":"input_text","text":"inspect tool screenshots"}]},{"type":"function_call_output","output":[{"type":"output_text","text":"tool result"},{"type":"input_image","image_url":"https://example.com/tool-1.png"},{"type":"input_image","image_url":"https://example.com/tool-2.png"}]}]}`)
	plan, err := Discover(body)
	if err != nil {
		t.Fatal(err)
	}
	groups := plan.Groups()
	if len(groups) != 1 || groups[0].Source != "function_call_output" || len(groups[0].Images) != 2 || groups[0].Prompt != "inspect tool screenshots" {
		t.Fatalf("groups = %#v", groups)
	}
	rewritten, err := plan.RewriteGroupsText([]string{"Image 1 shows the start. Image 2 shows the result."})
	if err != nil {
		t.Fatal(err)
	}
	text := string(rewritten)
	if strings.Contains(text, "input_image") || strings.Count(text, "Joint visual analysis") != 1 || !strings.Contains(text, "tool result") {
		t.Fatalf("function output group rewrite = %s", text)
	}
}

func TestDiscoverFixtures(t *testing.T) {
	fixtures := []struct {
		name   string
		images int
	}{
		{"01-content-input-image.json", 1},
		{"02-function-call-output.json", 1},
		{"03-multi-image-mixed.json", 2},
		{"04-no-image.json", 0},
		{"05-stream.json", 1},
		{"06-compact.json", 1},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "responses", fixture.name))
			if err != nil {
				t.Fatal(err)
			}
			plan, err := Discover(body)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(plan.Images()); got != fixture.images {
				t.Fatalf("images = %d, want %d", got, fixture.images)
			}
		})
	}
}

func TestFunctionCallOutputOrderAndUnknownFields(t *testing.T) {
	body := []byte(`{"input":[{"role":"user","content":[{"type":"input_text","text":"first","non_image_extra":{"preserve":true}},{"type":"input_image","image_url":"https://example.com/one.png","image_only_extra":{"drop":true}},{"type":"input_text","text":"second"}]},{"type":"function_call_output","output":[{"type":"input_image","image_url":"https://example.com/two.png","image_only_extra":"drop"},{"type":"output_text","text":"last","non_image_extra":"preserve"}],"tool_meta":{"x":1}}],"unknown":{"nested":[1,true,"v"]}}`)
	plan, err := Discover(body)
	if err != nil {
		t.Fatal(err)
	}
	images := plan.Images()
	if len(images) != 2 || images[0].Number != 1 || images[1].Number != 2 {
		t.Fatalf("unexpected images: %#v", images)
	}
	if images[0].FocusHint != "first" {
		t.Errorf("first focus = %q", images[0].FocusHint)
	}
	if images[1].FocusHint != "second" {
		t.Errorf("fallback focus = %q", images[1].FocusHint)
	}
	rewritten, err := plan.Rewrite([]ImageResult{{VisibleText: "one", VisualDescription: "ONE"}, {VisibleText: "two", VisualDescription: "TWO"}})
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(rewritten, &object); err != nil {
		t.Fatal(err)
	}
	if _, ok := object["unknown"]; !ok {
		t.Fatal("unknown root field was dropped")
	}
	items := object["input"].([]any)
	content := items[0].(map[string]any)["content"].([]any)
	if _, ok := content[0].(map[string]any)["non_image_extra"]; !ok {
		t.Fatal("unknown field on non-image content block was dropped")
	}
	if _, ok := content[1].(map[string]any)["image_only_extra"]; ok {
		t.Fatal("field belonging to replaced input_image survived replacement")
	}
	output := items[1].(map[string]any)["output"].([]any)
	if _, ok := output[0].(map[string]any)["image_only_extra"]; ok {
		t.Fatal("field belonging to replaced function output image survived replacement")
	}
	if _, ok := output[1].(map[string]any)["non_image_extra"]; !ok {
		t.Fatal("unknown field on non-image function output block was dropped")
	}
	encoded := string(rewritten)
	if strings.Contains(encoded, "example.com/one.png") || strings.Contains(encoded, "example.com/two.png") {
		t.Fatal("original image reference survived rewrite")
	}
	if !strings.Contains(encoded, "tool_meta") || !strings.Contains(encoded, "output_text") {
		t.Fatal("function output unknown fields/order not preserved")
	}
}

func TestTopLevelStringAndNoInputPassthrough(t *testing.T) {
	for _, body := range []string{`{"input":"plain text"}`, `{"model":"x"}`} {
		plan, err := Discover([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if plan.HasImages() {
			t.Fatal("unexpected image")
		}
		out, err := plan.Rewrite(nil)
		if err != nil || string(out) != body {
			t.Fatalf("passthrough = %q, err=%v", out, err)
		}
	}
}

func TestStringFunctionCallOutputIsValidAndPreserved(t *testing.T) {
	body := `{"input":[{"type":"function_call_output","call_id":"call_1","output":"unable to locate image"}]}`
	plan, err := Discover([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if plan.HasImages() {
		t.Fatal("string tool output unexpectedly produced an image")
	}
	out, err := plan.RewriteGroupsText(nil)
	if err != nil || string(out) != body {
		t.Fatalf("string tool output changed: %s, err=%v", out, err)
	}
}

func TestTypedErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
		kind ErrorKind
		code int
	}{
		{"malformed", `{"input":[`, ErrorMalformedRequest, 400},
		{"content object", `{"input":[{"content":{"type":"input_image","image_url":"https://example.com/a.png"}}]}`, ErrorMalformedRequest, 400},
		{"output object", `{"input":[{"type":"function_call_output","output":{"type":"input_image","image_url":"https://example.com/a.png"}}]}`, ErrorMalformedRequest, 400},
		{"file id", `{"input":[{"content":[{"type":"input_image","file_id":"file-1"}]}]}`, ErrorUnsupportedImage, 422},
		{"empty image URL", `{"input":[{"content":[{"type":"input_image","image_url":"  "}]}]}`, ErrorUnsupportedImage, 422},
		{"scheme", `{"input":[{"content":[{"type":"input_image","image_url":"ftp://example.com/x"}]}]}`, ErrorUnsupportedImage, 422},
		{"non-image data", `{"input":[{"content":[{"type":"input_image","image_url":"data:text/plain;base64,SGVsbG8="}]}]}`, ErrorUnsupportedImage, 422},
		{"URL credentials", `{"input":[{"content":[{"type":"input_image","image_url":"https://user:secret@example.com/x.png"}]}]}`, ErrorUnsupportedImage, 422},
		{"malformed URI", `{"input":[{"content":[{"type":"input_image","image_url":"https://example.com/%zz.png"}]}]}`, ErrorUnsupportedImage, 422},
		{"malformed data percent", `{"input":[{"content":[{"type":"input_image","image_url":"data:image/png,%zz"}]}]}`, ErrorUnsupportedImage, 422},
		{"malformed data base64", `{"input":[{"content":[{"type":"input_image","image_url":"data:image/png;base64,***"}]}]}`, ErrorUnsupportedImage, 422},
		{"reference too large", `{"input":[{"content":[{"type":"input_image","image_url":"https://example.com/large.png"}]}]}`, ErrorLimitsExceeded, 413},
		{"too many", `{"input":[{"content":[{"type":"input_image","image_url":"https://e/x"},{"type":"input_image","image_url":"https://e/y"}]}]}`, ErrorLimitsExceeded, 413},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := Options{}
			if test.name == "too many" {
				options.MaxImages = 1
			}
			if test.name == "reference too large" {
				options.MaxReferenceBytes = 8
			}
			_, err := Discover([]byte(test.body), options)
			if err == nil {
				t.Fatal("expected error")
			}
			var plannerErr *Error
			if !errors.As(err, &plannerErr) {
				t.Fatalf("error type %T", err)
			}
			if plannerErr.Kind != test.kind || plannerErr.StatusCode != test.code {
				t.Fatalf("error = %#v", plannerErr)
			}
			if strings.Contains(err.Error(), "user:secret") {
				t.Fatalf("error leaked source credentials: %v", err)
			}
		})
	}
}

func TestLimitErrorsExposeSafeMeasurements(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		options Options
		limit   LimitKind
		actual  int
		maximum int
	}{
		{"request body", `{"input":"0123456789"}`, Options{MaxBodyBytes: 8}, LimitRequestBody, len(`{"input":"0123456789"}`), 8},
		{"image reference", `{"input":[{"content":[{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`, Options{MaxReferenceBytes: 8}, LimitImageReference, len("https://example.com/a.png"), 8},
		{"image count", `{"input":[{"content":[{"type":"input_image","image_url":"https://e/x"},{"type":"input_image","image_url":"https://e/y"}]}]}`, Options{MaxImages: 1}, LimitImageCount, 2, 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Discover([]byte(test.body), test.options)
			var plannerErr *Error
			if !errors.As(err, &plannerErr) {
				t.Fatalf("error = %T, want *Error", err)
			}
			if plannerErr.Limit != test.limit || plannerErr.Actual != test.actual || plannerErr.Maximum != test.maximum {
				t.Fatalf("limit error = %#v", plannerErr)
			}
		})
	}
}

func TestImageCountErrorBreaksDownMultiTurnAndDuplicates(t *testing.T) {
	body := []byte(`{"input":[` +
		`{"role":"user","content":[{"type":"input_image","image_url":"https://e/repeated"},{"type":"input_image","image_url":"https://e/history"}]},` +
		`{"role":"user","content":[{"type":"input_image","image_url":"https://e/repeated"},{"type":"input_image","image_url":"https://e/current-a"},{"type":"input_image","image_url":"https://e/current-b"}]}` +
		`]}`)
	_, err := Discover(body, Options{MaxImages: 3})
	var plannerErr *Error
	if !errors.As(err, &plannerErr) || plannerErr.ImageCount == nil || plannerErr.Actual != 4 {
		t.Fatalf("error = %#v", err)
	}
	details := plannerErr.ImageCount
	if details.ImageBlocks != 5 || details.UniqueImageReferences != 4 || details.DuplicateImageBlocks != 1 ||
		details.ImageInputItems != 2 || details.LastImageItemIndex != 1 || details.LastImageItemBlocks != 3 || details.EarlierImageBlocks != 2 ||
		details.ContentImages != 5 || details.FunctionOutputImages != 0 {
		t.Fatalf("image count details = %#v", details)
	}
}

func TestDiscoverAcceptsDataImageCaseAndParameters(t *testing.T) {
	body := []byte(`{"input":[{"content":[{"type":"input_image","image_url":"DATA:IMAGE/PNG;charset=utf-8;BASE64,QUJDRA=="}]}]}`)
	plan, err := Discover(body)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(plan.Images()); got != 1 {
		t.Fatalf("images = %d", got)
	}
}

func TestRewriteRedactsEncodedAndWrappedReferences(t *testing.T) {
	reference := "data:image/png;base64,QUJDREVGR0hJSktMTU5PUA=="
	body := []byte(`{"input":[{"content":[{"type":"input_image","image_url":"` + reference + `"}]}]}`)
	plan, err := Discover(body)
	if err != nil {
		t.Fatal(err)
	}
	result := ImageResult{
		VisibleText: strings.Join([]string{
			"Normal text stays.",
			reference,
			`data:image\/png;base64,QUJDREVGR0hJSktMTU5PUA==`,
			"data%3Aimage/png;base64,QUJDREVGR0hJSktMTU5PUA==",
			`data%3Aimage\/png;base64,QUJDREVGR0hJSktMTU5PUA==`,
			"data%3A%69mage%2Fpng;base64,QUJDREVGR0hJSktMTU5PUA==",
			"d%61ta%3A%69mage%2Fpng;base64,QUJDREVGR0hJSktMTU5PUA==",
			"d%2561ta%253A%2569mage%252Fpng;base64,QUJDREVGR0hJSktMTU5PUA==",
			"https%3A%2F%2Fuser%3Asecret%40example.com/private.png",
			"data%3Aimage%2Fpng%3Bbase64%2CQUJDREVGR0hJSktMTU5PUA%3D%3D",
			"data:image/png;base64,QUJDREVGR0hJ\nSktMTU5PUA==",
		}, "\n"),
		VisualDescription: "Keep this sentence; remove https://user:secret@example.com/private.png but preserve https://docs.example.com/guide and https%3A%2F%2Fdocs.example.com%2Fencoded.",
	}
	rewritten, err := plan.Rewrite([]ImageResult{result})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rewritten), reference) {
		t.Fatal("raw input reference survived encoded JSON body")
	}
	text := rewrittenReplacementText(t, rewritten)
	for _, forbidden := range []string{
		"data:image", `data:image\/`, "data%3Aimage", "data%3aimage",
		"data%3A%69mage", "d%61ta%3A%69mage", "d%2561ta%253A%2569mage", "user:secret", "user%3Asecret%40example.com",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("sensitive token %q survived in %q", forbidden, text)
		}
	}
	if !strings.Contains(text, "Normal text stays.") || !strings.Contains(text, "Keep this sentence") ||
		!strings.Contains(text, "https://docs.example.com/guide") ||
		!strings.Contains(text, "https%3A%2F%2Fdocs.example.com%2Fencoded") {
		t.Fatalf("normal text was not retained: %q", text)
	}
	if count := strings.Count(text, omittedImageReference); count < 10 {
		t.Fatalf("expected redaction markers, got %d in %q", count, text)
	}
}

func TestRewriteRedactsSourceURLButPreservesDistinctDocumentURL(t *testing.T) {
	reference := "https://images.example.com/source.png?x=1&y=2"
	body := []byte(`{"input":[{"content":[{"type":"input_image","image_url":"` + reference + `"}]}]}`)
	plan, err := Discover(body)
	if err != nil {
		t.Fatal(err)
	}
	result := ImageResult{
		VisibleText: strings.Join([]string{
			"raw " + reference,
			`JSON https:\/\/images.example.com\/source.png?x=1&y=2`,
			"encoded https%3A%2F%2Fimages.example.com%2Fsource.png%3Fx%3D1%26y%3D2",
			"wrapped https://images.example.com/\nsource.png?x=1&y=2",
		}, "\n"),
		VisualDescription: "Documentation: https://docs.example.com/vision?chapter=2",
	}
	rewritten, err := plan.Rewrite([]ImageResult{result})
	if err != nil {
		t.Fatal(err)
	}
	text := rewrittenReplacementText(t, rewritten)
	for _, sourceVariant := range []string{
		reference,
		`https:\/\/images.example.com\/source.png?x=1&y=2`,
		"https%3A%2F%2Fimages.example.com%2Fsource.png%3Fx%3D1%26y%3D2",
		"https://images.example.com/\nsource.png?x=1&y=2",
	} {
		if strings.Contains(text, sourceVariant) {
			t.Fatalf("source variant %q survived in %q", sourceVariant, text)
		}
	}
	if !strings.Contains(text, "https://docs.example.com/vision?chapter=2") {
		t.Fatalf("distinct document URL was removed: %q", text)
	}
	if strings.Contains(string(rewritten), reference) {
		t.Fatalf("raw source reference survived encoded body: %s", rewritten)
	}
}

func TestRewriteRedactsPercentEscapeCaseVariant(t *testing.T) {
	reference := "https://images.example.com/x%2fsecret"
	echoed := "https://images.example.com/x%2Fsecret"
	distinct := "https://docs.example.com/x%2Fsecret"
	body := []byte(`{"input":[{"content":[{"type":"input_image","image_url":"` + reference + `"}]}]}`)
	plan, err := Discover(body)
	if err != nil {
		t.Fatal(err)
	}
	rewritten, err := plan.Rewrite([]ImageResult{{
		VisibleText:       "echoed source: " + echoed,
		VisualDescription: "Unrelated documentation remains at " + distinct,
	}})
	if err != nil {
		t.Fatal(err)
	}
	text := rewrittenReplacementText(t, rewritten)
	if strings.Contains(text, echoed) {
		t.Fatalf("percent-escape case variant survived: %q", text)
	}
	if !strings.Contains(text, distinct) {
		t.Fatalf("unrelated URL was removed: %q", text)
	}
}

func TestRewriteRedactsCanonicalLowercaseSignedSourceURL(t *testing.T) {
	reference := "HTTPS://IMAGES.EXAMPLE.COM/Signed/Asset.PNG?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=AbCdEf012345#Preview"
	echoed := "https://images.example.com/Signed/Asset.PNG?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=AbCdEf012345#Preview"
	distinct := "https://docs.example.com/Signed/Asset.PNG?X-Amz-Signature=PublicExample"
	body := []byte(`{"input":[{"content":[{"type":"input_image","image_url":"` + reference + `"}]}]}`)
	plan, err := Discover(body)
	if err != nil {
		t.Fatal(err)
	}
	rewritten, err := plan.Rewrite([]ImageResult{{
		VisibleText: strings.Join([]string{
			"normalized " + echoed,
			`escaped https:\/\/images.example.com\/Signed\/Asset.PNG?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=AbCdEf012345#Preview`,
			"wrapped https://images.example.com/Signed/\nAsset.PNG?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=AbCdEf012345#Preview",
		}, "\n"),
		VisualDescription: "Distinct documentation remains at " + distinct,
	}})
	if err != nil {
		t.Fatal(err)
	}
	text := rewrittenReplacementText(t, rewritten)
	for _, forbidden := range []string{
		echoed,
		`https:\/\/images.example.com\/Signed\/Asset.PNG?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=AbCdEf012345#Preview`,
		"https://images.example.com/Signed/\nAsset.PNG?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=AbCdEf012345#Preview",
		"AbCdEf012345",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("canonical source material %q survived in %q", forbidden, text)
		}
	}
	if !strings.Contains(text, distinct) {
		t.Fatalf("distinct document URL was removed: %q", text)
	}
}

func TestRewriteRedactsForeignDataImageButRetainsNormalText(t *testing.T) {
	body := []byte(`{"input":[{"content":[{"type":"input_image","image_url":"https://example.com/source.png"}]}]}`)
	plan, err := Discover(body)
	if err != nil {
		t.Fatal(err)
	}
	rewritten, err := plan.Rewrite([]ImageResult{{
		VisibleText:       "ordinary words and image/png remain",
		VisualDescription: "foreign data:image/jpeg;base64,QUJDREVGRw== end",
	}})
	if err != nil {
		t.Fatal(err)
	}
	text := rewrittenReplacementText(t, rewritten)
	if strings.Contains(strings.ToLower(text), "data:image") {
		t.Fatalf("foreign data image survived: %q", text)
	}
	if !strings.Contains(text, "ordinary words and image/png remain") || !strings.Contains(text, " end") {
		t.Fatalf("normal text was damaged: %q", text)
	}
}

func rewrittenReplacementText(t *testing.T, body []byte) string {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatal(err)
	}
	items := root["input"].([]any)
	content := items[0].(map[string]any)["content"].([]any)
	return content[0].(map[string]any)["text"].(string)
}

func TestFocusLimitAndResultValidation(t *testing.T) {
	body := []byte(`{"input":[{"role":"user","content":[{"type":"input_text","text":"0123456789"},{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`)
	plan, err := Discover(body, Options{MaxFocusChars: 4})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Images()[0].FocusHint; got != "0123" {
		t.Fatalf("focus = %q", got)
	}
	if _, err := plan.Rewrite([]ImageResult{{}}); err == nil {
		t.Fatal("empty result must fail")
	}
	if _, err := plan.Rewrite(nil); err == nil {
		t.Fatal("missing result must fail")
	}
}

func TestParseVLMText(t *testing.T) {
	result := ParseVLMText("Visible text:\nabc\n\nVisual description:\nA graph.")
	if result.VisibleText != "abc" || result.VisualDescription != "A graph." {
		t.Fatalf("parsed result = %#v", result)
	}
	result = ParseVLMText("A plain description")
	if result.VisualDescription != "A plain description" || result.VisibleText == "" {
		t.Fatalf("plain result = %#v", result)
	}
}

func FuzzDiscoverNeverPanics(f *testing.F) {
	for _, seed := range []string{
		`{}`, `{"input":[]}`, `{"input":"text"}`, `{"input":[{"content":[{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}]}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		plan, err := Discover(body)
		if err != nil || !plan.HasImages() {
			return
		}
		results := make([]ImageResult, len(plan.Images()))
		for i := range results {
			results[i] = ImageResult{VisualDescription: "ok"}
		}
		_, _ = plan.Rewrite(results)
	})
}
