// Package modelcatalog loads the versioned model/runtime catalog used by cr.
// The package deliberately has no dependency on config or adapters: catalog
// data describes capabilities, while Go remains responsible for harness and
// transport behavior.
package modelcatalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/open-cli-collective/cli-common/statedir"
)

const (
	// SchemaVersion is the on-disk catalog schema understood by this binary.
	SchemaVersion = 1
	// CatalogDirName is the per-user directory holding installed snapshots.
	CatalogDirName = "catalog"
	currentPointer = "catalog.current"
	// DefaultUpdateURL is the public catalog source used by catalog update.
	DefaultUpdateURL = "https://raw.githubusercontent.com/open-cli-collective/codereview-cli/main/internal/modelcatalog/data"
)

//go:embed data/manifest.json data/runtimes.csv data/models.csv data/defaults.csv data/pricing.csv
var bundledFS embed.FS

var requiredFiles = []string{"manifest.json", "runtimes.csv", "models.csv", "defaults.csv", "pricing.csv"}

// Source describes where a snapshot came from.
type Source struct {
	Kind     string `json:"kind"`
	Path     string `json:"path,omitempty"`
	Revision string `json:"revision"`
}

// Manifest is the catalog compatibility and provenance header.
type Manifest struct {
	SchemaVersion int               `json:"schema_version"`
	Revision      string            `json:"revision"`
	GeneratedAt   string            `json:"generated_at,omitempty"`
	Sources       []Source          `json:"sources,omitempty"`
	Files         map[string]string `json:"files,omitempty"`
}

// Runtime describes an authenticated provider harness.
type Runtime struct {
	ID                    string `json:"runtime_id"`
	Provider              string `json:"provider"`
	Auth                  string `json:"auth"`
	Harness               string `json:"harness"`
	Adapter               string `json:"adapter"`
	SuggestedName         string `json:"suggested_name"`
	DisplayName           string `json:"display_name"`
	MaximumEffort         string `json:"maximum_effort"`
	RequiresCredentialRef bool   `json:"requires_credential_ref"`
	SourceURL             string `json:"source_url"`
	VerifiedDate          string `json:"verified_date"`
}

// Model describes one model as exposed by one runtime.
type Model struct {
	RuntimeID         string   `json:"runtime_id"`
	ModelID           string   `json:"model_id"`
	SupportedEfforts  []string `json:"supported_efforts,omitempty"`
	Fast              bool     `json:"fast"`
	FastKnown         bool     `json:"fast_known"`
	Ultrafast         bool     `json:"ultrafast"`
	UltrafastKnown    bool     `json:"ultrafast_known"`
	MinHarnessVersion string   `json:"min_harness_version,omitempty"`
	SourceURL         string   `json:"source_url"`
	VerifiedDate      string   `json:"verified_date"`
}

// Default is a portable tier mapping for one runtime.
type Default struct {
	RuntimeID    string `json:"runtime_id"`
	Tier         string `json:"tier"`
	ModelID      string `json:"model_id"`
	Effort       string `json:"effort"`
	MaxEffort    string `json:"max_effort,omitempty"`
	SourceURL    string `json:"source_url"`
	VerifiedDate string `json:"verified_date"`
}

// Price is a verified rate per one million tokens. Nil fields mean unknown,
// never zero.
type Price struct {
	ModelID      string   `json:"model_id"`
	Speed        string   `json:"speed"`
	ContextBand  string   `json:"context_band"`
	Input        *float64 `json:"input_usd_per_million,omitempty"`
	Output       *float64 `json:"output_usd_per_million,omitempty"`
	CacheRead    *float64 `json:"cache_read_usd_per_million,omitempty"`
	CacheWrite   *float64 `json:"cache_write_usd_per_million,omitempty"`
	CacheWrite5m *float64 `json:"cache_write_5m_usd_per_million,omitempty"`
	CacheWrite1h *float64 `json:"cache_write_1h_usd_per_million,omitempty"`
	SourceURL    string   `json:"source_url"`
	VerifiedDate string   `json:"verified_date"`
}

// Catalog is an immutable validated snapshot. Its slices are private so a
// command cannot accidentally mutate the snapshot used by another stage.
type Catalog struct {
	manifest Manifest
	source   Source
	runtimes []Runtime
	models   []Model
	defaults []Default
	pricing  []Price
}

