package statepaths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/open-cli-collective/cli-common/statedir"
	"github.com/open-cli-collective/cli-common/statedirtest"
)

const (
	headSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	baseSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestEncodeDecode(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		contains []string
	}{
		{name: "unreserved", input: "AZaz09._-", contains: []string{"AZaz09._-"}},
		{name: "percent", input: "100%", contains: []string{"%25"}},
		{name: "space", input: "a b", contains: []string{"%20"}},
		{name: "slash", input: "a/b", contains: []string{"%2F"}},
		{name: "colon", input: "a:b", contains: []string{"%3A"}},
		{name: "backslash", input: `a\b`, contains: []string{"%5C"}},
		{name: "windows hostile", input: `"<|?*>`, contains: []string{"%22", "%3C", "%7C", "%3F", "%2A", "%3E"}},
		{name: "control", input: "line\nbreak", contains: []string{"%0A"}},
		{name: "non ascii", input: "café", contains: []string{"%C3%A9"}},
		{name: "emoji", input: "agent😀", contains: []string{"%F0%9F%98%80"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded := Encode(tt.input)
			for _, want := range tt.contains {
				if !strings.Contains(encoded, want) {
					t.Fatalf("Encode(%q) = %q, want it to contain %q", tt.input, encoded, want)
				}
			}
			if decoded := Decode(encoded); decoded != tt.input {
				t.Fatalf("Decode(Encode(%q)) = %q, want original", tt.input, decoded)
			}
		})
	}
}

func TestPRKeyAndResumeScope(t *testing.T) {
	prKey, err := PRKey("github.com", "open-cli", "repo/name", 12)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	if prKey != "github.com_open-cli_repo%2Fname_12" {
		t.Fatalf("PRKey = %q, want encoded segment key", prKey)
	}

	withUnderscore, err := PRKey("github", "open_cli", "repo_name", 12)
	if err != nil {
		t.Fatalf("PRKey with underscore: %v", err)
	}
	if withUnderscore != "github_open%5Fcli_repo%5Fname_12" {
		t.Fatalf("PRKey with underscore = %q, want underscore escaped inside segments", withUnderscore)
	}

	first, err := PRKey("a_b", "c", "d", 1)
	if err != nil {
		t.Fatalf("first PRKey: %v", err)
	}
	second, err := PRKey("a", "b_c", "d", 1)
	if err != nil {
		t.Fatalf("second PRKey: %v", err)
	}
	if first == second {
		t.Fatalf("PRKey collision: %q == %q", first, second)
	}

	scope, err := ResumeScope("work profile", "rian@example.com")
	if err != nil {
		t.Fatalf("ResumeScope: %v", err)
	}
	if scope != "work%20profile__rian%40example.com" {
		t.Fatalf("ResumeScope = %q, want encoded profile and identity", scope)
	}

	withUnderscoreScope, err := ResumeScope("work_profile", "monit_reviewer")
	if err != nil {
		t.Fatalf("ResumeScope with underscore: %v", err)
	}
	if withUnderscoreScope != "work%5Fprofile__monit%5Freviewer" {
		t.Fatalf("ResumeScope with underscore = %q, want underscore escaped inside segments", withUnderscoreScope)
	}

	firstScope, err := ResumeScope("a_", "b")
	if err != nil {
		t.Fatalf("first ResumeScope: %v", err)
	}
	secondScope, err := ResumeScope("a", "_b")
	if err != nil {
		t.Fatalf("second ResumeScope: %v", err)
	}
	if firstScope == secondScope {
		t.Fatalf("ResumeScope collision: %q == %q", firstScope, secondScope)
	}
}

func TestKeyHashUsesUnambiguousTupleFraming(t *testing.T) {
	one := KeyHash("ab", "c", "d", "e", "f")
	two := KeyHash("a", "bc", "d", "e", "f")
	again := KeyHash("ab", "c", "d", "e", "f")
	differentProfile := KeyHash("ab", "c", "d", "profile", "f")
	differentIdentity := KeyHash("ab", "c", "d", "e", "identity")

	if len(one) != 12 {
		t.Fatalf("KeyHash length = %d, want 12", len(one))
	}
	if one != strings.ToLower(one) {
		t.Fatalf("KeyHash = %q, want lowercase hex", one)
	}
	if one != again {
		t.Fatalf("KeyHash not stable: %q then %q", one, again)
	}
	if one == two {
		t.Fatalf("KeyHash boundary collision: %q == %q", one, two)
	}
	if one == differentProfile {
		t.Fatalf("KeyHash ignores profile: %q == %q", one, differentProfile)
	}
	if one == differentIdentity {
		t.Fatalf("KeyHash ignores posting identity: %q == %q", one, differentIdentity)
	}
}

