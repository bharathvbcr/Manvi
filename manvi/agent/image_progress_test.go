package agent

import (
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/tools"
	"testing"
)

func TestDistinctScreenshotsAreDistinctProgress(t *testing.T) {
	a := tools.Result{Content: llm.Content{llm.ImageBlock{MediaType: "image/png", Data: []byte{1}}}}
	b := tools.Result{Content: llm.Content{llm.ImageBlock{MediaType: "image/png", Data: []byte{2}}}}
	if resultDigest("capture", a) == resultDigest("capture", b) {
		t.Fatal("different screenshots count as identical empty results")
	}
}
