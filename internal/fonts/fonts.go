// Package fonts makes a mounted directory of extra fonts visible to Chromium.
//
// The image bundles Liberation and Noto, which covers Latin, Cyrillic, Greek,
// CJK and emoji — but not brand or licensed faces. Rather than require a rebuild
// to add one, a directory of font files can be mounted at runtime and this
// package wires it into fontconfig before the browser starts.
package fonts

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DefaultDir is where fonts should be mounted. It is one of the paths
// fontconfig already scans (see /etc/fonts/fonts.conf), so a plain bind mount
// there needs no configuration at all, and it is the FHS location for locally
// added fonts so it never collides with distro-managed ones.
const DefaultDir = "/usr/local/share/fonts"

// fontExts are the formats fontconfig can actually load.
var fontExts = map[string]bool{
	".ttf": true, ".ttc": true, ".otf": true, ".otc": true,
	".pfa": true, ".pfb": true, ".pcf": true, ".bdf": true, ".dfont": true,
}

// webOnlyExts are common web formats fontconfig ignores. Mounting these is a
// silent no-op, so it is worth saying so out loud.
var webOnlyExts = map[string]bool{".woff": true, ".woff2": true, ".eot": true, ".svg": true}

// Config describes where to find extra fonts and how to register them.
type Config struct {
	// Dir holds the font files. A missing or empty directory is not an error.
	Dir    string
	Logger *slog.Logger

	// Test seams.
	systemConfig string   // usually /etc/fonts/fonts.conf
	scanRoots    []string // paths fontconfig already scans
	configHome   string   // XDG_CONFIG_HOME
	runFcCache   func(ctx context.Context, dir string) error
	setenv       func(key, value string) error
}

// Result reports what Prepare found.
type Result struct {
	// Usable counts font files fontconfig can load.
	Usable int
	// WebOnly counts .woff/.woff2/.eot files, which fontconfig ignores.
	WebOnly int
	// ConfigPath is the generated fontconfig file, empty when none was needed.
	ConfigPath string
}

