package controllers

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// A chat worker's native identity lives in providerConversationId, not
// agentSessionId. The curated session view must expose it so clients can resume
// the provider conversation without reading daemon storage or guessing an id.
func TestSessionViewProjectsHarnessTaggedProviderConversation(t *testing.T) {
	session := domain.Session{
		ID:       "magento-1",
		Harness:  domain.HarnessOpenCodeV2,
		Mode:     domain.SessionModeChat,
		Metadata: domain.SessionMetadata{ProviderConversationID: "ao-harness:opencode-v2:ses_native_1"},
	}
	view := sessionView(httptest.NewRequest("GET", "/api/v1/sessions/magento-1", nil), session)
	if view.ProviderConversationID != "ao-harness:opencode-v2:ses_native_1" {
		t.Fatalf("providerConversationId = %q", view.ProviderConversationID)
	}
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"providerConversationId":"ao-harness:opencode-v2:ses_native_1"`) {
		t.Fatalf("session wire is missing providerConversationId: %s", data)
	}
	if !strings.Contains(string(data), `"mode":"chat"`) {
		t.Fatalf("session wire is missing chat mode: %s", data)
	}
}
