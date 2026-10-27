package agent

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/prompt"
	"github.com/bharathvbcr/Manvi/manvi/session"
)

func grayPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func imageCount(msgs []llm.Message) (n int) {
	for _, m := range msgs {
		for _, b := range m.Content {
			if tr, ok := b.(llm.ToolResultBlock); ok {
				for _, c := range tr.Content {
					if _, ok := c.(llm.ImageBlock); ok {
						n++
					}
				}
			}
		}
	}
	return n
}

// screenshotLog is a computer-use history: n tool results, each an image and
// optionally a text caption, behind one operator prompt.
func screenshotLog(t *testing.T, n, images int, caption string) *session.Log {
	t.Helper()
	shot := grayPNG(t, 1000, 1000) // ceil(1000/28)^2 = 1296 tokens each
	log := newIdentityLog(t)
	for i := 0; i < n; i++ {
		id := llm.CallID(fmt.Sprintf("c%d", i))
		if _, err := log.Append(session.AssistantMessage, session.MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ToolCallBlock{ID: id, Name: "computer", Arguments: []byte(`{"action":"screenshot"}`)}}}}); err != nil {
			t.Fatal(err)
		}
		var content llm.Content
		if caption != "" {
			content = append(content, llm.TextBlock{Text: caption})
		}
		for j := 0; j < images; j++ {
			content = append(content, llm.ImageBlock{MediaType: "image/png", Data: shot})
		}
		if _, err := log.Append(session.ToolResult, session.ToolResultData{ToolCallID: id, Content: content}); err != nil {
			t.Fatal(err)
		}
	}
	return log
}

var screenshotBudget = Budget{ContextWindow: 16384, ReservedOutput: 2048, Overhead: 1024}

// A computer-use history is mostly screenshots. They are counted against the
// window, so they have to be reclaimable: a window full of image-only results
// used to give an empty, Insufficient plan.
func TestImageOnlyResultsAreCompacted(t *testing.T) {
	log := screenshotLog(t, 20, 1, "")
	msgs, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	plan := PlanCompactionCalibrated(msgs, "sys", nil, screenshotBudget, nil, nil)
	t.Logf("before=%d after=%d threshold=%d steps=%d insufficient=%v",
		plan.Before, plan.After, screenshotBudget.Threshold(), len(plan.Steps), plan.Insufficient)
	if plan.Before <= screenshotBudget.Threshold() {
		t.Fatalf("setup: history does not exceed the threshold")
	}
	if plan.Empty() {
		t.Fatalf("20 screenshots are %d tokens over a %d threshold and nothing can be reclaimed (Insufficient=%v)",
			plan.Before, screenshotBudget.Threshold(), plan.Insufficient)
	}
	if plan.Insufficient {
		t.Errorf("compacting the eligible screenshots should be enough: after=%d threshold=%d", plan.After, screenshotBudget.Threshold())
	}
	if plan.After >= plan.Before-1296 {
		t.Errorf("the plan reports no image saving: before=%d after=%d", plan.Before, plan.After)
	}
}

// A compacted result that held images says so. The model must not be left to
// infer from a text-only result that a screenshot it once saw was never there.
func TestCompactionNamesTheImagesItRemoved(t *testing.T) {
	for _, tc := range []struct {
		name    string
		images  int
		caption string
		want    string
	}{
		{"image only", 1, "", fmt.Sprintf(ImageRemovalMarker, 1)},
		{"image with caption", 1, "screenshot taken", fmt.Sprintf(ImageRemovalMarker, 1)},
		{"two images", 2, "", fmt.Sprintf(ImageRemovalMarker, 2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := screenshotLog(t, 12, tc.images, tc.caption)
			plan := compactOnce(t, log, screenshotBudget)
			if plan.Empty() {
				t.Fatal("no plan")
			}
			msgs, err := log.DeriveMessages()
			if err != nil {
				t.Fatal(err)
			}
			compacted := map[llm.CallID]bool{}
			for _, s := range plan.Steps {
				compacted[s.ToolCallID] = true
			}
			kept := 0
			for _, m := range msgs {
				for _, b := range m.Content {
					tr, ok := b.(llm.ToolResultBlock)
					if !ok {
						continue
					}
					var text strings.Builder
					images := 0
					for _, c := range tr.Content {
						switch v := c.(type) {
						case llm.TextBlock:
							text.WriteString(v.Text)
						case llm.ImageBlock:
							images++
						}
					}
					if !compacted[tr.ToolCallID] {
						kept += images
						continue
					}
					if images != 0 {
						t.Errorf("%s: compacted result still carries %d image(s)", tr.ToolCallID, images)
					}
					if !strings.Contains(text.String(), tc.want) {
						t.Errorf("%s: compacted result does not name what it dropped (%q):\n%s", tr.ToolCallID, tc.want, text.String())
					}
					if tc.caption != "" && !strings.Contains(text.String(), tc.caption) {
						t.Errorf("%s: the caption was lost:\n%s", tr.ToolCallID, text.String())
					}
				}
			}
			if kept != imageCount(msgs) || kept == 0 {
				t.Errorf("results outside the plan must keep their images: kept=%d total=%d", kept, imageCount(msgs))
			}
		})
	}
}

