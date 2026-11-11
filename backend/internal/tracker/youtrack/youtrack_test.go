package youtrack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetIssueParsesCurrentValuesAndBundleChoices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if !strings.HasPrefix(r.URL.Path, "/issues/CKM-825") {
			t.Errorf("path = %s", r.URL.Path)
		}
		io.WriteString(w, `{
			"idReadable": "CKM-825",
			"customFields": [
				{"name":"State","value":{"name":"In Progress"},"projectCustomField":{"bundle":{"values":[{"name":"In Progress"},{"name":"Code Review"}]}}},
				{"name":"AI Reviewer Model","value":null,"projectCustomField":{"bundle":null}},
				{"name":"AI Last Submitted Commit","value":{"name":"abc"},"projectCustomField":{}}
			]
		}`)
	}))
	defer server.Close()

	client := NewClient(server.URL, "synthetic-token")
	issue, err := client.GetIssue(context.Background(), "CKM-825")
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if issue.Fields[FieldState].Value != "In Progress" {
		t.Fatalf("state = %#v", issue.Fields[FieldState])
	}
	if got := issue.Fields[FieldState].Allowed; len(got) != 2 {
		t.Fatalf("state choices = %#v", got)
	}
	reviewer := issue.Fields[FieldReviewerModel]
	if reviewer.Value != "" || len(reviewer.Allowed) != 0 {
		t.Fatalf("free-text field = %#v, want empty value and no bundle", reviewer)
	}
	if issue.Fields[FieldLastSubmitted].Value != "abc" {
		t.Fatalf("commit field = %#v", issue.Fields[FieldLastSubmitted])
	}
}

func TestUpdateFieldsPostsValuesAndCommentUsesBearer(t *testing.T) {
	var sawBearer bool
	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			sawBearer = true
		}
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/comments") && r.Method == http.MethodPost:
			posted = map[string]any{"comment": string(body)}
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":"1-1"}`)
		case strings.HasPrefix(r.URL.Path, "/issues/CKM-825") && r.Method == http.MethodPost:
			if err := json.Unmarshal(body, &posted); err != nil {
				t.Errorf("decode body: %v", err)
			}
			io.WriteString(w, `{"id":"CKM-825"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, "synthetic-token")
	if err := client.Comment(context.Background(), "CKM-825", "evidence"); err != nil {
		t.Fatalf("Comment() error = %v", err)
	}
	if err := client.UpdateFields(context.Background(), "CKM-825", map[string]string{FieldState: "In Progress"}); err != nil {
		t.Fatalf("UpdateFields() error = %v", err)
	}
	if !sawBearer {
		t.Fatal("requests did not carry a Bearer credential")
	}
	custom, ok := posted["customFields"].([]any)
	if !ok || len(custom) != 1 {
		t.Fatalf("posted body = %#v", posted)
	}
	entry := custom[0].(map[string]any)
	if entry["name"] != FieldState {
		t.Fatalf("posted field = %#v", entry)
	}
}

func TestUnauthorizedErrorDoesNotEchoTheToken(t *testing.T) {
	const token = "synthetic-token-value"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"invalid_token"}`)
	}))
	defer server.Close()

	_, err := NewClient(server.URL, token).GetIssue(context.Background(), "CKM-825")
	if err == nil {
		t.Fatal("GetIssue() accepted a 401")
	}
	var unauthorized ErrUnauthorized
	if !errors.As(err, &unauthorized) {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatal("error message leaked the token")
	}
}

func TestClientRejectsMalformedIssueIDs(t *testing.T) {
	client := NewClient("http://127.0.0.1:1", "synthetic-token")
	if _, err := client.GetIssue(context.Background(), "not-an-issue"); err == nil {
		t.Fatal("GetIssue() accepted a malformed issue id")
	}
	if err := client.Comment(context.Background(), "CKM", "x"); err == nil {
		t.Fatal("Comment() accepted a malformed issue id")
	}
}
