package fsatomic

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestWriteFileAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "artifact.json")
	if err := WriteFileAtomic(path, []byte("first"), 0o400); err != nil {
		t.Fatalf("WriteFileAtomic first: %v", err)
	}
	if err := WriteFileAtomic(path, []byte("second"), 0o400); err != nil {
		t.Fatalf("WriteFileAtomic replace: %v", err)
	}

	// #nosec G304 -- test path is controlled by t.TempDir.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "second" {
		t.Fatalf("content = %q, want second", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o400 {
		t.Fatalf("mode = %o, want 400", got)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temporary file still exists: %v", err)
	}
}

func TestWriteFileAtomicDoesNotReplaceTargetWhenTempWriteFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact.json")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	if err := os.Mkdir(path+".tmp", 0o700); err != nil {
		t.Fatalf("Mkdir temp blocker: %v", err)
	}

	if err := WriteFileAtomic(path, []byte("replacement"), 0o600); err == nil {
		t.Fatal("WriteFileAtomic error = nil, want temp write failure")
	}
	// #nosec G304 -- test path is controlled by t.TempDir.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(data) != "original" {
		t.Fatalf("content = %q, want original", data)
	}
}

func TestWriteJSONAndReadJSON(t *testing.T) {
	type payload struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	want := payload{Name: "review", Count: 2}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := WriteJSON(path, want); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	// #nosec G304 -- test path is controlled by t.TempDir.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got := string(data); got != "{\n  \"name\": \"review\",\n  \"count\": 2\n}\n" {
		t.Fatalf("JSON = %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}

	var got payload
	if err := ReadJSON(path, &got); err != nil {
		t.Fatalf("ReadJSON: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("payload = %#v, want %#v", got, want)
	}
}

func TestWriteJSONMarshalErrorDoesNotCreateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.json")
	if err := WriteJSON(path, make(chan int)); err == nil {
		t.Fatal("WriteJSON error = nil, want marshal failure")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("output file exists after marshal failure: %v", err)
	}
}

func TestReadJSONErrors(t *testing.T) {
	dir := t.TempDir()
	var got map[string]any
	if err := ReadJSON(filepath.Join(dir, "missing.json"), &got); !os.IsNotExist(err) {
		t.Fatalf("missing ReadJSON error = %v, want not exist", err)
	}
	path := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := ReadJSON(path, &got); err == nil {
		t.Fatal("invalid ReadJSON error = nil")
	}
}
