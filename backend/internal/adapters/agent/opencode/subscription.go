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

// EnvSubscriptionLabel lets a project pin its workers to a subscription without
// touching the daemon-wide default. AO reserves the AO_ prefix, so this is the
// project-config key the adapter reads.
const EnvSubscriptionLabel = "OPENCODE_SUBSCRIPTION"

// LabelForSession resolves the subscription a worker must keep for its whole
// life: the worker's own pin wins, then an explicit project label, then the
// daemon default. The pin is what makes a restart or resume land in the same
// account scope as the original launch.
func LabelForSession(env map[string]string, sessionID string, dataDir string) (string, error) {
	if pin := strings.TrimSpace(readPinnedLabel(dataDir, sessionID)); pin != "" {
		if !subscriptionLabelPattern.MatchString(pin) {
			return "", fmt.Errorf("opencode: pinned subscription label is invalid, got %q", pin)
		}
		return pin, nil
	}
	if label := strings.TrimSpace(env[EnvSubscriptionLabel]); label != "" {
		if !subscriptionLabelPattern.MatchString(label) {
			return "", fmt.Errorf("opencode: %s must match %s, got %q", EnvSubscriptionLabel, subscriptionLabelPattern, label)
		}
		return label, nil
	}
	return SubscriptionLabel()
}

// PinSessionLabel records the label a worker launched with, so a later resume
// reuses it even if the project configuration changes.
func PinSessionLabel(dataDir, sessionID, label string) error {
	label = strings.TrimSpace(label)
	if label == "" || !subscriptionLabelPattern.MatchString(label) {
		return fmt.Errorf("opencode: cannot pin invalid subscription label %q", label)
	}
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("opencode: cannot pin a subscription without a session id")
	}
	dir := filepath.Join(AODataDir(dataDir), "coordinator", "subscriptions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("opencode: create subscription pin dir: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, sessionID), []byte(label+"\n"), 0o600)
}

// AODataDir resolves AO's state data directory.
func AODataDir(explicit string) string {
	if dir := strings.TrimSpace(explicit); dir != "" {
		return dir
	}
	if dir := strings.TrimSpace(os.Getenv("AO_DATA_DIR")); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".ao/data"
	}
	return filepath.Join(home, ".ao", "data")
}

func readPinnedLabel(dataDir, sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(AODataDir(dataDir), "coordinator", "subscriptions", sessionID))
	if err != nil {
		return ""
	}
	return string(data)
}