// Manifest returns a copy of the snapshot manifest.
func (c *Catalog) Manifest() Manifest {
	if c == nil {
		return Manifest{}
	}
	m := c.manifest
	m.Sources = append([]Source(nil), m.Sources...)
	if m.Files != nil {
		m.Files = map[string]string{}
		for name, digest := range c.manifest.Files {
			m.Files[name] = digest
		}
	}
	return m
}

// Source returns the immutable snapshot source.
func (c *Catalog) Source() Source {
	if c == nil {
		return Source{}
	}
	return c.source
}

// Revision returns the snapshot revision.
func (c *Catalog) Revision() string { return c.Manifest().Revision }

// Runtimes returns a copy in stable order.
func (c *Catalog) Runtimes() []Runtime { return append([]Runtime(nil), c.runtimes...) }

// Models returns a copy in stable order.
func (c *Catalog) Models() []Model {
	out := append([]Model(nil), c.models...)
	for i := range out {
		out[i].SupportedEfforts = append([]string(nil), out[i].SupportedEfforts...)
	}
	return out
}

// Defaults returns a copy in stable order.
func (c *Catalog) Defaults() []Default { return append([]Default(nil), c.defaults...) }

// Pricing returns a copy in stable order.
func (c *Catalog) Pricing() []Price {
	out := append([]Price(nil), c.pricing...)
	for i := range out {
		out[i] = clonePrice(out[i])
	}
	return out
}

// Runtime finds a runtime by identity.
func (c *Catalog) Runtime(provider, auth, adapter string) (Runtime, bool) {
	for _, runtime := range c.runtimes {
		if runtime.Provider == provider && runtime.Auth == auth && runtime.Adapter == adapter {
			return runtime, true
		}
	}
	return Runtime{}, false
}

// RuntimeByID finds a runtime by stable catalog ID.
func (c *Catalog) RuntimeByID(id string) (Runtime, bool) {
	for _, runtime := range c.runtimes {
		if runtime.ID == id {
			return runtime, true
		}
	}
	return Runtime{}, false
}

// ModelsForRuntime returns the models offered by one runtime.
func (c *Catalog) ModelsForRuntime(runtimeID string) []Model {
	out := make([]Model, 0)
	for _, model := range c.models {
		if model.RuntimeID == runtimeID {
			model.SupportedEfforts = append([]string(nil), model.SupportedEfforts...)
			out = append(out, model)
		}
	}
	return out
}

// DefaultFor returns the catalog default for a runtime and tier.
func (c *Catalog) DefaultFor(runtimeID, tier string) (Default, bool) {
	for _, value := range c.defaults {
		if value.RuntimeID == runtimeID && value.Tier == tier {
			return value, true
		}
	}
	return Default{}, false
}

// PriceFor returns a price row for a concrete model and observed speed.
func (c *Catalog) PriceFor(modelID, speed string) (Price, bool) {
	for _, price := range c.pricing {
		if price.ModelID == modelID && price.Speed == speed && price.ContextBand == "all" {
			return clonePrice(price), true
		}
	}
	return Price{}, false
}

// PricesFor returns every context-band rate for a concrete model and speed.
func (c *Catalog) PricesFor(modelID, speed string) []Price {
	prices := make([]Price, 0)
	for _, price := range c.pricing {
		if price.ModelID == modelID && price.Speed == speed {
			prices = append(prices, clonePrice(price))
		}
	}
	return prices
}

// LoadBundled loads the offline catalog embedded in the binary.
func LoadBundled() (*Catalog, error) {
	return loadFS(bundledFS, "data", Source{Kind: "bundled"})
}

// InstalledDir returns the per-user installed catalog directory.
func InstalledDir() (string, error) {
	root, err := (statedir.Data{Tool: "cr"}).DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, CatalogDirName), nil
}

