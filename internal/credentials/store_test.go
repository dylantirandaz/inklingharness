package credentials

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveRestrictsModeAndLoadRoundTrips(t *testing.T) {
	store := Store{path: filepath.Join(t.TempDir(), "nested", "credentials.json")}

	_, found, err := store.Load()
	if err != nil || found {
		t.Fatalf("Load before Save: found=%v err=%v", found, err)
	}

	// A pre-existing world-readable file must end up restricted after Save.
	if err := os.MkdirAll(filepath.Dir(store.Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path(), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("tk-secret"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, want 0600", info.Mode().Perm())
	}

	apiKey, found, err := store.Load()
	if err != nil || !found || apiKey != "tk-secret" {
		t.Fatalf("Load after Save: key=%q found=%v err=%v", apiKey, found, err)
	}
}