func (c *Config) withDefaults() {
	if c.Dir == "" {
		c.Dir = DefaultDir
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.systemConfig == "" {
		c.systemConfig = "/etc/fonts/fonts.conf"
	}
	if c.scanRoots == nil {
		// Kept in sync with /etc/fonts/fonts.conf in the runtime image. Note that
		// this Debian's fonts.conf has no XDG include, so a user-level
		// fontconfig file is NOT picked up automatically — hence FONTCONFIG_FILE
		// below for directories outside these roots.
		c.scanRoots = []string{"/usr/share/fonts", "/usr/local/share/fonts"}
		if home, err := os.UserHomeDir(); err == nil {
			c.scanRoots = append(c.scanRoots, filepath.Join(home, ".fonts"))
		}
	}
	if c.configHome == "" {
		c.configHome = os.Getenv("XDG_CONFIG_HOME")
		if c.configHome == "" {
			if home, err := os.UserHomeDir(); err == nil {
				c.configHome = filepath.Join(home, ".config")
			}
		}
	}
	if c.runFcCache == nil {
		c.runFcCache = fcCache
	}
	if c.setenv == nil {
		c.setenv = os.Setenv
	}
}

// Prepare registers Dir with fontconfig and rebuilds the font cache.
//
// It must run before the browser is launched: Chromium reads fontconfig once at
// startup and inherits our environment, so a later change would not be seen.
//
// A missing or empty directory is the normal case and is not an error.
func Prepare(ctx context.Context, cfg Config) (Result, error) {
	cfg.withDefaults()
	var res Result

	info, err := os.Stat(cfg.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		cfg.Logger.Debug("no extra font directory", "dir", cfg.Dir)
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("stat font dir %s: %w", cfg.Dir, err)
	}
	if !info.IsDir() {
		return res, fmt.Errorf("font path %s is not a directory", cfg.Dir)
	}

	res.Usable, res.WebOnly, err = countFonts(cfg.Dir)
	if err != nil {
		return res, fmt.Errorf("scan font dir %s: %w", cfg.Dir, err)
	}

	if res.WebOnly > 0 {
		cfg.Logger.Warn("ignoring web font files: fontconfig cannot load them, "+
			"convert to .ttf or .otf",
			"dir", cfg.Dir, "count", res.WebOnly)
	}
	if res.Usable == 0 {
		cfg.Logger.Debug("extra font directory is empty", "dir", cfg.Dir)
		return res, nil
	}

	// Directories outside fontconfig's default roots need to be declared. The
	// generated file includes the system config so nothing is lost.
	if !cfg.covered() {
		path, err := cfg.writeFontconfig()
		if err != nil {
			return res, err
		}
		if err := cfg.setenv("FONTCONFIG_FILE", path); err != nil {
			return res, fmt.Errorf("set FONTCONFIG_FILE: %w", err)
		}
		res.ConfigPath = path
		cfg.Logger.Info("registered font directory with fontconfig",
			"dir", cfg.Dir, "config", path)
	}

	if err := cfg.runFcCache(ctx, cfg.Dir); err != nil {
		// Not fatal: fontconfig falls back to scanning at startup, which is
		// slower but still finds the fonts.
		cfg.Logger.Warn("fc-cache failed; fonts should still load but startup "+
			"will be slower", "error", err)
	}

	cfg.Logger.Info("loaded extra fonts", "dir", cfg.Dir, "count", res.Usable)
	return res, nil
}

// covered reports whether Dir already sits under a path fontconfig scans.
func (c *Config) covered() bool {
	abs, err := filepath.Abs(c.Dir)
	if err != nil {
		return false
	}
	for _, root := range c.scanRoots {
		rootAbs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		if abs == rootAbs || strings.HasPrefix(abs, rootAbs+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

const fontconfigTemplate = `<?xml version="1.0"?>
<!DOCTYPE fontconfig SYSTEM "urn:fontconfig:fonts.dtd">
<!-- Generated by pdf-renderer. Do not edit; it is rewritten at every start. -->
<fontconfig>
  <include ignore_missing="yes">%s</include>
  <dir>%s</dir>
</fontconfig>
`

func (c *Config) writeFontconfig() (string, error) {
	if c.configHome == "" {
		return "", errors.New("cannot register fonts: no writable config directory " +
			"(set XDG_CONFIG_HOME, or mount fonts under " + DefaultDir + ")")
	}
	// Chromium is launched as a child of this process and so runs as the same
	// user; nothing else needs to read these.
	dir := filepath.Join(c.configHome, "fontconfig")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}

	abs, err := filepath.Abs(c.Dir)
	if err != nil {
		return "", err
	}
	body := fmt.Sprintf(fontconfigTemplate, escapeXML(c.systemConfig), escapeXML(abs))

	path := filepath.Join(dir, "fonts.conf")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// escapeXML escapes the characters that matter inside an element's text.
func escapeXML(s string) string {
	return strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;",
	).Replace(s)
}

func countFonts(dir string) (usable, webOnly int, err error) {
	err = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			// A single unreadable entry should not abort the scan.
			return nil //nolint:nilerr // tolerate unreadable entries
		}
		if d.IsDir() {
			return nil
		}
		switch ext := strings.ToLower(filepath.Ext(d.Name())); {
		case fontExts[ext]:
			usable++
		case webOnlyExts[ext]:
			webOnly++
		}
		return nil
	})
	return usable, webOnly, err
}

func fcCache(ctx context.Context, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// -f forces a rebuild; without it fontconfig trusts an existing cache and
	// would not notice a directory that appeared after the image was built.
	// dir is operator-supplied (PDFRENDER_FONTS_DIR), never caller-supplied, and
	// is passed as its own argv element rather than through a shell, so there is
	// nothing to inject: the worst case is scanning a directory the operator named.
	cmd := exec.CommandContext(ctx, "fc-cache", "-f", dir) //nolint:gosec // G204: see above
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("fc-cache: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
