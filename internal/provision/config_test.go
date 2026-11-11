package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/biralo-studio/biralobase/tests"
)

func TestSecretFilesAndConflicts(t *testing.T) {
	const key = "BIRALOBASE_TEST_SECRET"
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("  secret with spaces  \n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(key+"_FILE", path)
	v, err := Secret(key)
	if err != nil || v != "  secret with spaces  " {
		t.Fatalf("secret decoding failed: %v", err)
	}
	t.Setenv(key, "do-not-leak")
	_, err = Secret(key)
	if err == nil || strings.Contains(err.Error(), "do-not-leak") {
		t.Fatal("conflict or redaction failed")
	}
}

func TestManagedAdminLifecycle(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	email := "managed@example.com"
	password := "a-long-initial-password"
	if err := ReconcileAdmin(app, email, password); err != nil {
		t.Fatal(err)
	}
	a, err := app.FindAuthRecordByEmail("_superusers", email)
	if err != nil {
		t.Fatal(err)
	}
	id, key, hash := a.Id, a.TokenKey(), a.GetString("password")
	if err := ReconcileAdmin(app, email, password); err != nil {
		t.Fatal(err)
	}
	a, _ = app.FindAuthRecordByEmail("_superusers", email)
	if a.TokenKey() != key || a.GetString("password") != hash {
		t.Fatal("unchanged secret invalidated credentials")
	}
	if err := ReconcileAdmin(app, "renamed@example.com", "a-long-rotated-password"); err != nil {
		t.Fatal(err)
	}
	a, err = app.FindRecordById("_superusers", id)
	if err != nil || a.Email() != "renamed@example.com" || !a.ValidatePassword("a-long-rotated-password") {
		t.Fatal("rotation lost managed identity")
	}
	if a.ValidatePassword(password) {
		t.Fatal("old password remains valid")
	}
}

func TestProductionConfigValidation(t *testing.T) {
	c := Config{AdminEmail: "admin@example.com", AdminPassword: "a-long-test-password", PublicURL: "https://example.com", EncryptionKey: strings.Repeat("x", 32)}
	if err := c.ValidateServer(); err != nil {
		t.Fatal(err)
	}
	c.PublicURL = "http://example.com"
	if c.ValidateServer() == nil {
		t.Fatal("insecure production origin accepted")
	}
	c.PublicURL = "https://example.com"
	c.EncryptionKey = ""
	if c.ValidateServer() == nil {
		t.Fatal("missing encryption secret accepted")
	}
}
