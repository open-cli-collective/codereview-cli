//go:build ignore

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func main() {
	if len(os.Args) != 3 {
		fatal("usage: go run ./scripts/review-json-field.go <review-json> <field.path>")
	}
	body, err := os.ReadFile(os.Args[1])
	if err != nil {
		fatal("read %s: %v", os.Args[1], err)
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		fatal("decode %s: %v", os.Args[1], err)
	}
	for _, part := range strings.Split(os.Args[2], ".") {
		object, ok := value.(map[string]any)
		if !ok {
			fatal("field %q is not an object at %q", os.Args[2], part)
		}
		next, ok := object[part]
		if !ok {
			fatal("field %q missing at %q", os.Args[2], part)
		}
		value = next
	}
	switch typed := value.(type) {
	case string:
		fmt.Println(typed)
	default:
		fatal("field %q is %T, want string", os.Args[2], value)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "review-json-field: "+format+"\n", args...)
	os.Exit(1)
}
