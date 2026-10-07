package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.conf")
	if err := os.WriteFile(path, []byte("# comment\nport = 8080\n\nname=demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config["port"] != "8080" || config["name"] != "demo" || len(config) != 2 {
		t.Fatalf("LoadConfig = %v", config)
	}
}
