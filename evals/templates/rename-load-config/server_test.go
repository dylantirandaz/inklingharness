package fixture

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.conf")
	if err := os.WriteFile(path, []byte("name=demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if port, err := Port(path); err != nil || port != "80" {
		t.Fatalf("Port = %q, %v; want 80", port, err)
	}
}
