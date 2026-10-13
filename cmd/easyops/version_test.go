package main

import "testing"

// TestAppVersion: appVersion returns APP_VERSION when set, else "dev".
func TestAppVersion(t *testing.T) {
	t.Setenv("APP_VERSION", "v1.2.3")
	if got := appVersion(); got != "v1.2.3" {
		t.Fatalf("appVersion() = %q, want v1.2.3", got)
	}
	t.Setenv("APP_VERSION", "")
	if got := appVersion(); got != "dev" {
		t.Fatalf("appVersion() empty = %q, want dev", got)
	}
}
