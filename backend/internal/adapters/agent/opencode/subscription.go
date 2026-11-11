package opencode

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// EnvOpenCodeSubscription names the OpenCode account scope an Orchestrator
// process pins for every OpenCode 2 session it launches. The value is an
// operator-chosen subscription label, never a credential, an account number or
// a filesystem path.
//
// OpenCode 2 stores saved accounts in <XDG_DATA_HOME>/opencode/auth.json and
// switches the active account inside that one store. Pinning a label therefore
// means pinning a data home: two subscriptions can run side by side without
// either process calling `opencode auth switch` on the other's credential
// store.
const EnvOpenCodeSubscription = "AO_OPENCODE_SUBSCRIPTION"

// SubscriptionScopesDirName is the directory under the user's data parent that
// holds one isolated OpenCode data home per subscription label.
const SubscriptionScopesDirName = "opencode-subscriptions"

// subscriptionLabelPattern keeps a label usable as a single path segment: it
// cannot traverse (`..`), start a flag, or carry a separator.
var subscriptionLabelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// SubscriptionLabel returns the subscription scope this process is pinned to,
// or "" when no scope is configured. Chaining of unrelated labels is treated as
// invalid rather than silently truncated.
func SubscriptionLabel() (string, error) {
	label := strings.TrimSpace(os.Getenv(EnvOpenCodeSubscription))
	if label == "" {
		return "", nil
	}
	if !subscriptionLabelPattern.MatchString(label) {
		return "", fmt.Errorf("opencode: %s must match %s, got %q", EnvOpenCodeSubscription, subscriptionLabelPattern, label)
	}
	return label, nil
}

// SubscriptionDataHome returns the XDG_DATA_HOME that holds the saved accounts
// and sessions for label. The directory is not created here; callers that are
// about to launch OpenCode use PrepareSubscriptionDataHome.
func SubscriptionDataHome(label string) (string, error) {
	label = strings.TrimSpace(label)
	if !subscriptionLabelPattern.MatchString(label) {
		return "", fmt.Errorf("opencode: subscription label must match %s, got %q", subscriptionLabelPattern, label)
	}
	parent, err := dataHomeParent()
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, SubscriptionScopesDirName, label), nil
}

// PrepareSubscriptionDataHome resolves and creates the pinned data home. It
// deliberately does not copy another scope's store: credentials are only ever
// written by that scope's own `opencode auth login`, so enabling a label can
// never duplicate a saved account into a new directory.
func PrepareSubscriptionDataHome(label string) (string, error) {
	home, err := SubscriptionDataHome(label)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", fmt.Errorf("opencode: create subscription data home: %w", err)
	}
	return home, nil
}

// dataHomeParent returns the parent directory OpenCode data homes live under:
// $XDG_DATA_HOME when set, otherwise ~/.local/share.
func dataHomeParent() (string, error) {
	parent := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
	if parent != "" {
		if !filepath.IsAbs(parent) {
			return "", fmt.Errorf("opencode: XDG_DATA_HOME must be absolute, got %q", parent)
		}
		return filepath.Clean(parent), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("opencode: resolve absolute user data home: %w", err)
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("opencode: user data home must be absolute, got %q", home)
	}
	return filepath.Join(home, ".local", "share"), nil
}
