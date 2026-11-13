// Package config reads flags and the ZWAVE_HUB_* environment once at start and
// validates the result. It is the only package that reads the environment.
package config

import (
	"encoding/base64"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment variable names. Each is read in this package and nowhere else
// (constitution III); flags of the same name override them for one run.
const (
	EnvBind              = "ZWAVE_HUB_BIND"
	EnvDataDir           = "ZWAVE_HUB_DATA_DIR"
	EnvMasterKey         = "ZWAVE_HUB_MASTER_KEY"
	EnvLogLevel          = "ZWAVE_HUB_LOG_LEVEL"
	EnvDestructiveOps    = "ZWAVE_HUB_DESTRUCTIVE_OPS"
	EnvDiscoveryInterval = "ZWAVE_HUB_DISCOVERY_INTERVAL"
	EnvDriverMode        = "ZWAVE_HUB_DRIVER_MODE"
	EnvSidecarDir        = "ZWAVE_HUB_SIDECAR_DIR"
)

// Defaults from quickstart.md §2. They are also what the container image and
// the compose file assume, so changing one here changes the deployed default.
const (
	DefaultBind              = "0.0.0.0:8080"
	DefaultDataDir           = "/data"
	DefaultLogLevel          = "info"
	DefaultDestructiveOps    = "enabled"
	DefaultDiscoveryInterval = 2 * time.Second
	DefaultDriverMode        = DriverModeHardware
	DefaultSidecarDir        = "/opt/zwave-sidecar"

	// MinDiscoveryInterval is the floor quickstart.md §2 rejects. A faster poll
	// costs sysfs reads forever and cannot make a plug-in visible sooner than
	// the kernel settles it.
	MinDiscoveryInterval = 500 * time.Millisecond

	// masterKeySize is the AES-256 key length; ZWAVE_HUB_MASTER_KEY must decode
	// to exactly this many bytes.
	masterKeySize = 32
)

// DriverMode selects what the supervisor starts: the real stick, or
// zwave-server's --mock-driver for development and contract tests.
type DriverMode string

const (
	DriverModeHardware DriverMode = "hardware"
	DriverModeMock     DriverMode = "mock"
)

// Config is the validated configuration. Every field is final once Load
// returns; nothing re-reads the environment later.
type Config struct {
	// Bind is the HTTP listen address (host:port). It is the network's trust
	// boundary: the API has no authentication (FR-020).
	Bind string
	// DataDir holds hub.db, master.key when the environment does not supply
	// one, and the zwave-js cache.
	DataDir string
	// MasterKey is the decoded 32-byte key from ZWAVE_HUB_MASTER_KEY, or nil
	// when unset, in which case the store creates <DataDir>/master.key.
	MasterKey []byte
	// LogLevel is the structured event floor. info in production.
	LogLevel slog.Level
	// DestructiveOps gates the network reset (ZWAVE_HUB_DESTRUCTIVE_OPS).
	DestructiveOps bool
	// DiscoveryInterval is the sysfs poll period, at least
	// MinDiscoveryInterval.
	DiscoveryInterval time.Duration
	// DriverMode is hardware unless explicitly set to mock.
	DriverMode DriverMode
	// SidecarDir is where @zwave-js/server is installed in the image.
	SidecarDir string
}

// DestructiveOpsLabel is the value the events and the env example use.
func (c *Config) DestructiveOpsLabel() string {
	if c.DestructiveOps {
		return "enabled"
	}
	return "disabled"
}

// Env is the process environment as a name-to-value map. Tests pass one;
// production passes Environ().
type Env map[string]string

// Environ reads the current process environment. It is the only os.Environ
// call in the repository.
func Environ() Env {
	pairs := os.Environ()
	env := make(Env, len(pairs))
	for _, pair := range pairs {
		name, value, ok := strings.Cut(pair, "=")
		if ok {
			env[name] = value
		}
	}
	return env
}

// Lookup returns the value and whether it was set and non-empty. An empty
// variable is treated as unset, which is what a `FOO=` line in a compose file
// means to an operator.
func (e Env) Lookup(name string) (string, bool) {
	value, ok := e[name]
	if !ok || value == "" {
		return "", false
	}
	return value, true
}

// ConfigError is one setting that cannot be used. Field names the flag or
// variable and Reason is a fixed sentence that never contains the value, so the
// caller can log it verbatim: hub.config_invalid must not echo a master key the
// operator typed wrong.
type ConfigError struct {
	Field  string
	Reason string
}

func (e *ConfigError) Error() string { return e.Field + ": " + e.Reason }

// Load parses args over env and returns the validated configuration. Usage and
// flag errors go to out. It fails fast with a *ConfigError naming the field.
func Load(args []string, env Env, out io.Writer) (*Config, error) {
	cfg := &Config{}
	flags := flag.NewFlagSet("zwave-hub", flag.ContinueOnError)
	flags.SetOutput(out)

	// Defaults come from the environment so a flag is only needed to override
	// it for one run; a flag wins, which is what makes local development
	// possible without editing a deployed environment.
	flags.StringVar(&cfg.Bind, "bind", envOr(env, EnvBind, DefaultBind), "HTTP listen address (host:port)")
	flags.StringVar(&cfg.DataDir, "data-dir", envOr(env, EnvDataDir, DefaultDataDir), "directory holding hub.db and the fallback master key")
	flags.StringVar(&cfg.SidecarDir, "sidecar-dir", envOr(env, EnvSidecarDir, DefaultSidecarDir), "directory holding @zwave-js/server")
	logLevel := flags.String("log-level", envOr(env, EnvLogLevel, DefaultLogLevel), "event floor: debug, info, warn or error")
	destructiveOps := flags.String("destructive-ops", envOr(env, EnvDestructiveOps, DefaultDestructiveOps), "enabled or disabled; disabled closes the network reset")
	discoveryInterval := flags.String("discovery-interval", envOr(env, EnvDiscoveryInterval, DefaultDiscoveryInterval.String()), "sysfs poll period, at least 500ms")
	driverMode := flags.String("driver-mode", string(envOr(env, EnvDriverMode, string(DefaultDriverMode))), "hardware or mock")
	// ZWAVE_HUB_MASTER_KEY deliberately has no flag: command lines are visible
	// in ps and in the container's inspect output.

	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if rest := flags.Args(); len(rest) > 0 {
		return nil, &ConfigError{Field: "arguments", Reason: "unexpected positional arguments: " + strconv.Quote(strings.Join(rest, " "))}
	}

	if err := validateBind(cfg.Bind); err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.DataDir) == "" {
		return nil, &ConfigError{Field: "data-dir", Reason: "must not be empty"}
	}
	if strings.TrimSpace(cfg.SidecarDir) == "" {
		return nil, &ConfigError{Field: "sidecar-dir", Reason: "must not be empty"}
	}

	level, err := parseLogLevel(*logLevel)
	if err != nil {
		return nil, err
	}
	cfg.LogLevel = level

	ops, err := parseDestructiveOps(*destructiveOps)
	if err != nil {
		return nil, err
	}
	cfg.DestructiveOps = ops

	interval, err := parseDiscoveryInterval(*discoveryInterval)
	if err != nil {
		return nil, err
	}
	cfg.DiscoveryInterval = interval

	mode, err := parseDriverMode(*driverMode)
	if err != nil {
		return nil, err
	}
	cfg.DriverMode = mode

	key, err := parseMasterKey(env)
	if err != nil {
		return nil, err
	}
	cfg.MasterKey = key

	return cfg, nil
}

