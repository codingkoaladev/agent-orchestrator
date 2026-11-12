// Package youtrack is the focused bridge between an Orchestrator daemon and
// YouTrack tickets. It owns two things only:
//
//   - the REST calls that read a ticket's custom fields and write the AI
//     workflow fields plus one evidence comment, and
//   - the workflow guards that decide whether a transition is allowed.
//
// It deliberately does not discover tickets, choose models, or launch agents:
// the coordinator does that and reports the facts here.
package youtrack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the YouTrack REST root for the owner's instance.
const DefaultBaseURL = "https://codingkoala.youtrack.cloud/api"

// Client is a minimal YouTrack REST client. The token is held in memory only;
// no method writes it to an error, log line or URL.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewClient builds a client. An empty base URL selects DefaultBaseURL.
func NewClient(baseURL, token string) *Client {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// FieldKind is the write shape YouTrack expects for a custom field. The REST
// API rejects a text value on a bundle field and vice versa.
type FieldKind string

const (
	KindState FieldKind = "state"
	KindEnum  FieldKind = "enum"
	KindText  FieldKind = "text"
	KindOther FieldKind = "other"
)

// fieldKind maps a project custom field type onto a write shape.
func fieldKind(projectFieldType string) FieldKind {
	switch {
	case strings.Contains(projectFieldType, "StateProjectCustomField"):
		return KindState
	case strings.Contains(projectFieldType, "EnumProjectCustomField"):
		return KindEnum
	case strings.Contains(projectFieldType, "SimpleProjectCustomField"):
		return KindText
	default:
		return KindOther
	}
}

// Field is one custom field as YouTrack reported it.
type Field struct {
	Name   string
	Value  string
	Kind   FieldKind
	Exists bool
	// Allowed lists bundle values when the field is bundle-backed. An empty
	// slice on a bundle field means "free text".
	Allowed []string
}

// fieldValue reads a custom field value that YouTrack returns as an object
// ({"name": ...}) for bounded fields, a bare string for text fields, or a
// millisecond epoch for date-time fields. presentation is preferred for dates
// because it is already in the project user's timezone.
func fieldValue(raw json.RawMessage, presentation string) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var object struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &object); err == nil && object.Name != "" {
		return object.Name
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var millis float64
	if err := json.Unmarshal(raw, &millis); err == nil && millis > 0 {
		// Use the exact instant, not a locale-formatted presentation string.
		return time.UnixMilli(int64(millis)).UTC().Format(time.RFC3339)
	}
	if strings.TrimSpace(presentation) != "" {
		return presentation
	}
	return ""
}

// Issue is the subset of a ticket this bridge reads and writes.
type Issue struct {
	ID          string
	Summary     string
	Description string
	Fields      map[string]Field
}

// ErrUnauthorized reports a rejected credential without echoing it.
type ErrUnauthorized struct{ Status int }

func (e ErrUnauthorized) Error() string {
	return fmt.Sprintf("youtrack: credential rejected (HTTP %d); check the injected token", e.Status)
}

// apiError keeps a bounded, credential-free summary of a failed response.
type apiError struct {
	Status int
	Op     string
	Body   string
}

func (e apiError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 200 {
		body = body[:200] + "…"
	}
	if body == "" {
		return fmt.Sprintf("youtrack: %s failed with HTTP %d", e.Op, e.Status)
	}
	return fmt.Sprintf("youtrack: %s failed with HTTP %d: %s", e.Op, e.Status, body)
}

func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	if strings.TrimSpace(c.Token) == "" {
		return nil, fmt.Errorf("youtrack: no token configured")
	}
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("youtrack: encode request: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, payload)
	if err != nil {
		return nil, fmt.Errorf("youtrack: build request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return nil, fmt.Errorf("youtrack: %s request failed: %w", method, redact(err))
	}
	defer response.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if readErr != nil {
		return nil, fmt.Errorf("youtrack: read response: %w", readErr)
	}
	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return nil, ErrUnauthorized{Status: response.StatusCode}
	case response.StatusCode < 200 || response.StatusCode >= 300:
		return nil, apiError{Status: response.StatusCode, Op: method + " " + path, Body: string(data)}
	}
	return data, nil
}

// redact removes the configured token from an error chain, so a transport that
// quotes a URL or header cannot leak it to logs.
func redact(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	if idx := strings.Index(message, "Authorization"); idx >= 0 {
		message = message[:idx] + "Authorization: [redacted]"
	}
	return fmt.Errorf("%s", message)
}

const issueFieldsQuery = "idReadable,summary,description,customFields(name,$type,value,presentation,projectCustomField(\"$type\",field(name),bundle(values(name))))"

