package main

import (
	"testing"
	"time"
)

// envLookup builds a lookup func from a map, so config tests don't touch the
// real process environment.
func envLookup(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	c, err := loadConfig(envLookup(nil))
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}

	// 8080, not 80: the container runs as a non-root user which cannot bind 80.
	if c.Port != "8080" {
		t.Errorf("Port = %q, want %q", c.Port, "8080")
	}
	if c.ExecPath != "/headless-shell/headless-shell" {
		t.Errorf("ExecPath = %q", c.ExecPath)
	}
	if c.NoSandbox {
		t.Error("NoSandbox = true, want false: the sandbox must be on by default")
	}
	if c.MaxRenders != 500 {
		t.Errorf("MaxRenders = %d, want 500", c.MaxRenders)
	}
	if c.MaxAge != 30*time.Minute {
		t.Errorf("MaxAge = %v, want 30m", c.MaxAge)
	}
	if c.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want %q", c.LogFormat, "json")
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	c, err := loadConfig(envLookup(map[string]string{
		"PORT":                      "9000",
		"PDFRENDER_EXEC_PATH":       "/usr/bin/chromium",
		"PDFRENDER_NO_SANDBOX":      "1",
		"PDFRENDER_MAX_CONCURRENT":  "4",
		"PDFRENDER_MAX_RENDERS":     "100",
		"PDFRENDER_ACQUIRE_TIMEOUT": "2s",
		"PDFRENDER_MAX_AGE":         "10m",
		"PDFRENDER_RENDER_TIMEOUT":  "45s",
		"PDFRENDER_READY_TIMEOUT":   "5s",
		"PDFRENDER_MAX_BODY_BYTES":  "2048",
		"PDFRENDER_LOG_FORMAT":      "text",
	}))
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}

	if c.Port != "9000" {
		t.Errorf("Port = %q, want %q", c.Port, "9000")
	}
	if c.ExecPath != "/usr/bin/chromium" {
		t.Errorf("ExecPath = %q", c.ExecPath)
	}
	if !c.NoSandbox {
		t.Error("NoSandbox = false, want true")
	}
	if c.MaxConcurrent != 4 {
		t.Errorf("MaxConcurrent = %d, want 4", c.MaxConcurrent)
	}
	if c.AcquireTimeout != 2*time.Second {
		t.Errorf("AcquireTimeout = %v, want 2s", c.AcquireTimeout)
	}
	if c.MaxAge != 10*time.Minute {
		t.Errorf("MaxAge = %v, want 10m", c.MaxAge)
	}
	if c.RenderTimeout != 45*time.Second {
		t.Errorf("RenderTimeout = %v, want 45s", c.RenderTimeout)
	}
	if c.ReadyTimeout != 5*time.Second {
		t.Errorf("ReadyTimeout = %v, want 5s", c.ReadyTimeout)
	}
	if c.MaxBodyBytes != 2048 {
		t.Errorf("MaxBodyBytes = %d, want 2048", c.MaxBodyBytes)
	}
	if c.LogFormat != "text" {
		t.Errorf("LogFormat = %q, want %q", c.LogFormat, "text")
	}
}

func TestLoadConfigNoSandboxAcceptsTrue(t *testing.T) {
	for _, v := range []string{"1", "true"} {
		c, err := loadConfig(envLookup(map[string]string{"PDFRENDER_NO_SANDBOX": v}))
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		if !c.NoSandbox {
			t.Errorf("PDFRENDER_NO_SANDBOX=%q gave NoSandbox=false", v)
		}
	}
	// Anything else must leave the sandbox on — failing closed is the safe default.
	for _, v := range []string{"0", "false", "yes", "maybe", ""} {
		c, err := loadConfig(envLookup(map[string]string{"PDFRENDER_NO_SANDBOX": v}))
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		if c.NoSandbox {
			t.Errorf("PDFRENDER_NO_SANDBOX=%q gave NoSandbox=true; must fail closed", v)
		}
	}
}

// A bad value must fail loudly rather than silently falling back to a default —
// a typo'd PORT should not quietly serve on the wrong one.
func TestLoadConfigRejectsBadValues(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{"non-numeric port", map[string]string{"PORT": "http"}},
		{"port zero", map[string]string{"PORT": "0"}},
		{"port too high", map[string]string{"PORT": "70000"}},
		{"negative port", map[string]string{"PORT": "-1"}},
		{"bad max concurrent", map[string]string{"PDFRENDER_MAX_CONCURRENT": "lots"}},
		{"negative max concurrent", map[string]string{"PDFRENDER_MAX_CONCURRENT": "-2"}},
		{"bad duration", map[string]string{"PDFRENDER_RENDER_TIMEOUT": "soon"}},
		{"negative duration", map[string]string{"PDFRENDER_MAX_AGE": "-5m"}},
		{"bad body limit", map[string]string{"PDFRENDER_MAX_BODY_BYTES": "10MB"}},
		{"zero body limit", map[string]string{"PDFRENDER_MAX_BODY_BYTES": "0"}},
		{"bad log format", map[string]string{"PDFRENDER_LOG_FORMAT": "xml"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := loadConfig(envLookup(tt.env)); err == nil {
				t.Errorf("loadConfig(%v) error = nil, want a failure", tt.env)
			}
		})
	}
}

// An empty value is treated as unset rather than as an error.
func TestLoadConfigEmptyValuesFallBackToDefaults(t *testing.T) {
	c, err := loadConfig(envLookup(map[string]string{
		"PORT":                     "",
		"PDFRENDER_EXEC_PATH":      "",
		"PDFRENDER_RENDER_TIMEOUT": "",
	}))
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if c.Port != "8080" {
		t.Errorf("Port = %q, want the default 8080", c.Port)
	}
}

func TestLoadConfigFontsDir(t *testing.T) {
	// Default is the fontconfig-scanned mount point.
	c, err := loadConfig(envLookup(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.FontsDir != "/usr/local/share/fonts" {
		t.Errorf("FontsDir = %q, want /usr/local/share/fonts", c.FontsDir)
	}

	c, err = loadConfig(envLookup(map[string]string{"PDFRENDER_FONTS_DIR": "/opt/brand-fonts"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.FontsDir != "/opt/brand-fonts" {
		t.Errorf("FontsDir = %q, want /opt/brand-fonts", c.FontsDir)
	}

	// An explicit empty value opts out of the scan entirely, unlike the other
	// settings where empty means "unset, use the default".
	c, err = loadConfig(envLookup(map[string]string{"PDFRENDER_FONTS_DIR": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if c.FontsDir != "" {
		t.Errorf("FontsDir = %q, want empty to disable the font scan", c.FontsDir)
	}
}
