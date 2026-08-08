package agent

import "encoding/json"

func jsonMarshal(v any) ([]byte, error)     { return json.Marshal(v) }
func jsonUnmarshal(raw []byte, v any) error { return json.Unmarshal(raw, v) }
