package gemini

import (
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
	"strings"
	"testing"
)

// The usage object of an interaction.completed event as the Interactions API
// reference documents it (https://aistudio.google.com/api-reference/interactions-api,
// read 2026-11-09): the cached field is total_cached_tokens. This pins the name
// over a documented response shape; the live wire has not been checked.
func streamWithUsage(usage string) string {
	return strings.Replace(happyStream, `"usage":{"total_input_tokens":19,"total_output_tokens":41,"total_thought_tokens":8}`, `"usage":`+usage, 1)
}

func TestCachedInputTokensAreReadFromTheDocumentedField(t *testing.T) {
	cases := map[string]struct {
		usage string
		want  int
	}{
		"documented name":       {`{"total_input_tokens":19,"total_cached_tokens":12,"total_output_tokens":41,"total_thought_tokens":8}`, 12},
		"legacy name":           {`{"total_input_tokens":19,"cached_content_token_count":7,"total_output_tokens":41,"total_thought_tokens":8}`, 7},
		"both: documented wins": {`{"total_input_tokens":19,"total_cached_tokens":12,"cached_content_token_count":7,"total_output_tokens":41}`, 12},
		"neither":               {`{"total_input_tokens":19,"total_output_tokens":41}`, 0},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			adapter, _ := adapterFor(t, streamWithUsage(c.usage))
			stream, err := adapter.Stream(adaptertest.Ctx(), request())
			if err != nil {
				t.Fatal(err)
			}
			_, resp, err := adaptertest.Drain(stream)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Usage.CacheReadTokens != c.want || resp.Usage.InputTokens != 19 {
				t.Fatalf("%s: usage = %+v, want cache read %d", name, resp.Usage, c.want)
			}
		})
	}
}
