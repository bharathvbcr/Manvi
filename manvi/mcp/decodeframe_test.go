package mcp

import (
	"encoding/json"
	"reflect"
	"testing"
)

// referenceDecode is the two-decode classification decodeFrame replaced.
func referenceDecode(line []byte) (Response, string, bool, error) {
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return Response{}, "", false, err
	}
	var envelope struct {
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return Response{}, "", false, errUnreadableFrame
	}
	return resp, envelope.Method, len(envelope.Result) > 0 || len(envelope.Error) > 0, nil
}

// One decode instead of two must classify every frame the way two did,
// including the error text a violation is reported with.
func TestDecodeFrameMatchesTheTwoDecodeClassification(t *testing.T) {
	frames := []string{
		`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"hi"}]}}`,
		`{"jsonrpc":"2.0","id":2,"error":{"code":-32601,"message":"nope"}}`,
		`{"jsonrpc":"2.0","method":"notifications/progress","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"sampling/createMessage"}`,
		`{"jsonrpc":"2.0","id":4,"result":null}`,
		`{"jsonrpc":"2.0","id":5,"error":null}`,
		`{"id":6,"error":null}`,
		`{"id":7,"result":null}`,
		`{"id":8,"error":{}}`,
		`{"id":9}`,
		`{"level":"info","msg":"structured log"}`,
		`{"jsonrpc":""}`,
		`{"id":10,"method":5}`,
		`{"id":11,"error":"x"}`,
		`{"jsonrpc":5}`,
		`{"_meta":3}`,
		`{"id":"abc","result":{}}`,
		`{"id":12,"result":{},"_meta":{"k":"v"}}`,
		`[1,2]`,
		`not json`,
		`{"id":13,"result":`,
	}
	for _, f := range frames {
		wantResp, wantMethod, wantPayload, wantErr := referenceDecode([]byte(f))
		gotResp, gotMethod, gotPayload, gotErr := decodeFrame([]byte(f))
		if (wantErr == nil) != (gotErr == nil) || (wantErr != nil && wantErr.Error() != gotErr.Error()) {
			t.Errorf("%s: error %v, want %v", f, gotErr, wantErr)
			continue
		}
		if !reflect.DeepEqual(gotResp, wantResp) || gotMethod != wantMethod || gotPayload != wantPayload {
			t.Errorf("%s: got (%+v, %q, %v), want (%+v, %q, %v)",
				f, gotResp, gotMethod, gotPayload, wantResp, wantMethod, wantPayload)
		}
	}
}