type issueResponse struct {
	IDReadable   string `json:"idReadable"`
	Summary      string `json:"summary"`
	Description  string `json:"description"`
	CustomFields []struct {
		Name               string          `json:"name"`
		Value              json.RawMessage `json:"value"`
		Presentation       string          `json:"presentation"`
		ProjectCustomField struct {
			Type   string `json:"$type"`
			Bundle *struct {
				Values []struct {
					Name string `json:"name"`
				} `json:"values"`
			} `json:"bundle"`
		} `json:"projectCustomField"`
	} `json:"customFields"`
}

// GetIssue reads the ticket's custom fields, including bundle choices.
func (c *Client) GetIssue(ctx context.Context, issueID string) (Issue, error) {
	if !ValidIssueID(issueID) {
		return Issue{}, fmt.Errorf("youtrack: invalid issue id %q", issueID)
	}
	path := "/issues/" + url.PathEscape(issueID) + "?fields=" + url.QueryEscape(issueFieldsQuery)
	data, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return Issue{}, err
	}
	var decoded issueResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return Issue{}, fmt.Errorf("youtrack: decode issue: %w", err)
	}
	issue := Issue{ID: decoded.IDReadable, Summary: decoded.Summary, Description: decoded.Description, Fields: map[string]Field{}}
	for _, field := range decoded.CustomFields {
		value := fieldValue(field.Value, field.Presentation)
		var allowed []string
		if field.ProjectCustomField.Bundle != nil {
			allowed = make([]string, 0, len(field.ProjectCustomField.Bundle.Values))
			for _, choice := range field.ProjectCustomField.Bundle.Values {
				allowed = append(allowed, choice.Name)
			}
		}
		issue.Fields[field.Name] = Field{
			Name:    field.Name,
			Value:   value,
			Kind:    fieldKind(field.ProjectCustomField.Type),
			Exists:  true,
			Allowed: allowed,
		}
	}
	return issue, nil
}

// Value is one field write: the text to store and the shape YouTrack expects.
type Value struct {
	Text string
	Kind FieldKind
}

// UpdateFields writes custom fields. Values must already be validated by the
// workflow package; this method only transports them in the shape each field
// type accepts.
func (c *Client) UpdateFields(ctx context.Context, issueID string, fields map[string]Value) error {
	if len(fields) == 0 {
		return nil
	}
	if !ValidIssueID(issueID) {
		return fmt.Errorf("youtrack: invalid issue id %q", issueID)
	}
	custom := make([]map[string]any, 0, len(fields))
	for name, value := range fields {
		entry := map[string]any{"name": name}
		switch value.Kind {
		case KindState:
			entry["$type"] = "StateIssueCustomField"
			entry["value"] = map[string]any{"$type": "StateBundleElement", "name": value.Text}
		case KindEnum:
			entry["$type"] = "SingleEnumIssueCustomField"
			entry["value"] = map[string]any{"$type": "EnumBundleElement", "name": value.Text}
		case KindText:
			entry["$type"] = "SimpleIssueCustomField"
			entry["value"] = value.Text
		default:
			return fmt.Errorf("youtrack: field %q has an unsupported custom-field type", name)
		}
		custom = append(custom, entry)
	}
	body := map[string]any{"$type": "Issue", "customFields": custom}
	_, err := c.do(ctx, http.MethodPost, "/issues/"+url.PathEscape(issueID)+"?fields=id", body)
	return err
}

// RecentComments returns the newest comment bodies, oldest first, bounded by
// limit. Apply uses it to keep a retried transition from posting duplicate
// evidence.
func (c *Client) RecentComments(ctx context.Context, issueID string, limit int) ([]string, error) {
	if !ValidIssueID(issueID) {
		return nil, fmt.Errorf("youtrack: invalid issue id %q", issueID)
	}
	if limit <= 0 {
		limit = 5
	}
	path := "/issues/" + url.PathEscape(issueID) + "/comments?fields=text&$top=" + strconv.Itoa(limit)
	data, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var comments []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &comments); err != nil {
		return nil, fmt.Errorf("youtrack: decode comments: %w", err)
	}
	bodies := make([]string, 0, len(comments))
	for _, comment := range comments {
		bodies = append(bodies, comment.Text)
	}
	return bodies, nil
}

// Comment appends one Markdown comment.
func (c *Client) Comment(ctx context.Context, issueID, text string) error {
	if !ValidIssueID(issueID) {
		return fmt.Errorf("youtrack: invalid issue id %q", issueID)
	}
	body := map[string]any{"text": text}
	_, err := c.do(ctx, http.MethodPost, "/issues/"+url.PathEscape(issueID)+"/comments?fields=id", body)
	return err
}
