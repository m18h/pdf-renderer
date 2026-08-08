package fonts

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testConfig wires every seam to a temp dir so no test touches the real
// fontconfig, environment or fc-cache binary.
type testConfig struct {
	cfg        Config
	fcCalls    []string
	envSet     map[string]string
	fcCacheErr error
}

func newTestConfig(t *testing.T, fontDir string) *testConfig {
	t.Helper()
	tc := &testConfig{envSet: map[string]string{}}
	tc.cfg = Config{
		Dir:          fontDir,
		Logger:       quietLogger(),
		systemConfig: "/etc/fonts/fonts.conf",
		scanRoots:    []string{"/usr/share/fonts", "/usr/local/share/fonts"},
		configHome:   t.TempDir(),
		runFcCache: func(_ context.Context, dir string) error {
			tc.fcCalls = append(tc.fcCalls, dir)
			return tc.fcCacheErr
		},
		setenv: func(k, v string) error {
			tc.envSet[k] = v
			return nil
		},
	}
	return tc
}

func writeFile(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("not a real font"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A missing directory is the normal case — nothing was mounted.
func TestPrepareMissingDirIsNotAnError(t *testing.T) {
	tc := newTestConfig(t, filepath.Join(t.TempDir(), "does-not-exist"))

	res, err := Prepare(context.Background(), tc.cfg)
	if err != nil {
		t.Fatalf("Prepare() error = %v, want nil", err)
	}
	if res.Usable != 0 {
		t.Errorf("Usable = %d, want 0", res.Usable)
	}
	if len(tc.fcCalls) != 0 {
		t.Errorf("fc-cache ran %d times, want 0", len(tc.fcCalls))
	}
}

func TestPrepareEmptyDirDoesNothing(t *testing.T) {
	tc := newTestConfig(t, t.TempDir())

	res, err := Prepare(context.Background(), tc.cfg)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if res.Usable != 0 {
		t.Errorf("Usable = %d, want 0", res.Usable)
	}
	if len(tc.fcCalls) != 0 {
		t.Error("fc-cache ran for an empty directory")
	}
	if _, ok := tc.envSet["FONTCONFIG_FILE"]; ok {
		t.Error("FONTCONFIG_FILE was set for an empty directory")
	}
}

func TestPrepareCountsLoadableFonts(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{
		"Brand-Regular.ttf", "Brand-Bold.otf", "Legacy.pfb",
		"Collection.ttc", "Bitmap.pcf", "UPPERCASE.TTF",
		"nested/Deep-Italic.ttf",
		"README.md", "license.txt", ".DS_Store",
	} {
		writeFile(t, dir, n)
	}
	tc := newTestConfig(t, dir)

	res, err := Prepare(context.Background(), tc.cfg)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if res.Usable != 7 {
		t.Errorf("Usable = %d, want 7 (nested and mixed-case included, docs excluded)", res.Usable)
	}
	if len(tc.fcCalls) != 1 || tc.fcCalls[0] != dir {
		t.Errorf("fc-cache calls = %v, want exactly one for %q", tc.fcCalls, dir)
	}
}

// Web formats are a silent no-op in fontconfig, so they must be reported rather
// than quietly ignored.
func TestPrepareReportsWebOnlyFonts(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Brand.ttf")
	writeFile(t, dir, "Brand.woff")
	writeFile(t, dir, "Brand.woff2")
	writeFile(t, dir, "Brand.eot")
	tc := newTestConfig(t, dir)

	res, err := Prepare(context.Background(), tc.cfg)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if res.Usable != 1 {
		t.Errorf("Usable = %d, want 1", res.Usable)
	}
	if res.WebOnly != 3 {
		t.Errorf("WebOnly = %d, want 3", res.WebOnly)
	}
}

// A directory fontconfig already scans needs no generated config at all.
func TestPrepareUnderScanRootNeedsNoConfig(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "custom")
	writeFile(t, dir, "Brand.ttf")

	tc := newTestConfig(t, dir)
	tc.cfg.scanRoots = []string{root}

	res, err := Prepare(context.Background(), tc.cfg)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if res.ConfigPath != "" {
		t.Errorf("ConfigPath = %q, want empty: the directory is already scanned", res.ConfigPath)
	}
	if _, ok := tc.envSet["FONTCONFIG_FILE"]; ok {
		t.Error("FONTCONFIG_FILE was set unnecessarily")
	}
	if len(tc.fcCalls) != 1 {
		t.Error("fc-cache should still run to pick up the new files")
	}
}

func TestPrepareOutsideScanRootWritesConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Brand.ttf")
	tc := newTestConfig(t, dir)

	res, err := Prepare(context.Background(), tc.cfg)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if res.ConfigPath == "" {
		t.Fatal("ConfigPath is empty; a directory outside the scan roots must be declared")
	}
	if got := tc.envSet["FONTCONFIG_FILE"]; got != res.ConfigPath {
		t.Errorf("FONTCONFIG_FILE = %q, want %q", got, res.ConfigPath)
	}

	body, err := os.ReadFile(res.ConfigPath)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "<dir>"+dir+"</dir>") {
		t.Errorf("generated config does not declare the font dir:\n%s", got)
	}
	// The system config must still be included or we would lose every bundled font.
	if !strings.Contains(got, "/etc/fonts/fonts.conf") {
		t.Errorf("generated config does not include the system config:\n%s", got)
	}
	if !strings.HasPrefix(got, `<?xml version="1.0"?>`) {
		t.Errorf("generated config is not an XML document:\n%s", got)
	}
}

