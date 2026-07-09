package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCredsAppliesDefaults(t *testing.T) {
	path := writeCredsFile(t, `{"access_token":"a","refresh_token":"r","client_id":"c"}`)
	c, err := LoadCreds(path)
	if err != nil {
		t.Fatalf("LoadCreds: %v", err)
	}
	if c.TokenURL != defaultTokenURL {
		t.Errorf("TokenURL = %q, want default %q", c.TokenURL, defaultTokenURL)
	}
	if c.Scope != defaultScope {
		t.Errorf("Scope = %q, want default %q", c.Scope, defaultScope)
	}
	if !c.canRefresh() {
		t.Error("canRefresh = false, want true (client_id + refresh_token present)")
	}
}

func TestLoadCredsMissingAccessToken(t *testing.T) {
	path := writeCredsFile(t, `{"refresh_token":"r"}`)
	if _, err := LoadCreds(path); err == nil {
		t.Fatal("expected error for empty access_token, got nil")
	}
}

func TestCanRefreshFalseWithoutClientID(t *testing.T) {
	path := writeCredsFile(t, `{"access_token":"a","refresh_token":"r"}`)
	c, err := LoadCreds(path)
	if err != nil {
		t.Fatalf("LoadCreds: %v", err)
	}
	if c.canRefresh() {
		t.Error("canRefresh = true, want false (no client_id)")
	}
}

func TestSaveCredsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tesla.json")
	in := &Creds{AccessToken: "new-access", RefreshToken: "rotated", ExpiresAt: 1750000000, ClientID: "c"}
	if err := SaveCreds(path, in); err != nil {
		t.Fatalf("SaveCreds: %v", err)
	}
	out, err := LoadCreds(path)
	if err != nil {
		t.Fatalf("LoadCreds: %v", err)
	}
	if out.AccessToken != in.AccessToken || out.RefreshToken != in.RefreshToken || out.ExpiresAt != in.ExpiresAt {
		t.Errorf("round-trip mismatch: got %+v want %+v", out, in)
	}
}

func writeCredsFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tesla.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write creds: %v", err)
	}
	return path
}
