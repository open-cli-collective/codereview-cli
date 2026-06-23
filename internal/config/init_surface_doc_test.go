package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestInitConfigSurfaceInventoryMatchesSchema(t *testing.T) {
	doc := readInitConfigSurfaceDoc(t)
	got := initSurfaceInventoryPaths(t, doc)
	want := initSurfaceExpectedSchemaPaths()

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("init config surface paths mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestInitConfigSurfaceAuditsInitControlFlags(t *testing.T) {
	doc := readInitConfigSurfaceDoc(t)
	for _, value := range []string{"`cr init` control", "--non-interactive", "--replace-profile"} {
		if !strings.Contains(doc, value) {
			t.Fatalf("init config surface doc missing %q", value)
		}
	}
}

func readInitConfigSurfaceDoc(t *testing.T) string {
	t.Helper()

	path := filepath.Join("..", "..", "docs", "init-config-surface.md")
	// #nosec G304 -- test reads a repo-relative documentation file.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

func initSurfaceInventoryPaths(t *testing.T, doc string) []string {
	t.Helper()

	re := regexp.MustCompile(`(?m)^\| (config\.[^|]+?) \|`)
	matches := re.FindAllStringSubmatch(doc, -1)
	if len(matches) == 0 {
		t.Fatal("init config surface doc has no durable config inventory rows")
	}
	seen := map[string]struct{}{}
	paths := make([]string, 0, len(matches))
	for _, match := range matches {
		path := strings.TrimSpace(match[1])
		if _, ok := seen[path]; ok {
			t.Fatalf("duplicate init config surface path %q", path)
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func initSurfaceExpectedSchemaPaths() []string {
	paths := configSchemaLeafPaths(reflect.TypeOf(File{}), "config")
	paths = append(paths,
		"config.repository_profiles[]",
		"config.profiles.<name>",
	)
	sort.Strings(paths)
	return paths
}

func configSchemaLeafPaths(typ reflect.Type, prefix string) []string {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}

	if typ.Kind() == reflect.Struct {
		paths := []string{}
		for index := 0; index < typ.NumField(); index++ {
			field := typ.Field(index)
			if field.PkgPath != "" {
				continue
			}
			name := yamlFieldName(field)
			if name == "" {
				continue
			}
			next := prefix + "." + name
			if name == "profiles" && field.Type.Kind() == reflect.Map {
				paths = append(paths, configSchemaLeafPaths(field.Type.Elem(), next+".<name>")...)
				continue
			}
			paths = append(paths, configSchemaLeafPaths(field.Type, next)...)
		}
		return paths
	}

	if typ.Kind() == reflect.Slice {
		elem := typ.Elem()
		for elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		if elem.Kind() == reflect.Struct {
			return configSchemaLeafPaths(elem, prefix+"[]")
		}
		return []string{prefix + "[]"}
	}

	if typ.Kind() == reflect.Map {
		return []string{prefix}
	}

	return []string{prefix}
}

func yamlFieldName(field reflect.StructField) string {
	tag := field.Tag.Get("yaml")
	name, _, _ := strings.Cut(tag, ",")
	if name == "-" {
		return ""
	}
	return name
}