// The notice is the whole of what the model is told, so it names the count as
// the harness writes it, and a result that held text keeps that text, cut as
// any text result is, ahead of the notice.
func TestImageNoticeFollowsTheTextOfAMixedResult(t *testing.T) {
	shot := grayPNG(t, 1000, 1000)
	log := newIdentityLog(t)
	for i := 0; i < 12; i++ {
		id := llm.CallID(fmt.Sprintf("c%d", i))
		if _, err := log.Append(session.AssistantMessage, session.MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ToolCallBlock{ID: id, Name: "computer", Arguments: []byte(`{}`)}}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := log.Append(session.ToolResult, session.ToolResultData{ToolCallID: id, Content: llm.Content{
			llm.TextBlock{Text: bigOutput(200, "node")},
			llm.ImageBlock{MediaType: "image/png", Data: shot},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	plan := compactOnce(t, log, Budget{ContextWindow: 40000, ReservedOutput: 2048, Overhead: 1024})
	if plan.Empty() {
		t.Fatal("no plan")
	}
	want := fmt.Sprintf(ImageRemovalMarker, 1)
	for _, step := range plan.Steps {
		if !strings.HasSuffix(step.Text, "\n"+want) {
			t.Errorf("%s: the notice does not close the compacted text:\n%s", step.ToolCallID, step.Text)
		}
		if !strings.Contains(step.Text, "node line 000") || !strings.Contains(step.Text, "omitted to fit") {
			t.Errorf("%s: the text part was not compacted as a text result is:\n%s", step.ToolCallID, step.Text)
		}
		if step.FromBytes < len(shot) {
			t.Errorf("%s: FromBytes %d does not count the image (%d bytes)", step.ToolCallID, step.FromBytes, len(shot))
		}
	}
}

// A result without images is shortened to exactly the text the text compactor
// gives at one of its floors: no notice, no added byte, so the prompt prefix a
// local server caches is what it was before images were counted.
func TestTextOnlyResultsAreCompactedToExactlyTheFloorText(t *testing.T) {
	msgs := history(24, 40)
	original := map[llm.CallID]string{}
	for _, m := range msgs {
		for _, b := range m.Content {
			if tr, ok := b.(llm.ToolResultBlock); ok {
				original[tr.ToolCallID] = firstText(t, tr)
			}
		}
	}
	plan := PlanCompactionCalibrated(msgs, "sys", nil, testBudget(8192), nil, nil)
	if plan.Empty() {
		t.Fatal("no plan")
	}
	for _, step := range plan.Steps {
		matched := false
		for _, floor := range compactFloors {
			if step.Text == CompactToolResultText(original[step.ToolCallID], floor) {
				matched = true
			}
		}
		if !matched {
			t.Errorf("%s: step text is not the compactor's output at any floor:\n%s", step.ToolCallID, step.Text)
		}
		if step.FromBytes != len(original[step.ToolCallID]) || step.ToBytes != len(step.Text) {
			t.Errorf("%s: sizes %d/%d, want %d/%d", step.ToolCallID, step.FromBytes, step.ToBytes, len(original[step.ToolCallID]), len(step.Text))
		}
	}
}

// An image smaller than its own notice is not worth removing: replacing it
// would spend context to save none.
func TestAnImageSmallerThanItsNoticeIsLeftAlone(t *testing.T) {
	tiny := grayPNG(t, 28, 28) // one patch: one token
	log := newIdentityLog(t)
	for i := 0; i < 12; i++ {
		id := llm.CallID(fmt.Sprintf("c%d", i))
		if _, err := log.Append(session.AssistantMessage, session.MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ToolCallBlock{ID: id, Name: "computer", Arguments: []byte(`{}`)}}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := log.Append(session.ToolResult, session.ToolResultData{ToolCallID: id, Content: llm.Content{
			llm.ImageBlock{MediaType: "image/png", Data: tiny}}}); err != nil {
			t.Fatal(err)
		}
	}
	msgs, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	// A budget the history just exceeds, so the planner runs.
	budget := Budget{ContextWindow: 4096, ReservedOutput: 0, Overhead: 0}
	plan := PlanCompactionCalibrated(msgs, strings.Repeat("system ", 4200), nil, budget, nil, nil)
	if plan.Before <= budget.Threshold() {
		t.Fatalf("setup: %d tokens does not exceed %d", plan.Before, budget.Threshold())
	}
	if !plan.Empty() {
		t.Fatalf("a one-token image was replaced by a longer notice: %d step(s)", len(plan.Steps))
	}
}

// Images are priced by the estimate the calibrator corrects, so the saving the
// plan reports scales with the observed ratio as the text's does.
func TestImageSavingIsCalibratedLikeText(t *testing.T) {
	log := screenshotLog(t, 20, 1, "")
	msgs, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	var cal Calibrator
	raw := CountRequestTokens("sys", nil, msgs)
	cal.Observe(raw, raw*3/4)
	plan := PlanCompactionCalibrated(msgs, "sys", nil, screenshotBudget, nil, &cal)
	if plan.Empty() {
		t.Fatal("no plan")
	}
	perStep := float64(plan.Before-plan.After) / float64(len(plan.Steps))
	// One 1000x1000 screenshot is 1296 estimated tokens; removed, it costs the
	// notice. Both are scaled by the ratio the calibrator holds.
	notice := float64(prompt.EstimateTokens(fmt.Sprintf(ImageRemovalMarker, 1)))
	want := (1296 - notice) * cal.Ratio()
	if perStep < want-2 || perStep > want+2 {
		t.Fatalf("saving per step %.1f, want about %.1f at ratio %.2f", perStep, want, cal.Ratio())
	}
}
