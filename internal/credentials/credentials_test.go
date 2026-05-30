package credentials

import (
	"errors"
	"testing"

	"github.com/open-cli-collective/cli-common/credstore"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

func TestParseRefEnforcesCodereviewService(t *testing.T) {
	ref, err := ParseRef("codereview/work")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if ref.Profile != "work" || ref.Full != "codereview/work" {
		t.Fatalf("ParseRef = %#v, want work ref", ref)
	}

	_, err = ParseRef("other/work")
	if !errors.Is(err, ErrWrongService) {
		t.Fatalf("wrong-service error = %v, want ErrWrongService", err)
	}
}

func TestStoreOptionsBackendPrecedenceMetadata(t *testing.T) {
	t.Setenv(BackendEnvVar(), "")

	cfg := config.File{Keyring: config.KeyringConfig{Backend: "memory"}}
	store, err := OpenStore("", false, cfg)
	if err != nil {
		t.Fatalf("OpenStore config backend: %v", err)
	}
	backend, source := store.Backend()
	_ = store.Close()
	if backend != credstore.BackendMemory || source != credstore.SourceConfig {
		t.Fatalf("Backend = (%s,%s), want (memory,config)", backend, source)
	}

	store, err = OpenStore("memory", true, config.File{})
	if err != nil {
		t.Fatalf("OpenStore explicit backend: %v", err)
	}
	backend, source = store.Backend()
	_ = store.Close()
	if backend != credstore.BackendMemory || source != credstore.SourceExplicit {
		t.Fatalf("Backend = (%s,%s), want (memory,explicit)", backend, source)
	}

	t.Setenv(BackendEnvVar(), "memory")
	store, err = OpenStore("", false, config.File{})
	if err != nil {
		t.Fatalf("OpenStore env backend: %v", err)
	}
	backend, source = store.Backend()
	_ = store.Close()
	if backend != credstore.BackendMemory || source != credstore.SourceEnv {
		t.Fatalf("Backend = (%s,%s), want (memory,env)", backend, source)
	}
}

func TestStoreOptionsInvalidBackendFlag(t *testing.T) {
	_, err := StoreOptions("bogus", true, config.File{})
	if !errors.Is(err, ErrInvalidBackendSelection) {
		t.Fatalf("StoreOptions error = %v, want ErrInvalidBackendSelection", err)
	}
}

func TestAllowedKeyMemoryRoundTrip(t *testing.T) {
	store, err := OpenStore("memory", true, config.File{})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	if err := store.Set("work", GitTokenKey, "token"); err != nil {
		t.Fatalf("Set allowed key: %v", err)
	}
	got, err := store.Get("work", GitTokenKey)
	if err != nil {
		t.Fatalf("Get allowed key: %v", err)
	}
	if got != "token" {
		t.Fatalf("Get = %q, want token", got)
	}
	if err := store.Set("work", "bad_key", "token"); !errors.Is(err, credstore.ErrKeyNotAllowed) {
		t.Fatalf("Set disallowed key error = %v, want ErrKeyNotAllowed", err)
	}
}