// LoadInstalled loads the installed catalog, falling back to the bundled
// snapshot only when no installed directory exists.
func LoadInstalled() (*Catalog, error) {
	dir, err := InstalledDir()
	if err != nil {
		return nil, fmt.Errorf("model catalog: resolve install path: %w", err)
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return LoadBundled()
	} else if err != nil {
		return nil, fmt.Errorf("model catalog: inspect installed catalog: %w", err)
	}
	if pointer, readErr := os.ReadFile(filepath.Join(dir, currentPointer)); readErr == nil { // #nosec G304 -- dir is the resolved per-user catalog directory.
		revisionDir := strings.TrimSpace(string(pointer))
		if revisionDir == "" || revisionDir == "." || revisionDir == ".." || filepath.Base(revisionDir) != revisionDir || strings.ContainsAny(revisionDir, `/\\`) {
			return nil, errors.New("model catalog: installed pointer is invalid")
		}
		catalog, loadErr := LoadPath(filepath.Join(dir, revisionDir))
		if loadErr != nil {
			return nil, loadErr
		}
		catalog.source = Source{Kind: "installed", Path: filepath.Join(dir, revisionDir), Revision: catalog.Revision()}
		return catalog, nil
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return nil, fmt.Errorf("model catalog: read installed pointer: %w", readErr)
	}
	// A directory without the atomically published pointer is an incomplete
	// install. Keep the bundled baseline active until a complete revision is
	// published; explicit local paths still use LoadPath directly.
	return LoadBundled()
}

// LoadConfigured loads an explicit local directory or the installed/bundled
// snapshot when path is empty.
func LoadConfigured(path string) (*Catalog, error) {
	if strings.TrimSpace(path) != "" {
		return LoadPath(path)
	}
	return LoadInstalled()
}

// LoadPath validates a catalog directory completely before returning it.
func LoadPath(dir string) (*Catalog, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("model catalog: catalog path is required")
	}
	return loadFS(osDirFS(dir), ".", Source{Kind: "local", Path: dir})
}

// UpdateOptions controls catalog update. SourceDir is useful for offline
// package tests and local mirrors; URL downloads the five catalog files.
type UpdateOptions struct {
	SourceDir  string
	URL        string
	InstallDir string
	HTTPClient *http.Client
}

