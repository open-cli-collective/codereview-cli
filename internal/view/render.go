package view

import (
	"encoding/json"
	"io"
)

// RenderJSON writes indented JSON followed by a newline.
func RenderJSON(w io.Writer, v any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(v)
}

// Render writes v as JSON when asJSON is true, otherwise it calls text.
func Render(w io.Writer, asJSON bool, v any, text func(io.Writer) error) error {
	if asJSON {
		return RenderJSON(w, v)
	}
	return text(w)
}
