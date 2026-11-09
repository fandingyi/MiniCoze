package platform

import (
	"context"
	"errors"
)

type Agent struct {
	ID, WorkspaceID, Status string
	HasVersion              bool
}
type Conversation struct {
	AgentID, UserID string
	Preview         bool
}

var ErrForbidden = errors.New("resource access denied")
var ErrUnpublished = errors.New("agent not published")

type Access struct{ Store Store }

func (a Access) Workspace(ctx context.Context, userID, workspaceID string, manage bool) error {
	role, err := a.Store.WorkspaceRole(ctx, userID, workspaceID)
	if errors.Is(err, ErrNotFound) {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	if role != "OWNER" && role != "ADMIN" && role != "MEMBER" {
		return ErrForbidden
	}
	if manage && role != "OWNER" && role != "ADMIN" {
		return ErrForbidden
	}
	return nil
}

func (a Access) Run(ctx context.Context, userID, agentID, conversationID string, preview bool) error {
	agent, err := a.Store.Agent(ctx, agentID)
	if errors.Is(err, ErrNotFound) {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	if err = a.Workspace(ctx, userID, agent.WorkspaceID, false); err != nil {
		return err
	}
	if !preview && (agent.Status != "ACTIVE" || !agent.HasVersion) {
		return ErrUnpublished
	}
	if conversationID != "" {
		conv, err := a.Store.Conversation(ctx, conversationID)
		// Existing runtime permits a client-selected ID for a new conversation.
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if conv.UserID != userID || conv.AgentID != agentID || conv.Preview != preview {
			return ErrForbidden
		}
	}
	return nil
}