// Update validates an entire source snapshot then atomically installs it.
func Update(ctx context.Context, opts UpdateOptions) (*Catalog, error) {
	// A refresh is best-effort during package installation. Bound the whole
	// snapshot, including all five downloads, rather than allowing one timeout
	// per file to stretch an install for several minutes.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	target := strings.TrimSpace(opts.InstallDir)
	if target == "" {
		var err error
		target, err = InstalledDir()
		if err != nil {
			return nil, fmt.Errorf("model catalog: resolve install path: %w", err)
		}
	}
	if strings.TrimSpace(opts.SourceDir) != "" {
		return installDir(opts.SourceDir, target)
	}
	base := strings.TrimRight(strings.TrimSpace(opts.URL), "/")
	if base == "" {
		base = DefaultUpdateURL
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	files := make(map[string][]byte, len(requiredFiles))
	for _, name := range requiredFiles {
		body, err := download(ctx, client, base+"/"+name)
		if err != nil {
			return nil, err
		}
		files[name] = body
	}
	catalog, err := loadFiles(files, Source{Kind: "download"})
	if err != nil {
		return nil, err
	}
	if err := requireChecksums(catalog.Manifest()); err != nil {
		return nil, err
	}
	return installFiles(files, target, catalog)
}

func download(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("model catalog: create download request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("model catalog: download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("model catalog: download %s: HTTP %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("model catalog: read download %s: %w", url, err)
	}
	return body, nil
}

func readRequiredFiles(source string) (map[string][]byte, error) {
	files := make(map[string][]byte, len(requiredFiles))
	for _, name := range requiredFiles {
		body, err := os.ReadFile(filepath.Join(source, name)) // #nosec G304 -- source is the explicit catalog directory selected by the caller.
		if err != nil {
			return nil, fmt.Errorf("model catalog: stage %s: %w", name, err)
		}
		files[name] = body
	}
	return files, nil
}

func installDir(source, target string) (*Catalog, error) {
	files, err := readRequiredFiles(source)
	if err != nil {
		return nil, err
	}
	catalog, err := loadFiles(files, Source{Kind: "install"})
	if err != nil {
		return nil, err
	}
	return installFiles(files, target, catalog)
}

func installFiles(files map[string][]byte, target string, catalog *Catalog) (*Catalog, error) {
	if catalog == nil {
		var err error
		catalog, err = loadFiles(files, Source{Kind: "install"})
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return nil, fmt.Errorf("model catalog: create install directory: %w", err)
	}
	temp, err := os.MkdirTemp(target, ".catalog-install-")
	if err != nil {
		return nil, fmt.Errorf("model catalog: create install staging: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(temp)
		}
	}()
	for _, name := range requiredFiles {
		body := files[name]
		if err := os.WriteFile(filepath.Join(temp, name), body, 0o600); err != nil {
			return nil, fmt.Errorf("model catalog: stage %s: %w", name, err)
		}
	}
	digest := snapshotDigest(files)
	revisionDir := fmt.Sprintf("r-%x", digest[:8])
	finalDir := filepath.Join(target, revisionDir)
	if _, statErr := os.Stat(finalDir); statErr == nil {
		matches, matchErr := revisionContentsMatch(finalDir, files)
		if matchErr != nil {
			return nil, fmt.Errorf("model catalog: inspect existing revision: %w", matchErr)
		}
		if !matches {
			return nil, fmt.Errorf("model catalog: existing revision %s does not match snapshot", revisionDir)
		}
		_ = os.RemoveAll(temp)
	} else if errors.Is(statErr, os.ErrNotExist) {
		if err := os.Rename(temp, finalDir); err != nil {
			return nil, fmt.Errorf("model catalog: install revision: %w", err)
		}
	} else {
		return nil, fmt.Errorf("model catalog: inspect installed revision: %w", statErr)
	}
	pointerTemp, err := os.CreateTemp(target, ".catalog-pointer-")
	if err != nil {
		return nil, fmt.Errorf("model catalog: create installed pointer: %w", err)
	}
	pointerName := pointerTemp.Name()
	if _, err := io.WriteString(pointerTemp, revisionDir+"\n"); err != nil {
		_ = pointerTemp.Close()
		_ = os.Remove(pointerName)
		return nil, fmt.Errorf("model catalog: write installed pointer: %w", err)
	}
	if err := pointerTemp.Close(); err != nil {
		_ = os.Remove(pointerName)
		return nil, fmt.Errorf("model catalog: close installed pointer: %w", err)
	}
	if err := os.Rename(pointerName, filepath.Join(target, currentPointer)); err != nil {
		_ = os.Remove(pointerName)
		return nil, fmt.Errorf("model catalog: publish installed pointer: %w", err)
	}
	keep = true
	catalog.source = Source{Kind: "installed", Path: finalDir, Revision: catalog.Revision()}
	return catalog, nil
}

func revisionContentsMatch(dir string, files map[string][]byte) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	if len(entries) != len(requiredFiles) {
		return false, nil
	}
	for _, name := range requiredFiles {
		body, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- dir is the staged revision directory created by this package.
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return false, nil
			}
			return false, err
		}
		if !bytes.Equal(body, files[name]) {
			return false, nil
		}
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return false, nil
		}
		known := false
		for _, name := range requiredFiles {
			if entry.Name() == name {
				known = true
				break
			}
		}
		if !known {
			return false, nil
		}
	}
	return true, nil
}

func requireChecksums(manifest Manifest) error {
	for _, name := range requiredFiles[1:] {
		if strings.TrimSpace(manifest.Files[name]) == "" {
			return fmt.Errorf("model catalog: downloaded manifest must checksum %s", name)
		}
	}
	return nil
}

