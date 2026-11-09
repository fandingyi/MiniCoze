package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

var ErrKeyConflict = errors.New("idempotency key used for different input")
var ErrRunBusy = errors.New("conversation has an active run")
var ErrCapabilities = errors.New("unsupported capabilities or configuration")

type RunSubmission struct {
	AgentID         string            `json:"agentId"`
	Message         string            `json:"message"`
	ConversationID  string            `json:"conversationId,omitempty"`
	Preview         bool              `json:"preview,omitempty"`
	Model           *string           `json:"model,omitempty"`
	SystemPrompt    *string           `json:"systemPrompt,omitempty"`
	Temperature     *float64          `json:"temperature,omitempty"`
	MaxTokens       *int              `json:"maxTokens,omitempty"`
	Tools           []json.RawMessage `json:"tools,omitempty"`
	KnowledgeBaseID *string           `json:"knowledgeBaseId,omitempty"`
}

func (s RunSubmission) fingerprint() string {
	body, _ := json.Marshal(s)
	hash := sha256.Sum256(body)
	return hex.EncodeToString(hash[:])
}

type HistoryMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type SavedSnapshot struct {
	Public *PublicAccess `json:"public,omitempty"`
	PreviewSnapshot
	Capabilities    []Capability `json:"capabilities,omitempty"`
	ContextLimit    int          `json:"context_limit"`
	MaxOutputTokens int          `json:"max_output_tokens"`
}
type AgentRun struct {
	ID             string           `json:"id"`
	ConversationID string           `json:"conversationId"`
	AgentID        string           `json:"agentId"`
	UserID         string           `json:"-"`
	WorkspaceID    string           `json:"workspaceId"`
	RequestHash    string           `json:"-"`
	Mode           string           `json:"mode"`
	AgentVersionID *string          `json:"agentVersionId"`
	Snapshot       SavedSnapshot    `json:"-"`
	History        []HistoryMessage `json:"-"`
	InputText      string           `json:"-"`
	Status         string           `json:"status"`
	Usage          json.RawMessage  `json:"usage"`
	ErrorCode      *string          `json:"errorCode"`
	CreatedAt      time.Time        `json:"createdAt"`
	FinishedAt     *time.Time       `json:"finishedAt"`
	DeadlineAt     time.Time        `json:"deadlineAt"`
	OutputText     *string          `json:"output"`
}

func (r AgentRun) terminal() bool {
	return r.Status == "COMPLETED" || r.Status == "FAILED" || r.Status == "CANCELLED"
}
func (r AgentRun) assistantID() string { return r.ID + "-assistant" }

type RunResult struct {
	Text      string
	Usage     json.RawMessage
	ErrorCode string
}
type RunStore interface {
	BeginRun(context.Context, string, string, RunSubmission) (AgentRun, bool, error)
	GetRun(context.Context, string, string) (AgentRun, error)
	StartRun(context.Context, string) (AgentRun, error)
	FinishRun(context.Context, string, string, RunResult) (AgentRun, error)
	ExpireRuns(context.Context) (int64, error)
}
