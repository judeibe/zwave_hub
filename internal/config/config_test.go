package config

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func validKey() string {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return base64.StdEncoding.EncodeToString(key)
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(nil, Env{}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Bind != DefaultBind {
		t.Errorf("Bind = %q, want %q", cfg.Bind, DefaultBind)
	}
	if cfg.DataDir != DefaultDataDir {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, DefaultDataDir)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if !cfg.DestructiveOps {
		t.Error("DestructiveOps = false, want the enabled default")
	}
	if cfg.DiscoveryInterval != DefaultDiscoveryInterval {
		t.Errorf("DiscoveryInterval = %v, want %v", cfg.DiscoveryInterval, DefaultDiscoveryInterval)
	}
	if cfg.DriverMode != DriverModeHardware {
		t.Errorf("DriverMode = %q, want hardware", cfg.DriverMode)
	}
	if cfg.SidecarDir != DefaultSidecarDir {
		t.Errorf("SidecarDir = %q, want %q", cfg.SidecarDir, DefaultSidecarDir)
	}
	if cfg.MasterKey != nil {
		t.Error("MasterKey is set without ZWAVE_HUB_MASTER_KEY")
	}
}

func TestLoadEnvironment(t *testing.T) {
	env := Env{
		EnvBind:              "127.0.0.1:9090",
		EnvDataDir:           "/tmp/hub",
		EnvMasterKey:         validKey(),
		EnvLogLevel:          "debug",
		EnvDestructiveOps:    "disabled",
		EnvDiscoveryInterval: "750ms",
		EnvDriverMode:        "mock",
		EnvSidecarDir:        "/opt/sidecar",
	}
	cfg, err := Load(nil, env, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Bind != "127.0.0.1:9090" || cfg.DataDir != "/tmp/hub" || cfg.SidecarDir != "/opt/sidecar" {
		t.Errorf("addresses not taken from the environment: %+v", cfg)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want debug", cfg.LogLevel)
	}
	if cfg.DestructiveOps {
		t.Error("DestructiveOps = true, want false")
	}
	if cfg.DiscoveryInterval != 750*time.Millisecond {
		t.Errorf("DiscoveryInterval = %v, want 750ms", cfg.DiscoveryInterval)
	}
	if cfg.DriverMode != DriverModeMock {
		t.Errorf("DriverMode = %q, want mock", cfg.DriverMode)
	}
	if len(cfg.MasterKey) != 32 {
		t.Errorf("MasterKey length = %d, want 32", len(cfg.MasterKey))
	}
}

func TestFlagsOverrideEnvironment(t *testing.T) {
	env := Env{EnvBind: "0.0.0.0:8080", EnvDiscoveryInterval: "2s"}
	args := []string{"-bind", "127.0.0.1:1234", "-discovery-interval", "5s", "-destructive-ops", "disabled"}
	cfg, err := Load(args, env, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Bind != "127.0.0.1:1234" {
		t.Errorf("Bind = %q, want the flag value", cfg.Bind)
	}
	if cfg.DiscoveryInterval != 5*time.Second {
		t.Errorf("DiscoveryInterval = %v, want the flag value", cfg.DiscoveryInterval)
	}
	if cfg.DestructiveOps {
		t.Error("DestructiveOps = true, want the flag value false")
	}
}

func TestEmptyVariableIsUnset(t *testing.T) {
	cfg, err := Load(nil, Env{EnvBind: "", EnvMasterKey: "", EnvDataDir: ""}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Bind != DefaultBind || cfg.DataDir != DefaultDataDir {
		t.Errorf("empty variables did not fall back to the defaults: %+v", cfg)
	}
	if cfg.MasterKey != nil {
		t.Error("an empty ZWAVE_HUB_MASTER_KEY was treated as set")
	}
}

func TestDiscoveryIntervalFloor(t *testing.T) {
	for _, value := range []string{"499ms", "1ms", "0s"} {
		_, err := Load(nil, Env{EnvDiscoveryInterval: value}, &bytes.Buffer{})
		cfgErr, ok := AsConfigError(err)
		if !ok {
			t.Fatalf("interval %q: err = %v, want a ConfigError", value, err)
		}
		if cfgErr.Field != "discovery-interval" {
			t.Errorf("interval %q: field = %q, want discovery-interval", value, cfgErr.Field)
		}
	}
	if _, err := Load(nil, Env{EnvDiscoveryInterval: "500ms"}, &bytes.Buffer{}); err != nil {
		t.Errorf("500ms was rejected: %v", err)
	}
}

func TestMasterKeyValidation(t *testing.T) {
	cases := map[string]string{
		"not base64":     "this is not base64!!",
		"31 bytes":       base64.StdEncoding.EncodeToString(make([]byte, 31)),
		"33 bytes":       base64.StdEncoding.EncodeToString(make([]byte, 33)),
		"hex instead":    strings.Repeat("ab", 32),
		"padded garbage": base64.StdEncoding.EncodeToString(make([]byte, 32)) + "==",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(nil, Env{EnvMasterKey: value}, &bytes.Buffer{})
			cfgErr, ok := AsConfigError(err)
			if !ok {
				t.Fatalf("err = %v, want a ConfigError", err)
			}
			if cfgErr.Field != EnvMasterKey {
				t.Errorf("field = %q, want %q", cfgErr.Field, EnvMasterKey)
			}
			if strings.Contains(cfgErr.Reason, value) {
				t.Error("the reason echoes the submitted value")
			}
		})
	}
}

func TestRawBase64MasterKeyAccepted(t *testing.T) {
	key := make([]byte, 32)
	key[0] = 0xff // forces padding in standard encoding
	raw := base64.RawStdEncoding.EncodeToString(key)
	if strings.Contains(raw, "=") {
		t.Fatal("test key does not exercise the unpadded form")
	}
	cfg, err := Load(nil, Env{EnvMasterKey: raw}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("unpadded base64 rejected: %v", err)
	}
	if len(cfg.MasterKey) != 32 {
		t.Errorf("MasterKey length = %d, want 32", len(cfg.MasterKey))
	}
}

func TestDestructiveOpsParsing(t *testing.T) {
	for value, want := range map[string]bool{"enabled": true, "disabled": false, "DISABLED": false} {
		cfg, err := Load(nil, Env{EnvDestructiveOps: value}, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("%q: %v", value, err)
		}
		if cfg.DestructiveOps != want {
			t.Errorf("%q: DestructiveOps = %v, want %v", value, cfg.DestructiveOps, want)
		}
	}
	if _, err := Load(nil, Env{EnvDestructiveOps: "yes"}, &bytes.Buffer{}); !IsConfigError(err) {
		t.Errorf("an unparseable value was accepted: %v", err)
	}
}

func TestBindValidation(t *testing.T) {
	for _, value := range []string{"8080", "0.0.0.0", "0.0.0.0:0", "0.0.0.0:70000", "example.com:8080"} {
		if _, err := Load(nil, Env{EnvBind: value}, &bytes.Buffer{}); !IsConfigError(err) {
			t.Errorf("bind %q was accepted", value)
		}
	}
	for _, value := range []string{":8080", "0.0.0.0:8080", "127.0.0.1:8080", "localhost:8080", "[::1]:8080"} {
		if _, err := Load(nil, Env{EnvBind: value}, &bytes.Buffer{}); err != nil {
			t.Errorf("bind %q was rejected: %v", value, err)
		}
	}
}

func TestPositionalArgumentsRejected(t *testing.T) {
	if _, err := Load([]string{"serve"}, Env{}, &bytes.Buffer{}); !IsConfigError(err) {
		t.Errorf("a positional argument was accepted: %v", err)
	}
}

func TestDestructiveOpsLabel(t *testing.T) {
	if got := (&Config{DestructiveOps: true}).DestructiveOpsLabel(); got != "enabled" {
		t.Errorf("label = %q, want enabled", got)
	}
	if got := (&Config{DestructiveOps: false}).DestructiveOpsLabel(); got != "disabled" {
		t.Errorf("label = %q, want disabled", got)
	}
}