func snapshotDigest(files map[string][]byte) [32]byte {
	h := sha256.New()
	for _, name := range requiredFiles {
		_, _ = io.WriteString(h, name+"\x00")
		_, _ = h.Write(files[name])
		_, _ = io.WriteString(h, "\x00")
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

func loadFS(fsys fsLike, root string, source Source) (*Catalog, error) {
	files := make(map[string][]byte, len(requiredFiles))
	for _, name := range requiredFiles {
		body, err := fsys.ReadFile(path.Join(root, name))
		if err != nil {
			return nil, fmt.Errorf("model catalog: read %s: %w", name, err)
		}
		files[name] = body
	}
	return loadFiles(files, source)
}

func loadFiles(files map[string][]byte, source Source) (*Catalog, error) {
	manifestBytes, ok := files["manifest.json"]
	if !ok {
		return nil, errors.New("model catalog: read manifest: file is missing")
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("model catalog: parse manifest: %w", err)
	}
	if manifest.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("model catalog: schema version %d is incompatible with %d", manifest.SchemaVersion, SchemaVersion)
	}
	manifest.Revision = strings.TrimSpace(manifest.Revision)
	if manifest.Revision == "" {
		return nil, errors.New("model catalog: manifest revision is required")
	}
	if source.Revision == "" {
		source.Revision = manifest.Revision
	}
	for _, name := range requiredFiles[1:] {
		want := strings.TrimSpace(manifest.Files[name])
		if want == "" {
			continue
		}
		body, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("model catalog: read %s for checksum: file is missing", name)
		}
		got := fmt.Sprintf("%x", sha256.Sum256(body))
		if !strings.EqualFold(got, want) {
			return nil, fmt.Errorf("model catalog: %s checksum %s does not match manifest %s", name, got, want)
		}
	}
	fsys := mapFS(files)
	runtimes, err := parseRuntimes(fsys, ".")
	if err != nil {
		return nil, err
	}
	models, err := parseModels(fsys, ".")
	if err != nil {
		return nil, err
	}
	defaults, err := parseDefaults(fsys, ".")
	if err != nil {
		return nil, err
	}
	pricing, err := parsePricing(fsys, ".")
	if err != nil {
		return nil, err
	}
	if err := validateRelations(runtimes, models, defaults, pricing); err != nil {
		return nil, err
	}
	source.Revision = manifest.Revision
	manifest.Sources = append([]Source(nil), manifest.Sources...)
	return &Catalog{manifest: manifest, source: source, runtimes: runtimes, models: models, defaults: defaults, pricing: pricing}, nil
}

type fsLike interface{ ReadFile(string) ([]byte, error) }

type mapFS map[string][]byte

func (f mapFS) ReadFile(name string) ([]byte, error) {
	body, ok := f[path.Clean(name)]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), body...), nil
}

type osFS string

func osDirFS(dir string) osFS { return osFS(filepath.Clean(dir)) }
func (f osFS) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(string(f), name)) // #nosec G304 -- name is one of the package's fixed catalog files.
}

