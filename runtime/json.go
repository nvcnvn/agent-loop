package runtime

import "encoding/json"

// jsonUnmarshalImpl is a tiny shim so the replay package doesn't need to
// import encoding/json directly (keeps the replay file readable).
func jsonUnmarshalImpl(b []byte, v any) error { return json.Unmarshal(b, v) }
