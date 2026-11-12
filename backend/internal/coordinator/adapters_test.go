package coordinator

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type wireTransport func(*http.Request) (*http.Response, error)

func (f wireTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestResolveNativeIdentityUsesHarnessTaggedProviderConversation(t *testing.T) {
	native, err := resolveNativeIdentity("opencode-v2", "", "ao-harness:opencode-v2:ses_native_1")
	if err != nil || native != "ses_native_1" {
		t.Fatalf("native=%q err=%v", native, err)
	}
	if _, err := resolveNativeIdentity("opencode-v2", "", "ses_guessed"); err == nil {
		t.Fatal("untagged provider conversation was accepted as a native identity")
	}
	if _, err := resolveNativeIdentity("opencode-v2", "", "ao-harness:codex:ses_native_1"); err == nil {
		t.Fatal("provider conversation tagged for another harness was accepted")
	}
	if _, err := resolveNativeIdentity("opencode-v2", "ses_other", "ao-harness:opencode-v2:ses_native_1"); err == nil {
		t.Fatal("conflicting native identities were accepted")
	}
	if native, err := resolveNativeIdentity("codex", "ses_tui", ""); err != nil || native != "ses_tui" {
		t.Fatalf("agent session identity fallback: %q %v", native, err)
	}
}

func TestDaemonReadsActualSessionEnvelopeAndChatNativeIdentity(t *testing.T) {
	d := &Daemon{HTTP: &http.Client{Transport: wireTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"sessions":[{"id":"worker-1","harness":"opencode-v2","providerConversationId":"ao-harness:opencode-v2:ses-native","isTerminated":false}]}`
		if !strings.Contains(r.URL.RawQuery, "includeTerminated") {
			body = `{"session":{"id":"worker-1","harness":"opencode-v2","providerConversationId":"ao-harness:opencode-v2:ses-native","mode":"chat"}}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}, BaseURL: "http://localhost"}
	sessions, err := d.List(context.Background())
	if err != nil || len(sessions) != 1 || sessions[0].NativeID != "ses-native" {
		t.Fatalf("running worker identity disappeared: sessions=%v err=%v", sessions, err)
	}
	status, err := d.Get(context.Background(), "worker-1")
	if err != nil || status.NativeID != "ses-native" || status.Mode != "chat" {
		t.Fatalf("chat identity missing: %+v err=%v", status, err)
	}
}

func TestDaemonRejectsUnknownSessionEnvelope(t *testing.T) {
	d := &Daemon{BaseURL: "http://localhost", HTTP: &http.Client{Transport: wireTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":[]}`))}, nil
	})}}
	if _, err := d.List(context.Background()); err == nil {
		t.Fatal("unknown response treated as confirmed empty session list")
	}
}

func TestDaemonListFailsClosedOnAmbiguousNativeIdentity(t *testing.T) {
	d := &Daemon{BaseURL: "http://localhost", HTTP: &http.Client{Transport: wireTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"sessions":[{"id":"worker-1","harness":"opencode-v2","providerConversationId":"untagged","isTerminated":false}]}`))}, nil
	})}}
	if _, err := d.List(context.Background()); err == nil {
		t.Fatal("ambiguous native identity was projected as empty and treated as known liveness")
	}
}

func TestDaemonSpawnReturnsTheCreatedWorkerEvenWhenIdentityResolutionFails(t *testing.T) {
	d := &Daemon{BaseURL: "http://localhost", HTTP: &http.Client{Transport: wireTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader(`{"session":{"id":"worker-1","harness":"opencode-v2","providerConversationId":"untagged"}}`))}, nil
	})}}
	session, err := d.Spawn(context.Background(), SpawnRequest{Project: "ckadmin", Branch: "CKM-1"})
	if err == nil {
		t.Fatal("ambiguous identity was accepted")
	}
	if session.OrchestratorID != "worker-1" {
		t.Fatalf("created worker was discarded: %+v", session)
	}
}

func TestDaemonRefusesUnpinnedSubscriptionBeforeSpawn(t *testing.T) {
	posts := 0
	d := &Daemon{BaseURL: "http://localhost", HTTP: &http.Client{Transport: wireTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			posts++
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"project":{"config":{"env":{"OPENCODE_SUBSCRIPTION":"subscription-1"}}}}`))}, nil
	})}}
	if _, err := d.Spawn(context.Background(), SpawnRequest{Project: "ckadmin", Subscription: "subscription-3"}); err == nil {
		t.Fatal("ignored subscription override accepted")
	}
	if posts != 0 {
		t.Fatal("worker launched with wrong subscription")
	}
}

func TestDaemonSendUsesChatMessagesForChatWorker(t *testing.T) {
	var chatBody map[string]any
	d := &Daemon{BaseURL: "http://localhost", HTTP: &http.Client{Transport: wireTransport(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodGet:
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"session":{"id":"w1","mode":"chat","harness":"opencode-v2"}}`))}, nil
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions/w1/conversation/messages":
			_ = json.NewDecoder(r.Body).Decode(&chatBody)
			return &http.Response{StatusCode: 202, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
			return nil, nil
		}
	})}}
	if err := d.Send(context.Background(), "w1", SendRequest{Text: "fix this", ClientMessageID: "review-correction:CKM-1:abc"}); err != nil {
		t.Fatal(err)
	}
	if chatBody["text"] != "fix this" || chatBody["clientMessageId"] != "review-correction:CKM-1:abc" {
		t.Fatalf("chat body = %#v", chatBody)
	}
}

func TestDaemonSendKeepsTerminalSendForTUIWorker(t *testing.T) {
	var terminalBody map[string]any
	d := &Daemon{BaseURL: "http://localhost", HTTP: &http.Client{Transport: wireTransport(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodGet:
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"session":{"id":"w1","mode":"tui"}}`))}, nil
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions/w1/send":
			_ = json.NewDecoder(r.Body).Decode(&terminalBody)
			return &http.Response{StatusCode: 202, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
			return nil, nil
		}
	})}}
	if err := d.Send(context.Background(), "w1", SendRequest{Text: "fix this"}); err != nil {
		t.Fatal(err)
	}
	if terminalBody["message"] != "fix this" {
		t.Fatalf("terminal body = %#v", terminalBody)
	}
}

func TestCodexReviewerRefusesAnIsolatedReviewWithoutASnapshotter(t *testing.T) {
	reviewer := CodexReviewer{Binary: "codex"}
	_, err := reviewer.Review(context.Background(), ReviewRequest{Issue: "CKM-1", Commit: commitOne, Worktree: t.TempDir(), IsolatedSnapshot: true})
	if err == nil {
		t.Fatal("reviewer agreed to read a possibly dirty worktree")
	}
}