func readCSV(fsys fsLike, root, name string, want []string) ([]map[string]string, error) {
	body, err := fsys.ReadFile(path.Join(root, name))
	if err != nil {
		return nil, fmt.Errorf("model catalog: read %s: %w", name, err)
	}
	reader := csv.NewReader(strings.NewReader(string(body)))
	reader.TrimLeadingSpace = true
	headers, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("model catalog: read %s header: %w", name, err)
	}
	if len(headers) != len(want) {
		return nil, fmt.Errorf("model catalog: %s header has %d fields, want %d", name, len(headers), len(want))
	}
	for i := range headers {
		if strings.TrimSpace(headers[i]) != want[i] {
			return nil, fmt.Errorf("model catalog: %s header field %d is %q, want %q", name, i, headers[i], want[i])
		}
	}
	rows := make([]map[string]string, 0)
	for line := 2; ; line++ {
		values, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("model catalog: parse %s line %d: %w", name, line, err)
		}
		if len(values) != len(headers) {
			return nil, fmt.Errorf("model catalog: %s line %d has %d fields, want %d", name, line, len(values), len(headers))
		}
		row := make(map[string]string, len(headers))
		blank := true
		for i, value := range values {
			value = strings.TrimSpace(value)
			row[headers[i]] = value
			blank = blank && value == ""
		}
		if !blank {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func parseRuntimes(fsys fsLike, root string) ([]Runtime, error) {
	rows, err := readCSV(fsys, root, "runtimes.csv", []string{"runtime_id", "provider", "auth", "harness", "adapter", "suggested_name", "display_name", "maximum_effort", "requires_credential_ref", "source_url", "verified_date"})
	if err != nil {
		return nil, err
	}
	out := make([]Runtime, 0, len(rows))
	seen := map[string]bool{}
	identitySeen := map[string]bool{}
	for _, row := range rows {
		runtime := Runtime{ID: row["runtime_id"], Provider: row["provider"], Auth: row["auth"], Harness: row["harness"], Adapter: row["adapter"], SuggestedName: row["suggested_name"], DisplayName: row["display_name"], MaximumEffort: row["maximum_effort"], SourceURL: row["source_url"], VerifiedDate: row["verified_date"]}
		if runtime.ID == "" || seen[runtime.ID] {
			return nil, fmt.Errorf("model catalog: duplicate or blank runtime_id %q", runtime.ID)
		}
		seen[runtime.ID] = true
		identity := runtime.Provider + "\x00" + runtime.Auth + "\x00" + runtime.Adapter
		if identitySeen[identity] {
			return nil, fmt.Errorf("model catalog: duplicate runtime identity %s/%s/%s", runtime.Provider, runtime.Auth, runtime.Adapter)
		}
		identitySeen[identity] = true
		if runtime.Provider == "" || runtime.Auth == "" || runtime.Harness == "" || runtime.Adapter == "" || runtime.SuggestedName == "" || runtime.DisplayName == "" || !validEffort(runtime.MaximumEffort) || runtime.SourceURL == "" || runtime.VerifiedDate == "" {
			return nil, fmt.Errorf("model catalog: incomplete runtime %q", runtime.ID)
		}
		switch row["requires_credential_ref"] {
		case "Y":
			runtime.RequiresCredentialRef = true
		case "N":
		case "":
			return nil, fmt.Errorf("model catalog: runtime %q requires_credential_ref must be Y or N", runtime.ID)
		default:
			return nil, fmt.Errorf("model catalog: runtime %q requires_credential_ref %q is invalid", runtime.ID, row["requires_credential_ref"])
		}
		out = append(out, runtime)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func parseModels(fsys fsLike, root string) ([]Model, error) {
	rows, err := readCSV(fsys, root, "models.csv", []string{"runtime_id", "model_id", "supported_efforts", "fast", "ultrafast", "min_harness_version", "source_url", "verified_date"})
	if err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		model := Model{RuntimeID: row["runtime_id"], ModelID: row["model_id"], MinHarnessVersion: row["min_harness_version"], SourceURL: row["source_url"], VerifiedDate: row["verified_date"]}
		key := model.RuntimeID + "\x00" + model.ModelID
		if model.RuntimeID == "" || model.ModelID == "" || seen[key] {
			return nil, fmt.Errorf("model catalog: duplicate or blank model %q", key)
		}
		seen[key] = true
		if model.SourceURL == "" || model.VerifiedDate == "" {
			return nil, fmt.Errorf("model catalog: incomplete model %q", model.ModelID)
		}
		if rawEfforts := strings.TrimSpace(row["supported_efforts"]); rawEfforts != "" && !strings.EqualFold(rawEfforts, "null") {
			for _, effort := range strings.Split(rawEfforts, "|") {
				effort = strings.TrimSpace(effort)
				if !validEffort(effort) {
					return nil, fmt.Errorf("model catalog: model %q effort %q is invalid", model.ModelID, effort)
				}
				model.SupportedEfforts = append(model.SupportedEfforts, effort)
			}
		}
		if model.MinHarnessVersion != "" {
			return nil, fmt.Errorf("model catalog: model %q min_harness_version is not supported by this binary", model.ModelID)
		}
		var capErr error
		model.Fast, model.FastKnown, capErr = parseCapability(row["fast"])
		if capErr != nil {
			return nil, fmt.Errorf("model catalog: model %q fast: %w", model.ModelID, capErr)
		}
		model.Ultrafast, model.UltrafastKnown, capErr = parseCapability(row["ultrafast"])
		if capErr != nil {
			return nil, fmt.Errorf("model catalog: model %q ultrafast: %w", model.ModelID, capErr)
		}
		out = append(out, model)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RuntimeID == out[j].RuntimeID {
			return out[i].ModelID < out[j].ModelID
		}
		return out[i].RuntimeID < out[j].RuntimeID
	})
	return out, nil
}

func parseCapability(value string) (bool, bool, error) {
	switch strings.TrimSpace(strings.ToUpper(value)) {
	case "Y":
		return true, true, nil
	case "N":
		return false, true, nil
	case "", "NULL":
		return false, false, nil
	default:
		return false, false, fmt.Errorf("value %q must be Y, N, empty, or null", value)
	}
}

func parseDefaults(fsys fsLike, root string) ([]Default, error) {
	rows, err := readCSV(fsys, root, "defaults.csv", []string{"runtime_id", "tier", "model_id", "effort", "max_effort", "source_url", "verified_date"})
	if err != nil {
		return nil, err
	}
	out := make([]Default, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		value := Default{RuntimeID: row["runtime_id"], Tier: row["tier"], ModelID: row["model_id"], Effort: row["effort"], MaxEffort: row["max_effort"], SourceURL: row["source_url"], VerifiedDate: row["verified_date"]}
		key := value.RuntimeID + "\x00" + value.Tier
		if value.RuntimeID == "" || value.Tier == "" || value.ModelID == "" || seen[key] {
			return nil, fmt.Errorf("model catalog: duplicate or incomplete default %q", key)
		}
		seen[key] = true
		if value.Tier != "small" && value.Tier != "medium" && value.Tier != "large" {
			return nil, fmt.Errorf("model catalog: default tier %q is invalid", value.Tier)
		}
		if !validEffort(value.Effort) {
			return nil, fmt.Errorf("model catalog: default %q effort %q is invalid", key, value.Effort)
		}
		if value.MaxEffort != "" && !validEffort(value.MaxEffort) {
			return nil, fmt.Errorf("model catalog: default %q max_effort %q is invalid", key, value.MaxEffort)
		}
		if value.SourceURL == "" || value.VerifiedDate == "" {
			return nil, fmt.Errorf("model catalog: incomplete default %q", key)
		}
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RuntimeID == out[j].RuntimeID {
			return out[i].Tier < out[j].Tier
		}
		return out[i].RuntimeID < out[j].RuntimeID
	})
	return out, nil
}

