package driver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteConfigModesAndRemove(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	file, err := WriteConfig(dir, ConfigSpec{CacheDir: "/data/zwave-cache"})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, want 0600", info.Mode().Perm())
	}
	dirInfo, _ := os.Stat(dir)
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, want 0700", dirInfo.Mode().Perm())
	}
	if !strings.HasSuffix(file.Path(), ".cjs") || !strings.HasPrefix(filepath.Base(file.Path()), "driver-") {
		t.Fatalf("unexpected name %s", file.Path())
	}
	body, _ := os.ReadFile(file.Path())
	for _, want := range []string{`cacheDir: "/data/zwave-cache"`, "logToFile: false", "showLogo: false"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("config lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(string(body), "securityKeys") {
		t.Fatal("config carries security keys before any exist")
	}
	other, err := WriteConfig(dir, ConfigSpec{CacheDir: "/x"})
	if err != nil {
		t.Fatal(err)
	}
	if other.Path() == file.Path() {
		t.Fatal("two files share a name")
	}
	if err := file.Remove(); err != nil {
		t.Fatal(err)
	}
	if err := file.Remove(); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
	if _, err := os.Stat(file.Path()); !os.IsNotExist(err) {
		t.Fatalf("file still present: %v", err)
	}
}

func TestRenderConfigEscapesPaths(t *testing.T) {
	source, err := RenderConfig(ConfigSpec{CacheDir: `/a"b\c`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(source, `"/a\"b\\c"`) {
		t.Fatalf("path not escaped:\n%s", source)
	}
	if _, err := RenderConfig(ConfigSpec{}); err == nil {
		t.Fatal("empty cache dir accepted")
	}
}
