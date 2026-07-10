package view

import (
	"bytes"
	"io"
	"testing"
)

func TestRender(t *testing.T) {
	var out bytes.Buffer
	value := struct {
		HTML string `json:"html"`
	}{HTML: "<tag>"}
	if err := Render(&out, true, value, func(io.Writer) error {
		t.Fatal("text renderer called for JSON output")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"html\": \"\\u003ctag\\u003e\"\n}\n"; out.String() != want {
		t.Fatalf("JSON output = %q, want %q", out.String(), want)
	}
	out.Reset()
	if err := Render(&out, false, value, func(w io.Writer) error {
		_, err := io.WriteString(w, "text\n")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "text\n" {
		t.Fatalf("text output = %q, want %q", out.String(), "text\n")
	}
}
