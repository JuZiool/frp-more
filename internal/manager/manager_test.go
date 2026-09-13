package manager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testConfig = `serverAddr = "127.0.0.1"
serverPort = 7000
loginFailExit = false

[[proxies]]
name = "web"
type = "tcp"
localIP = "127.0.0.1"
localPort = 8080
remotePort = 6001
`

func TestUpdateConfigRenamesStoppedInstance(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(m.dir, "old.toml")
	if err := os.WriteFile(oldPath, []byte(testConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	m.instances["old"] = newInstance("old", oldPath, m.unsafe)

	newConfig := strings.Replace(testConfig, "127.0.0.1", "frps.internal", 1)
	if err := m.UpdateConfig("old", "new", newConfig); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old config still exists, stat error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(m.dir, "new.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != newConfig {
		t.Fatalf("new config = %q, want %q", got, newConfig)
	}
	if _, ok := m.instances["old"]; ok {
		t.Fatal("old instance name is still registered")
	}
	if _, ok := m.instances["new"]; !ok {
		t.Fatal("new instance name is not registered")
	}
}

func TestUpdateConfigInvalidLeavesOriginalConfig(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.dir, "demo.toml")
	if err := os.WriteFile(path, []byte(testConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	m.instances["demo"] = newInstance("demo", path, m.unsafe)

	if err := m.UpdateConfig("demo", "demo", "this is not valid toml = ["); err == nil {
		t.Fatal("invalid config unexpectedly succeeded")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != testConfig {
		t.Fatalf("original config changed to %q", got)
	}
}