func TestFormatAttempt(t *testing.T) {
	tests := []struct {
		input int
		want  string
	}{
		{input: 1, want: "001"},
		{input: 12, want: "012"},
		{input: 999, want: "999"},
		{input: 1000, want: "1000"},
	}

	for _, tt := range tests {
		got, err := FormatAttempt(tt.input)
		if err != nil {
			t.Fatalf("FormatAttempt(%d): %v", tt.input, err)
		}
		if got != tt.want {
			t.Fatalf("FormatAttempt(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}

	if _, err := FormatAttempt(0); err == nil {
		t.Fatal("FormatAttempt(0) error = nil, want error")
	}
}

func TestRunPaths(t *testing.T) {
	layout := NewLayout("data", "cache")
	spec := RunSpec{
		Host:            "github",
		Owner:           "open-cli",
		Repo:            "codereview-cli",
		PRNumber:        34,
		HeadSHA:         headSHA,
		BaseSHA:         baseSHA,
		Profile:         "work",
		PostingIdentity: "monit/reviewer",
		Attempt:         1,
	}

	paths, err := layout.Run(spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	prKey := "github_open-cli_codereview-cli_34"
	scope := "work__monit%2Freviewer"
	runDir := filepath.Join("data", "runs", prKey, headSHA, baseSHA, scope, "001")

	if got := layout.LedgerDB(); got != filepath.Join("data", "ledger.db") {
		t.Fatalf("LedgerDB = %q, want data ledger path", got)
	}
	if paths.Dir != runDir {
		t.Fatalf("Run dir = %q, want %q", paths.Dir, runDir)
	}
	if paths.DiffPatch != filepath.Join(runDir, "diff.patch") {
		t.Fatalf("DiffPatch = %q", paths.DiffPatch)
	}
	if paths.SlicesDir != filepath.Join(runDir, "slices") {
		t.Fatalf("SlicesDir = %q", paths.SlicesDir)
	}
	if paths.FindingsJSON != filepath.Join(runDir, "findings.json") {
		t.Fatalf("FindingsJSON = %q", paths.FindingsJSON)
	}
	if paths.RollupMarkdown != filepath.Join(runDir, "rollup.md") {
		t.Fatalf("RollupMarkdown = %q", paths.RollupMarkdown)
	}
	if paths.AgentLogsDir != filepath.Join(runDir, "agent-logs") {
		t.Fatalf("AgentLogsDir = %q", paths.AgentLogsDir)
	}
	slicePatch, err := paths.SlicePatch("harness:arch", "dir/file.go")
	if err != nil {
		t.Fatalf("SlicePatch: %v", err)
	}
	if slicePatch != filepath.Join(runDir, "slices", "harness%3Aarch", "dir%2Ffile.go.patch") {
		t.Fatalf("SlicePatch = %q", slicePatch)
	}
	agentLog, err := paths.AgentLog("harness:arch")
	if err != nil {
		t.Fatalf("AgentLog: %v", err)
	}
	if agentLog != filepath.Join(runDir, "agent-logs", "harness%3Aarch.jsonl") {
		t.Fatalf("AgentLog = %q", agentLog)
	}
	if paths.LockFile != filepath.Join("data", "locks", prKey+"__aaaaaaa__62574572babb.lock") {
		t.Fatalf("LockFile = %q", paths.LockFile)
	}
	lockFile, err := layout.LockFile(LockSpec{
		Host:            spec.Host,
		Owner:           spec.Owner,
		Repo:            spec.Repo,
		PRNumber:        spec.PRNumber,
		HeadSHA:         spec.HeadSHA,
		BaseSHA:         spec.BaseSHA,
		Profile:         spec.Profile,
		PostingIdentity: spec.PostingIdentity,
	})
	if err != nil {
		t.Fatalf("LockFile: %v", err)
	}
	if lockFile != paths.LockFile {
		t.Fatalf("LockFile() = %q, want Run().LockFile %q", lockFile, paths.LockFile)
	}
	if got := layout.HTTPCacheDir(); got != filepath.Join("cache", "http") {
		t.Fatalf("HTTPCacheDir = %q", got)
	}
}

func TestArtifactHelpersRejectEmptyComponents(t *testing.T) {
	paths := RunPaths{
		SlicesDir:    filepath.Join("run", "slices"),
		AgentLogsDir: filepath.Join("run", "agent-logs"),
	}

	if _, err := paths.SlicePatch("", "file.go"); err == nil {
		t.Fatal("SlicePatch empty agent error = nil, want error")
	}
	if _, err := paths.SlicePatch("agent", ""); err == nil {
		t.Fatal("SlicePatch empty file error = nil, want error")
	}
	if _, err := paths.AgentLog(""); err == nil {
		t.Fatal("AgentLog empty agent error = nil, want error")
	}
}

func TestRunSpecValidation(t *testing.T) {
	layout := NewLayout("data", "cache")
	valid := RunSpec{
		Host:            "github",
		Owner:           "open-cli",
		Repo:            "codereview-cli",
		PRNumber:        34,
		HeadSHA:         headSHA,
		BaseSHA:         baseSHA,
		Profile:         "work",
		PostingIdentity: "reviewer",
		Attempt:         1,
	}

	tests := []struct {
		name    string
		mutate  func(*RunSpec)
		wantErr string
	}{
		{name: "empty host", mutate: func(s *RunSpec) { s.Host = "" }, wantErr: "host"},
		{name: "empty owner", mutate: func(s *RunSpec) { s.Owner = "" }, wantErr: "owner"},
		{name: "empty repo", mutate: func(s *RunSpec) { s.Repo = "" }, wantErr: "repo"},
		{name: "empty profile", mutate: func(s *RunSpec) { s.Profile = "" }, wantErr: "profile"},
		{name: "empty identity", mutate: func(s *RunSpec) { s.PostingIdentity = "" }, wantErr: "posting identity"},
		{name: "bad pr number", mutate: func(s *RunSpec) { s.PRNumber = 0 }, wantErr: "PR number"},
		{name: "short head sha", mutate: func(s *RunSpec) { s.HeadSHA = strings.Repeat("a", 39) }, wantErr: "head SHA"},
		{name: "uppercase base sha", mutate: func(s *RunSpec) { s.BaseSHA = strings.Repeat("B", 40) }, wantErr: "base SHA"},
		{name: "bad attempt", mutate: func(s *RunSpec) { s.Attempt = 0 }, wantErr: "attempt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := valid
			tt.mutate(&spec)
			_, err := layout.Run(spec)
			if err == nil {
				t.Fatalf("Run() error = nil, want substring %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Run() error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestLockSpecValidation(t *testing.T) {
	layout := NewLayout("data", "cache")
	valid := LockSpec{
		Host:            "github",
		Owner:           "open-cli",
		Repo:            "codereview-cli",
		PRNumber:        34,
		HeadSHA:         headSHA,
		BaseSHA:         baseSHA,
		Profile:         "work",
		PostingIdentity: "reviewer",
	}

	tests := []struct {
		name    string
		mutate  func(*LockSpec)
		wantErr string
	}{
		{name: "empty host", mutate: func(s *LockSpec) { s.Host = "" }, wantErr: "host"},
		{name: "empty owner", mutate: func(s *LockSpec) { s.Owner = "" }, wantErr: "owner"},
		{name: "empty repo", mutate: func(s *LockSpec) { s.Repo = "" }, wantErr: "repo"},
		{name: "empty profile", mutate: func(s *LockSpec) { s.Profile = "" }, wantErr: "profile"},
		{name: "empty identity", mutate: func(s *LockSpec) { s.PostingIdentity = "" }, wantErr: "posting identity"},
		{name: "bad pr number", mutate: func(s *LockSpec) { s.PRNumber = 0 }, wantErr: "PR number"},
		{name: "short head sha", mutate: func(s *LockSpec) { s.HeadSHA = strings.Repeat("a", 39) }, wantErr: "head SHA"},
		{name: "uppercase base sha", mutate: func(s *LockSpec) { s.BaseSHA = strings.Repeat("B", 40) }, wantErr: "base SHA"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := valid
			tt.mutate(&spec)
			_, err := layout.LockFile(spec)
			if err == nil {
				t.Fatalf("LockFile() error = nil, want substring %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("LockFile() error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestRootsAreHermeticAndEnsured(t *testing.T) {
	root := statedirtest.Hermetic(t)

	dataRoot, err := DataRoot()
	if err != nil {
		t.Fatalf("DataRoot: %v", err)
	}
	cacheRoot, err := CacheRoot()
	if err != nil {
		t.Fatalf("CacheRoot: %v", err)
	}
	if !strings.HasPrefix(dataRoot, root) {
		t.Fatalf("DataRoot = %q, want under hermetic root %q", dataRoot, root)
	}
	if !strings.HasPrefix(cacheRoot, root) {
		t.Fatalf("CacheRoot = %q, want under hermetic root %q", cacheRoot, root)
	}
	if dataRoot == cacheRoot {
		t.Fatalf("DataRoot and CacheRoot collide at %q", dataRoot)
	}
	expectedData, err := (statedir.Data{Tool: Tool}).DataDir()
	if err != nil {
		t.Fatalf("expected DataDir: %v", err)
	}
	expectedCache, err := (statedir.Cache{Tool: Tool}).CacheDir()
	if err != nil {
		t.Fatalf("expected CacheDir: %v", err)
	}
	if dataRoot != expectedData {
		t.Fatalf("DataRoot = %q, want shared resolver root %q", dataRoot, expectedData)
	}
	if cacheRoot != expectedCache {
		t.Fatalf("CacheRoot = %q, want shared resolver root %q", cacheRoot, expectedCache)
	}
	if filepath.Base(dataRoot) == AppDir || filepath.Base(cacheRoot) == AppDir {
		t.Fatalf("roots must not use legacy app dir suffix: data=%q cache=%q", dataRoot, cacheRoot)
	}
	if _, err := os.Stat(dataRoot); !os.IsNotExist(err) {
		t.Fatalf("DataRoot must not create app root; stat err = %v", err)
	}
	if _, err := os.Stat(cacheRoot); !os.IsNotExist(err) {
		t.Fatalf("CacheRoot must not create app root; stat err = %v", err)
	}

	ensuredData, err := DataRootEnsured()
	if err != nil {
		t.Fatalf("DataRootEnsured: %v", err)
	}
	ensuredCache, err := CacheRootEnsured()
	if err != nil {
		t.Fatalf("CacheRootEnsured: %v", err)
	}
	if ensuredData != dataRoot {
		t.Fatalf("DataRootEnsured = %q, want %q", ensuredData, dataRoot)
	}
	if ensuredCache != cacheRoot {
		t.Fatalf("CacheRootEnsured = %q, want %q", ensuredCache, cacheRoot)
	}
	assertDir0700(t, ensuredData)
	assertDir0700(t, ensuredCache)
}

func TestDefaultLayoutEnsuredDoesNotMigrateLegacyRoot(t *testing.T) {
	statedirtest.Hermetic(t)
	dataRoot, err := DataRoot()
	if err != nil {
		t.Fatalf("DataRoot: %v", err)
	}
	legacyFile := filepath.Join(dataRoot, AppDir, "ledger.db")
	writeStatepathsFile(t, legacyFile, "legacy")

	layout, err := DefaultLayoutEnsured()
	if err != nil {
		t.Fatalf("DefaultLayoutEnsured: %v", err)
	}
	if layout.DataRoot != dataRoot {
		t.Fatalf("layout data root = %q, want %q", layout.DataRoot, dataRoot)
	}
	assertFileContents(t, legacyFile, "legacy")
	if _, err := os.Stat(filepath.Join(dataRoot, "ledger.db")); !os.IsNotExist(err) {
		t.Fatalf("new ledger stat err = %v, want missing because generic ensure must not migrate", err)
	}
}

func TestLegacyDataRootExistsIncludesStagedRoot(t *testing.T) {
	statedirtest.Hermetic(t)
	layout, err := DefaultLayoutEnsured()
	if err != nil {
		t.Fatalf("DefaultLayoutEnsured: %v", err)
	}
	exists, err := LegacyDataRootExists(layout)
	if err != nil {
		t.Fatalf("initial LegacyDataRootExists: %v", err)
	}
	if exists {
		t.Fatal("initial LegacyDataRootExists = true, want false")
	}

	stagedRoot := LegacyDataRoot(layout) + ".migrating"
	writeStatepathsFile(t, filepath.Join(stagedRoot, "runs", "sentinel.txt"), "staged")
	exists, err = LegacyDataRootExists(layout)
	if err != nil {
		t.Fatalf("staged LegacyDataRootExists: %v", err)
	}
	if !exists {
		t.Fatal("staged LegacyDataRootExists = false, want true")
	}

	writeStatepathsFile(t, filepath.Join(LegacyDataRoot(layout), "ledger.db"), "legacy")
	exists, err = LegacyDataRootExists(layout)
	if err != nil {
		t.Fatalf("both-root LegacyDataRootExists: %v", err)
	}
	if !exists {
		t.Fatal("both-root LegacyDataRootExists = false, want true")
	}
}

func TestMigrateLegacyDataRootMovesEntries(t *testing.T) {
	statedirtest.Hermetic(t)
	layout, err := DefaultLayoutEnsured()
	if err != nil {
		t.Fatalf("DefaultLayoutEnsured: %v", err)
	}
	legacyLedger := filepath.Join(layout.DataRoot, AppDir, "ledger.db")
	legacyRun := filepath.Join(layout.DataRoot, AppDir, "runs", "sentinel.txt")
	writeStatepathsFile(t, legacyLedger, "ledger")
	writeStatepathsFile(t, legacyRun, "run")

	if err := MigrateLegacyDataRoot(layout); err != nil {
		t.Fatalf("MigrateLegacyDataRoot: %v", err)
	}
	assertFileContents(t, filepath.Join(layout.DataRoot, "ledger.db"), "ledger")
	assertFileContents(t, filepath.Join(layout.DataRoot, "runs", "sentinel.txt"), "run")
	if _, err := os.Stat(filepath.Join(layout.DataRoot, AppDir)); !os.IsNotExist(err) {
		t.Fatalf("legacy data root stat err = %v, want removed", err)
	}
}

func TestMigrateLegacyCacheRootMovesEntries(t *testing.T) {
	statedirtest.Hermetic(t)
	layout, err := DefaultLayoutEnsured()
	if err != nil {
		t.Fatalf("DefaultLayoutEnsured: %v", err)
	}
	legacyHTTP := filepath.Join(layout.CacheRoot, AppDir, "http", "sentinel.txt")
	writeStatepathsFile(t, legacyHTTP, "cache")

	if err := MigrateLegacyCacheRoot(layout); err != nil {
		t.Fatalf("MigrateLegacyCacheRoot: %v", err)
	}
	assertFileContents(t, filepath.Join(layout.CacheRoot, "http", "sentinel.txt"), "cache")
	if _, err := os.Stat(filepath.Join(layout.CacheRoot, AppDir)); !os.IsNotExist(err) {
		t.Fatalf("legacy cache root stat err = %v, want removed", err)
	}
}

func TestMigrateLegacyCacheRootRefusesConflictsWithoutPartialMove(t *testing.T) {
	statedirtest.Hermetic(t)
	layout, err := DefaultLayoutEnsured()
	if err != nil {
		t.Fatalf("DefaultLayoutEnsured: %v", err)
	}
	legacyHTTP := filepath.Join(layout.CacheRoot, AppDir, "http", "legacy.txt")
	legacyOther := filepath.Join(layout.CacheRoot, AppDir, "other", "legacy.txt")
	writeStatepathsFile(t, legacyHTTP, "legacy-http")
	writeStatepathsFile(t, legacyOther, "legacy-other")
	writeStatepathsFile(t, filepath.Join(layout.CacheRoot, "http", "new.txt"), "new-http")

	err = MigrateLegacyCacheRoot(layout)
	if err == nil {
		t.Fatal("MigrateLegacyCacheRoot error = nil, want conflict")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("MigrateLegacyCacheRoot error = %v, want conflict context", err)
	}
	stagedRoot := filepath.Join(layout.CacheRoot, AppDir+".migrating")
	assertFileContents(t, filepath.Join(stagedRoot, "http", "legacy.txt"), "legacy-http")
	assertFileContents(t, filepath.Join(stagedRoot, "other", "legacy.txt"), "legacy-other")
	assertFileContents(t, filepath.Join(layout.CacheRoot, "http", "new.txt"), "new-http")
	if _, err := os.Stat(filepath.Join(layout.CacheRoot, "other")); !os.IsNotExist(err) {
		t.Fatalf("new other stat err = %v, want missing because migration must be all-or-nothing on conflict", err)
	}
}

func TestMigrateLegacyCacheRootIsIdempotent(t *testing.T) {
	statedirtest.Hermetic(t)
	layout, err := DefaultLayoutEnsured()
	if err != nil {
		t.Fatalf("DefaultLayoutEnsured: %v", err)
	}
	legacyHTTP := filepath.Join(layout.CacheRoot, AppDir, "http", "sentinel.txt")
	writeStatepathsFile(t, legacyHTTP, "cache")

	if err := MigrateLegacyCacheRoot(layout); err != nil {
		t.Fatalf("first MigrateLegacyCacheRoot: %v", err)
	}
	if err := MigrateLegacyCacheRoot(layout); err != nil {
		t.Fatalf("second MigrateLegacyCacheRoot: %v", err)
	}
	assertFileContents(t, filepath.Join(layout.CacheRoot, "http", "sentinel.txt"), "cache")
}

func TestMigrateLegacyRootResumesStagedMigration(t *testing.T) {
	statedirtest.Hermetic(t)
	layout, err := DefaultLayoutEnsured()
	if err != nil {
		t.Fatalf("DefaultLayoutEnsured: %v", err)
	}
	stagedRoot := filepath.Join(layout.DataRoot, AppDir+".migrating")
	writeStatepathsFile(t, filepath.Join(layout.DataRoot, "ledger.db"), "already-moved")
	writeStatepathsFile(t, filepath.Join(stagedRoot, "runs", "sentinel.txt"), "remaining")

	if err := MigrateLegacyDataRoot(layout); err != nil {
		t.Fatalf("MigrateLegacyDataRoot: %v", err)
	}
	assertFileContents(t, filepath.Join(layout.DataRoot, "ledger.db"), "already-moved")
	assertFileContents(t, filepath.Join(layout.DataRoot, "runs", "sentinel.txt"), "remaining")
	if _, err := os.Stat(stagedRoot); !os.IsNotExist(err) {
		t.Fatalf("staged root stat err = %v, want removed", err)
	}
}

func TestMigrateLegacyRootRefusesConflictsWithoutPartialMove(t *testing.T) {
	statedirtest.Hermetic(t)
	layout, err := DefaultLayoutEnsured()
	if err != nil {
		t.Fatalf("DefaultLayoutEnsured: %v", err)
	}
	legacyLedger := filepath.Join(layout.DataRoot, AppDir, "ledger.db")
	legacyRuns := filepath.Join(layout.DataRoot, AppDir, "runs", "sentinel.txt")
	writeStatepathsFile(t, legacyLedger, "legacy-ledger")
	writeStatepathsFile(t, legacyRuns, "legacy-run")
	writeStatepathsFile(t, filepath.Join(layout.DataRoot, "ledger.db"), "new-ledger")

	err = MigrateLegacyDataRoot(layout)
	if err == nil {
		t.Fatal("MigrateLegacyDataRoot error = nil, want conflict")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("MigrateLegacyDataRoot error = %v, want conflict context", err)
	}
	stagedRoot := filepath.Join(layout.DataRoot, AppDir+".migrating")
	assertFileContents(t, filepath.Join(stagedRoot, "ledger.db"), "legacy-ledger")
	assertFileContents(t, filepath.Join(stagedRoot, "runs", "sentinel.txt"), "legacy-run")
	assertFileContents(t, filepath.Join(layout.DataRoot, "ledger.db"), "new-ledger")
	if _, err := os.Stat(filepath.Join(layout.DataRoot, "runs")); !os.IsNotExist(err) {
		t.Fatalf("new runs stat err = %v, want missing because migration must be all-or-nothing on conflict", err)
	}
}

func TestMigrateLegacyRootIsIdempotent(t *testing.T) {
	statedirtest.Hermetic(t)
	layout, err := DefaultLayoutEnsured()
	if err != nil {
		t.Fatalf("DefaultLayoutEnsured: %v", err)
	}
	legacyLedger := filepath.Join(layout.DataRoot, AppDir, "ledger.db")
	writeStatepathsFile(t, legacyLedger, "ledger")

	if err := MigrateLegacyDataRoot(layout); err != nil {
		t.Fatalf("first MigrateLegacyDataRoot: %v", err)
	}
	if err := MigrateLegacyDataRoot(layout); err != nil {
		t.Fatalf("second MigrateLegacyDataRoot: %v", err)
	}
	assertFileContents(t, filepath.Join(layout.DataRoot, "ledger.db"), "ledger")
}

func writeStatepathsFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	// #nosec G304 -- test paths are controlled by statedirtest.Hermetic/t.TempDir.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func assertDir0700(t *testing.T, dir string) {
	t.Helper()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Fatalf("%s mode = %o, want 0700", dir, perm)
		}
	}
}
