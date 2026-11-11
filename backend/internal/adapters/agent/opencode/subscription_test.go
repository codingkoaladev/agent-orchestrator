package opencode

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSubscriptionLabelValidatesConfiguredValue(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{name: "unset", value: "", want: ""},
		{name: "trimmed", value: "  subscription-3  ", want: "subscription-3"},
		{name: "dots and underscores", value: "team_a.eu-1", want: "team_a.eu-1"},
		{name: "path traversal", value: "../subscription-3", wantErr: true},
		{name: "absolute path", value: "/home/user/.local/share/x", wantErr: true},
		{name: "uppercase", value: "Subscription-3", wantErr: true},
		{name: "leading dash", value: "-subscription-3", wantErr: true},
		{name: "too long", value: "a0123456789012345678901234567890123456789012345678901234567890123456789", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvOpenCodeSubscription, tt.value)
			got, err := SubscriptionLabel()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("SubscriptionLabel() = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("SubscriptionLabel() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("SubscriptionLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSubscriptionDataHomeIsASinglePathSegment(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("XDG_DATA_HOME", parent)
	t.Setenv(EnvOpenCodeSubscription, "")

	home, err := SubscriptionDataHome("subscription-3")
	if err != nil {
		t.Fatalf("SubscriptionDataHome() error = %v", err)
	}
	want := filepath.Join(parent, SubscriptionScopesDirName, "subscription-3")
	if home != want {
		t.Fatalf("SubscriptionDataHome() = %q, want %q", home, want)
	}
	if _, err := SubscriptionDataHome("../escape"); err == nil {
		t.Fatal("SubscriptionDataHome() accepted a traversing label")
	}
}

func TestV2DataHomeUsesPinnedScopeAndKeepsDefaultOtherwise(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("XDG_DATA_HOME", parent)

	t.Setenv(EnvOpenCodeSubscription, "")
	unpinned, err := V2DataHome()
	if err != nil {
		t.Fatalf("V2DataHome() error = %v", err)
	}
	if want := filepath.Join(parent, v2DataHomeDirName); unpinned != want {
		t.Fatalf("V2DataHome() = %q, want %q", unpinned, want)
	}

	t.Setenv(EnvOpenCodeSubscription, "subscription-2")
	pinned, err := V2DataHome()
	if err != nil {
		t.Fatalf("V2DataHome() error = %v", err)
	}
	if want := filepath.Join(parent, SubscriptionScopesDirName, "subscription-2"); pinned != want {
		t.Fatalf("V2DataHome() = %q, want %q", pinned, want)
	}

	t.Setenv(EnvOpenCodeSubscription, "bad/label")
	if _, err := V2DataHome(); err == nil {
		t.Fatal("V2DataHome() accepted a label containing a path separator")
	}
}

// A pinned subscription must start empty: copying the legacy store would bring
// another subscription's saved account and session history into a scope the
// operator never logged in to.
func TestPrepareV2DataHomeDoesNotCopyLegacyStoreIntoTemplateScope(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("XDG_DATA_HOME", parent)
	legacy := filepath.Join(parent, "opencode")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "auth.json"), []byte(`{"opencode-go":{"type":"api"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv(EnvOpenCodeSubscription, "subscription-3")
	home, err := PrepareV2DataHome(context.Background())
	if err != nil {
		t.Fatalf("PrepareV2DataHome() error = %v", err)
	}
	if want := filepath.Join(parent, SubscriptionScopesDirName, "subscription-3"); home != want {
		t.Fatalf("PrepareV2DataHome() = %q, want %q", home, want)
	}
	info, err := os.Stat(home)
	if err != nil {
		t.Fatalf("stat pinned scope: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("pinned scope permissions = %o, want 700", perm)
	}
	if _, err := os.Stat(filepath.Join(home, "opencode", "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("pinned scope contains copied credentials: err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "opencode")); !os.IsNotExist(err) {
		t.Fatalf("pinned scope contains copied store: err = %v", err)
	}
	// The legacy store is left untouched for OpenCode 1.
	if _, err := os.Stat(filepath.Join(legacy, "auth.json")); err != nil {
		t.Fatalf("legacy store was modified: %v", err)
	}
}