func parsePricing(fsys fsLike, root string) ([]Price, error) {
	rows, err := readCSV(fsys, root, "pricing.csv", []string{"model_id", "speed", "context_band", "input_usd_per_million", "output_usd_per_million", "cache_read_usd_per_million", "cache_write_usd_per_million", "cache_write_5m_usd_per_million", "cache_write_1h_usd_per_million", "source_url", "verified_date"})
	if err != nil {
		return nil, err
	}
	out := make([]Price, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		value := Price{ModelID: row["model_id"], Speed: row["speed"], ContextBand: row["context_band"], SourceURL: row["source_url"], VerifiedDate: row["verified_date"]}
		key := value.ModelID + "\x00" + value.Speed + "\x00" + value.ContextBand
		if value.ModelID == "" || (value.Speed != "standard" && value.Speed != "fast" && value.Speed != "ultrafast") || seen[key] {
			return nil, fmt.Errorf("model catalog: duplicate or incomplete price %q", key)
		}
		seen[key] = true
		if value.SourceURL == "" || value.VerifiedDate == "" {
			return nil, fmt.Errorf("model catalog: incomplete price %q", key)
		}
		if value.ContextBand != "all" && value.ContextBand != "short" && value.ContextBand != "long" {
			return nil, fmt.Errorf("model catalog: price %q context_band %q is invalid", key, value.ContextBand)
		}
		fields := []struct {
			name string
			dest **float64
		}{{"input_usd_per_million", &value.Input}, {"output_usd_per_million", &value.Output}, {"cache_read_usd_per_million", &value.CacheRead}, {"cache_write_usd_per_million", &value.CacheWrite}, {"cache_write_5m_usd_per_million", &value.CacheWrite5m}, {"cache_write_1h_usd_per_million", &value.CacheWrite1h}}
		for _, field := range fields {
			raw := strings.TrimSpace(row[field.name])
			if raw == "" || strings.EqualFold(raw, "null") {
				continue
			}
			parsed, parseErr := strconv.ParseFloat(raw, 64)
			if parseErr != nil || parsed < 0 || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
				return nil, fmt.Errorf("model catalog: price %q %s %q is invalid", key, field.name, raw)
			}
			*field.dest = &parsed
		}
		if value.Input == nil && value.Output == nil && value.CacheRead == nil && value.CacheWrite == nil && value.CacheWrite5m == nil && value.CacheWrite1h == nil {
			return nil, fmt.Errorf("model catalog: price %q has no known rates", key)
		}
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ModelID == out[j].ModelID {
			if out[i].Speed == out[j].Speed {
				return out[i].ContextBand < out[j].ContextBand
			}
			return out[i].Speed < out[j].Speed
		}
		return out[i].ModelID < out[j].ModelID
	})
	return out, nil
}

