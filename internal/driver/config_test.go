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
		t.Fatal("config carries security keys although the spec has none")
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

func repeatByte(b byte) []byte {
	out := make([]byte, 16)
	for i := range out {
		out[i] = b
	}
	return out
}

func TestRenderConfigKeysAsBuffers(t *testing.T) {
	source, err := RenderConfig(ConfigSpec{
		CacheDir:  "/c",
		Keys:      map[string][]byte{"S0_Legacy": repeatByte(0xab), "S2_Authenticated": repeatByte(0x01)},
		LongRange: map[string][]byte{"S2_Authenticated_LR": repeatByte(0x02)},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`securityKeys: {`,
		`S0_Legacy: Buffer.from("abababababababababababababababab", "hex"),`,
		`S2_Authenticated: Buffer.from("01010101010101010101010101010101", "hex"),`,
		`securityKeysLongRange: {`,
		`S2_Authenticated_LR: Buffer.from("02020202020202020202020202020202", "hex"),`,
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("config lacks %q:\n%s", want, source)
		}
	}
	// Absent classes are omitted, not written empty.
	for _, absent := range []string{"S2_AccessControl:", "S2_Unauthenticated:", "S2_AccessControl_LR:"} {
		if strings.Contains(source, absent) {
			t.Fatalf("config writes absent class %q:\n%s", absent, source)
		}
	}
}

func TestRenderConfigOmitsEmptyLongRangeSet(t *testing.T) {
	source, err := RenderConfig(ConfigSpec{CacheDir: "/c", Keys: map[string][]byte{"S0_Legacy": repeatByte(1)}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(source, "securityKeysLongRange") {
		t.Fatalf("empty Long Range set rendered:\n%s", source)
	}
}
