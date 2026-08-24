package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCfg(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "config")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadFrom(t *testing.T) {
	dir := t.TempDir()
	good := writeCfg(t, dir, "[workspace default]\nrepo = /x\n")

	t.Run("flag wins over env and default", func(t *testing.T) {
		other := filepath.Join(t.TempDir(), "other")
		if err := os.WriteFile(other, []byte("[workspace fromenv]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f, path, err := LoadFrom(good, other, other, false)
		if err != nil || f == nil {
			t.Fatalf("LoadFrom: f=%v err=%v", f, err)
		}
		if path != good {
			t.Errorf("path = %q, want the --config value %q", path, good)
		}
		if _, ok := f.Workspace("default"); !ok {
			t.Error("loaded the wrong file")
		}
	})

	t.Run("env wins over default", func(t *testing.T) {
		f, path, err := LoadFrom("", good, filepath.Join(dir, "absent"), false)
		if err != nil || f == nil {
			t.Fatalf("LoadFrom: f=%v err=%v", f, err)
		}
		if path != good {
			t.Errorf("path = %q, want %q", path, good)
		}
	})

	t.Run("explicit missing path is an error", func(t *testing.T) {
		missing := filepath.Join(dir, "nope")
		if _, _, err := LoadFrom(missing, "", "", false); err == nil {
			t.Error("LoadFrom(--config <missing>) succeeded, want an error")
		}
		if _, _, err := LoadFrom("", missing, "", false); err == nil {
			t.Error("LoadFrom(HARNESS_CONFIG=<missing>) succeeded, want an error")
		}
	})

	t.Run("missing default path is silent", func(t *testing.T) {
		f, path, err := LoadFrom("", "", filepath.Join(dir, "absent"), false)
		if err != nil || f != nil || path != "" {
			t.Errorf("LoadFrom(default absent) = %v, %q, %v; want nil, \"\", nil", f, path, err)
		}
	})

	t.Run("a malformed default file is still an error", func(t *testing.T) {
		bad := writeCfg(t, t.TempDir(), "[profile w]\n")
		if _, _, err := LoadFrom("", "", bad, false); err == nil {
			t.Error("a default-path file that does not parse was accepted")
		}
	})

	t.Run("in an agent nothing is read", func(t *testing.T) {
		f, path, err := LoadFrom(good, good, good, true)
		if err != nil || f != nil || path != "" {
			t.Errorf("LoadFrom(inAgent) = %v, %q, %v; want nil, \"\", nil", f, path, err)
		}
	})
}
