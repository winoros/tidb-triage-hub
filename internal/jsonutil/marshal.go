package jsonutil

import (
	"bytes"
	"encoding/json"
	"strings"
)

func MarshalCompactJS(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	s := strings.TrimSuffix(buf.String(), "\n")
	s = strings.ReplaceAll(s, "</", `\u003c/`)
	s = strings.ReplaceAll(s, "<!--", `\u003c!--`)
	s = strings.ReplaceAll(s, "\u2028", `\u2028`)
	s = strings.ReplaceAll(s, "\u2029", `\u2029`)
	return s, nil
}

func MarshalPrettyJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
