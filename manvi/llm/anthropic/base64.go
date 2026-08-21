package anthropic

import "encoding/base64"

// encodeBase64 renders image bytes for the wire's base64 source form.
func encodeBase64(data []byte) string { return base64.StdEncoding.EncodeToString(data) }
