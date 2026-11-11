package coordinator

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type wireTransport func(*http.Request) (*http.Response, error)

func (f wireTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestDaemonReadsActualSessionEnvelopeAndNativeIdentity(t *testing.T) {
	d := &Daemon{HTTP: &http.Client{Transport: wireTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"sessions":[{"id":"worker-1","agentSessionId":"ses-native","isTerminated":false}]}`
		if !strings.Contains(r.URL.RawQuery, "includeTerminated") {
			body = `{"session":{"id":"worker-1","agentSessionId":"ses-native"}}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}, BaseURL: "http://localhost"}
	sessions, err := d.List(context.Background())
	if err != nil || len(sessions) != 1 {
		t.Fatalf("running worker disappeared: sessions=%v err=%v", sessions, err)
	}
	status, err := d.Get(context.Background(), "worker-1")
	if err != nil || status.NativeID != "ses-native" {
		t.Fatalf("native identity missing: %+v err=%v", status, err)
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