func envOr(env Env, name, fallback string) string {
	if value, ok := env.Lookup(name); ok {
		return value
	}
	return fallback
}

func validateBind(bind string) error {
	host, port, err := net.SplitHostPort(bind)
	if err != nil {
		return &ConfigError{Field: "bind", Reason: "must be a host:port address"}
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return &ConfigError{Field: "bind", Reason: "port must be a number between 1 and 65535"}
	}
	if host != "" && net.ParseIP(host) == nil && host != "localhost" {
		return &ConfigError{Field: "bind", Reason: "host must be an IP address, localhost, or empty"}
	}
	return nil
}

func parseLogLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, &ConfigError{Field: "log-level", Reason: "must be debug, info, warn or error"}
	}
}

func parseDestructiveOps(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "enabled":
		return true, nil
	case "disabled":
		return false, nil
	default:
		return false, &ConfigError{Field: "destructive-ops", Reason: "must be enabled or disabled"}
	}
}

func parseDiscoveryInterval(value string) (time.Duration, error) {
	interval, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, &ConfigError{Field: "discovery-interval", Reason: "must be a duration such as 2s"}
	}
	if interval < MinDiscoveryInterval {
		return 0, &ConfigError{Field: "discovery-interval", Reason: "must be at least 500ms"}
	}
	return interval, nil
}

func parseDriverMode(value string) (DriverMode, error) {
	switch DriverMode(strings.ToLower(strings.TrimSpace(value))) {
	case DriverModeHardware:
		return DriverModeHardware, nil
	case DriverModeMock:
		return DriverModeMock, nil
	default:
		return "", &ConfigError{Field: "driver-mode", Reason: "must be hardware or mock"}
	}
}

// parseMasterKey decodes ZWAVE_HUB_MASTER_KEY when it is set. The reason
// deliberately never quotes the value.
func parseMasterKey(env Env) ([]byte, error) {
	encoded, ok := env.Lookup(EnvMasterKey)
	if !ok {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil {
		return nil, &ConfigError{Field: EnvMasterKey, Reason: "must be base64 encoding of exactly 32 bytes"}
	}
	if len(key) != masterKeySize {
		return nil, &ConfigError{Field: EnvMasterKey, Reason: "must be base64 encoding of exactly 32 bytes"}
	}
	return key, nil
}

// IsConfigError reports whether err is a validation failure rather than a flag
// parse error, so main can emit hub.config_invalid for the first and print
// usage for the second.
func IsConfigError(err error) bool {
	var target *ConfigError
	return errors.As(err, &target)
}

// AsConfigError returns the validation failure err carries, if any.
func AsConfigError(err error) (*ConfigError, bool) {
	var target *ConfigError
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}
