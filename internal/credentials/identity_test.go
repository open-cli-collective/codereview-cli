package credentials

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/open-cli-collective/cli-common/credstore"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

type identityReaderFunc func(string, string) (string, error)

func (reader identityReaderFunc) Get(profile, key string) (string, error) {
	return reader(profile, key)
}

func TestIdentityAPIKeyForKindUsesFixedAllowlistedKeys(t *testing.T) {
	tests := []struct {
		kind string
		key  string
	}{
		{kind: "system_one", key: SystemOneAPIKeyKey},
		{kind: "openai_decisions", key: OpenAIAPIKeyKey},
		{kind: "cloudflare_clef", key: CloudflareAPIKeyKey},
	}
	for _, test := range tests {
		t.Run(test.kind, func(t *testing.T) {
			key, err := IdentityAPIKeyForKind(test.kind)
			if err != nil || key != test.key {
				t.Fatalf("IdentityAPIKeyForKind = %q, %v; want %q", key, err, test.key)
			}
			if err := ValidateAllowedKey(key); err != nil {
				t.Fatalf("identity key %q is not allowlisted: %v", key, err)
			}
		})
	}
	if _, err := IdentityAPIKeyForKind("other"); !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("unknown kind error = %v, want invalid configuration", err)
	}
	want := []string{GitTokenKey, GitHubAppPrivateKeyKey, AnthropicAPIKeyKey, OpenAIAPIKeyKey, SystemOneAPIKeyKey, CloudflareAPIKeyKey}
	if got := AllowedKeys(); !reflect.DeepEqual(got, want) {
		t.Fatalf("AllowedKeys = %#v, want %#v", got, want)
	}
}

func TestReadIdentityAPIKeyReadsOnlyTheSelectedFixedKey(t *testing.T) {
	var calls int
	reader := identityReaderFunc(func(profile, key string) (string, error) {
		calls++
		if profile != "identity" || key != SystemOneAPIKeyKey {
			t.Fatalf("reader.Get(%q, %q), want identity/%s", profile, key, SystemOneAPIKeyKey)
		}
		return "synthetic-key", nil
	})
	value, err := ReadIdentityAPIKey(context.Background(), config.File{}, reader, config.CredentialLocation{
		Store: config.LocalOSCredentialStoreID, Name: "codereview/identity",
	}, "system_one")
	if err != nil || value != "synthetic-key" || calls != 1 {
		t.Fatalf("ReadIdentityAPIKey = %q, %v; reads=%d", value, err, calls)
	}
}

func TestReadIdentityAPIKeyRejectsInvalidLocationAndConflictingDeclaredKeyBeforeRead(t *testing.T) {
	calls := 0
	reader := identityReaderFunc(func(string, string) (string, error) {
		calls++
		return "synthetic-key", nil
	})
	invalidLocation := config.CredentialLocation{Store: "missing-store", Name: "codereview/identity"}
	if _, err := ReadIdentityAPIKey(context.Background(), config.File{}, reader, invalidLocation, "system_one"); err == nil {
		t.Fatal("missing selected store was accepted")
	}
	if calls != 0 {
		t.Fatalf("invalid store read credential %d times", calls)
	}

	cfg := config.File{Profiles: map[string]config.Profile{
		"identity": matrixProfile("codereview/git", "codereview/identity", config.LLMProviderOpenAI),
	}}
	if _, err := ReadIdentityAPIKey(context.Background(), cfg, reader, config.CredentialLocation{
		Store: config.LocalOSCredentialStoreID, Name: "codereview/identity",
	}, "system_one"); !errors.Is(err, credstore.ErrKeyNotAllowed) {
		t.Fatalf("conflicting declared key error = %v, want disallowed-key error", err)
	}
	if calls != 0 {
		t.Fatalf("conflicting ref read credential %d times", calls)
	}
}

func TestReadIdentityAPIKeyChecksCancellationAroundOneRead(t *testing.T) {
	t.Run("already canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		reader := identityReaderFunc(func(string, string) (string, error) {
			calls++
			return "synthetic-key", nil
		})
		_, err := ReadIdentityAPIKey(ctx, config.File{}, reader, config.CredentialLocation{
			Store: config.LocalOSCredentialStoreID, Name: "codereview/identity",
		}, "system_one")
		if !errors.Is(err, context.Canceled) || calls != 0 {
			t.Fatalf("canceled read error=%v calls=%d", err, calls)
		}
	})

	t.Run("canceled during read", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := 0
		reader := identityReaderFunc(func(string, string) (string, error) {
			calls++
			cancel()
			return "synthetic-key", nil
		})
		value, err := ReadIdentityAPIKey(ctx, config.File{}, reader, config.CredentialLocation{
			Store: config.LocalOSCredentialStoreID, Name: "codereview/identity",
		}, "system_one")
		if value != "" || !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatalf("canceled in-read result = %q, %v; calls=%d", value, err, calls)
		}
	})
}

func TestReadIdentityAPIKeySanitizesReadAndEmptyValueFailures(t *testing.T) {
	tests := []struct {
		name   string
		reader Reader
	}{
		{name: "reader error", reader: identityReaderFunc(func(string, string) (string, error) { return "", errors.New("secret-bearing backend detail") })},
		{name: "empty value", reader: identityReaderFunc(func(string, string) (string, error) { return "", nil })},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value, err := ReadIdentityAPIKey(context.Background(), config.File{}, test.reader, config.CredentialLocation{
				Store: config.LocalOSCredentialStoreID, Name: "codereview/identity",
			}, "system_one")
			if value != "" || !errors.Is(err, errIdentityAPIKeyUnavailable) || strings.Contains(err.Error(), "secret-bearing") {
				t.Fatalf("sanitized failure = %q, %v", value, err)
			}
		})
	}
}