func validateRelations(runtimes []Runtime, models []Model, defaults []Default, pricing []Price) error {
	runtimeSet := map[string]bool{}
	runtimesByID := map[string]Runtime{}
	modelSet := map[string]bool{}
	modelsByRuntime := map[string]map[string]Model{}
	for _, runtime := range runtimes {
		runtimeSet[runtime.ID] = true
		runtimesByID[runtime.ID] = runtime
	}
	for _, model := range models {
		if !runtimeSet[model.RuntimeID] {
			return fmt.Errorf("model catalog: model %q references unknown runtime %q", model.ModelID, model.RuntimeID)
		}
		modelSet[model.ModelID] = true
		if modelsByRuntime[model.RuntimeID] == nil {
			modelsByRuntime[model.RuntimeID] = map[string]Model{}
		}
		modelsByRuntime[model.RuntimeID][model.ModelID] = model
	}
	for _, value := range defaults {
		if !runtimeSet[value.RuntimeID] {
			return fmt.Errorf("model catalog: default references unknown runtime %q", value.RuntimeID)
		}
		if !modelSet[value.ModelID] {
			return fmt.Errorf("model catalog: default %q references unknown model %q", value.Tier, value.ModelID)
		}
		found := false
		for _, model := range models {
			if model.RuntimeID == value.RuntimeID && model.ModelID == value.ModelID {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("model catalog: default %s/%s model %q is not offered by runtime", value.RuntimeID, value.Tier, value.ModelID)
		}
		model := modelsByRuntime[value.RuntimeID][value.ModelID]
		if !contains(model.SupportedEfforts, value.Effort) {
			return fmt.Errorf("model catalog: default %s/%s effort %q is not supported by model %q", value.RuntimeID, value.Tier, value.Effort, value.ModelID)
		}
		if value.MaxEffort != "" && !contains(model.SupportedEfforts, value.MaxEffort) {
			return fmt.Errorf("model catalog: default %s/%s max_effort %q is not supported by model %q", value.RuntimeID, value.Tier, value.MaxEffort, value.ModelID)
		}
		runtime := runtimesByID[value.RuntimeID]
		if !effortAtMost(value.Effort, runtime.MaximumEffort) {
			return fmt.Errorf("model catalog: default %s/%s effort %q exceeds runtime maximum %q", value.RuntimeID, value.Tier, value.Effort, runtime.MaximumEffort)
		}
		if value.MaxEffort != "" && !effortAtMost(value.MaxEffort, runtime.MaximumEffort) {
			return fmt.Errorf("model catalog: default %s/%s max_effort %q exceeds runtime maximum %q", value.RuntimeID, value.Tier, value.MaxEffort, runtime.MaximumEffort)
		}
	}
	for _, value := range pricing {
		if !modelSet[value.ModelID] {
			return fmt.Errorf("model catalog: price references unknown model %q", value.ModelID)
		}
	}
	return nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func clonePrice(value Price) Price {
	clone := value
	clone.Input = cloneRate(value.Input)
	clone.Output = cloneRate(value.Output)
	clone.CacheRead = cloneRate(value.CacheRead)
	clone.CacheWrite = cloneRate(value.CacheWrite)
	clone.CacheWrite5m = cloneRate(value.CacheWrite5m)
	clone.CacheWrite1h = cloneRate(value.CacheWrite1h)
	return clone
}

func cloneRate(value *float64) *float64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func validEffort(value string) bool {
	switch value {
	case "low", "medium", "high", "xhigh", "max":
		return true
	default:
		return false
	}
}

func effortAtMost(value, maximum string) bool {
	return effortRank(value) <= effortRank(maximum)
}

func effortRank(value string) int {
	switch value {
	case "low":
		return 1
	case "medium":
		return 2
	case "high":
		return 3
	case "xhigh":
		return 4
	case "max":
		return 5
	default:
		return 0
	}
}
