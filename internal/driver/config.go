package driver

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Rendered file and directory modes (contracts/zwave-server-sidecar.md §2).
const (
	configFileMode = 0o600
	configDirMode  = 0o700
)

// ConfigSpec is what the rendered CommonJS file carries.
type ConfigSpec struct {
	// CacheDir is storage.cacheDir, the zwave-js network cache.
	CacheDir string
	// LogLevel is logConfig.level as zwave-js spells it.
	LogLevel string
	// Keys maps a classic security class to its 16 raw key bytes; Long Range
	// maps the two Long Range classes. A class that is absent is omitted from
	// the file rather than written empty, and a spec with neither map renders
	// no key section at all. Both maps use the same property names
	// (contracts/zwave-server-sidecar.md §2).
	Keys      map[string][]byte
	LongRange map[string][]byte
}

// classicKeyClasses and longRangeKeyClasses fix the property order of the
// rendered file, so the same keys always render the same bytes.
var (
	classicKeyClasses   = []string{"S2_AccessControl", "S2_Authenticated", "S2_Unauthenticated", "S0_Legacy"}
	longRangeKeyClasses = []string{"S2_Authenticated_LR", "S2_AccessControl_LR"}
)

// HasKeys reports whether the spec carries any key material, which is what
// decides that the child is started with --config at all: the real server
// refuses a config without securityKeys.S0_Legacy.
func (c ConfigSpec) HasKeys() bool {
	return len(c.Keys) > 0 || len(c.LongRange) > 0
}

// renderKeySet writes one `name: { Class: Buffer.from("<hex>", "hex"), ... },`
// block for the classes of order that have a value. Zwave-js declares keys as
// bytes and rejects a hex string, so each is a Buffer (the sidecar contract §2).
func renderKeySet(b *strings.Builder, name string, order []string, values map[string][]byte) {
	var lines []string
	for _, class := range order {
		if value, ok := values[class]; ok && len(value) > 0 {
			lines = append(lines, "    "+class+": Buffer.from(\""+hex.EncodeToString(value)+"\", \"hex\"),\n")
		}
	}
	if len(lines) == 0 {
		return
	}
	b.WriteString("  " + name + ": {\n")
	for _, line := range lines {
		b.WriteString(line)
	}
	b.WriteString("  },\n")
}

// RenderConfig returns the CommonJS source for spec. Strings are encoded as
// JSON, which is a valid JavaScript string literal, so a path with a quote or a
// backslash cannot break out of the file.
func RenderConfig(spec ConfigSpec) (string, error) {
	if spec.CacheDir == "" {
		return "", errors.New("driver: config cache dir is empty")
	}
	level := spec.LogLevel
	if level == "" {
		level = "info"
	}
	cacheDir, err := json.Marshal(spec.CacheDir)
	if err != nil {
		return "", fmt.Errorf("driver: encode cache dir: %w", err)
	}
	logLevel, err := json.Marshal(level)
	if err != nil {
		return "", fmt.Errorf("driver: encode log level: %w", err)
	}
	var b strings.Builder
	b.WriteString("module.exports = {\n")
	renderKeySet(&b, "securityKeys", classicKeyClasses, spec.Keys)
	renderKeySet(&b, "securityKeysLongRange", longRangeKeyClasses, spec.LongRange)
	b.WriteString("  storage: {\n")
	b.WriteString("    cacheDir: " + string(cacheDir) + ",\n")
	b.WriteString("    throttle: \"normal\",\n")
	b.WriteString("  },\n")
	b.WriteString("  logConfig: {\n")
	b.WriteString("    enabled: true,\n")
	b.WriteString("    level: " + string(logLevel) + ",\n")
	b.WriteString("    logToFile: false,\n")
	b.WriteString("    showLogo: false,\n")
	b.WriteString("  },\n")
	b.WriteString("};\n")
	return b.String(), nil
}

// ConfigFile is one rendered file on disk. Remove is idempotent, so the
// supervisor can call it at child stop and again at shutdown.
type ConfigFile struct {
	path string
}

// WriteConfig renders spec into dir as driver-<random>.cjs with mode 0600,
// creating dir with mode 0700 when it is missing. The random suffix means a
// stale file from a crashed run is never picked up, and O_EXCL means a file is
// never reused.
func WriteConfig(dir string, spec ConfigSpec) (*ConfigFile, error) {
	source, err := RenderConfig(spec)
	if err != nil {
		return nil, err
	}
	if dir == "" {
		return nil, errors.New("driver: config directory is empty")
	}
	if err := os.MkdirAll(dir, configDirMode); err != nil {
		return nil, fmt.Errorf("driver: create config directory: %w", err)
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return nil, fmt.Errorf("driver: generate config name: %w", err)
	}
	path := filepath.Join(dir, "driver-"+hex.EncodeToString(suffix[:])+".cjs")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, configFileMode)
	if err != nil {
		return nil, fmt.Errorf("driver: create config file: %w", err)
	}
	if _, err := file.WriteString(source); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("driver: write config file: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("driver: close config file: %w", err)
	}
	return &ConfigFile{path: path}, nil
}

// Path is the absolute path to pass as --config.
func (f *ConfigFile) Path() string {
	if f == nil {
		return ""
	}
	return f.path
}

// Remove unlinks the file. A file already gone is not an error.
func (f *ConfigFile) Remove() error {
	if f == nil || f.path == "" {
		return nil
	}
	if err := os.Remove(f.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("driver: remove config file: %w", err)
	}
	return nil
}