// Paths are interpolated into XML, so metacharacters must not break the document.
func TestGeneratedConfigEscapesXML(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, `fonts & "co" <test>`)
	writeFile(t, dir, "Brand.ttf")
	tc := newTestConfig(t, dir)

	res, err := Prepare(context.Background(), tc.cfg)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	body, err := os.ReadFile(res.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if strings.Contains(got, `& "co" <test>`) {
		t.Errorf("path was interpolated unescaped:\n%s", got)
	}
	for _, want := range []string{"&amp;", "&lt;", "&gt;"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in the escaped output:\n%s", want, got)
		}
	}
}

// fc-cache failing is a slow startup, not a broken service: fontconfig falls
// back to scanning at runtime.
func TestPrepareToleratesFcCacheFailure(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Brand.ttf")
	tc := newTestConfig(t, dir)
	tc.fcCacheErr = errors.New("fc-cache: command not found")

	res, err := Prepare(context.Background(), tc.cfg)
	if err != nil {
		t.Fatalf("Prepare() error = %v, want nil: fc-cache failure must not be fatal", err)
	}
	if res.Usable != 1 {
		t.Errorf("Usable = %d, want 1", res.Usable)
	}
}

func TestPrepareRejectsNonDirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "afile.ttf")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tc := newTestConfig(t, file)

	if _, err := Prepare(context.Background(), tc.cfg); err == nil {
		t.Error("Prepare() error = nil, want a failure when the path is a file")
	}
}

func TestCoveredMatchesOnlyRealPrefixes(t *testing.T) {
	c := Config{scanRoots: []string{"/usr/share/fonts"}}

	tests := []struct {
		dir  string
		want bool
	}{
		{"/usr/share/fonts", true},
		{"/usr/share/fonts/custom", true},
		{"/usr/share/fonts/truetype/noto", true},
		// A sibling whose name merely starts with the root must not match.
		{"/usr/share/fonts-extra", false},
		{"/usr/local/share/fonts", false},
		{"/opt/fonts", false},
	}
	for _, tt := range tests {
		c.Dir = tt.dir
		if got := c.covered(); got != tt.want {
			t.Errorf("covered(%q) = %v, want %v", tt.dir, got, tt.want)
		}
	}
}

func TestDefaultDirIsAFontconfigScanRoot(t *testing.T) {
	c := Config{Dir: DefaultDir}
	c.withDefaults()
	if !c.covered() {
		t.Errorf("DefaultDir %q is not among the default scan roots %v; "+
			"a plain bind mount there would need extra configuration",
			DefaultDir, c.scanRoots)
	}
}
