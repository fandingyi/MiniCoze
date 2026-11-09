package platform

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type StoredRunEvent struct {
	RunID     string
	Sequence  int
	Type      string
	Payload   map[string]any
	CreatedAt time.Time
}

// RunEventStore is deliberately separate from RunStore: non-Postgres test
// stores retain the regular stream behavior, while production persists every
// browser-visible event before attempting to deliver it.
type RunEventStore interface {
	AppendRunEvent(context.Context, StoredRunEvent) error
	ListRunEvents(context.Context, string, string, int) ([]StoredRunEvent, error)
}

func (p Postgres) AppendRunEvent(ctx context.Context, event StoredRunEvent) error {
	if event.RunID == "" || event.Sequence < 1 || len(event.Type) > 64 || event.Type == "" {
		return errors.New("invalid run event")
	}
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		return err
	}
	_, err = p.DB.ExecContext(ctx, `INSERT INTO "AgentRunEvent"("runId","sequence","type","payload") VALUES($1,$2,$3,$4)`, event.RunID, event.Sequence, event.Type, string(payload))
	return err
}

func (p Postgres) ListRunEvents(ctx context.Context, userID, runID string, after int) ([]StoredRunEvent, error) {
	if after < 0 {
		return nil, errors.New("invalid event sequence")
	}
	rows, err := p.DB.QueryContext(ctx, `SELECT e."runId",e."sequence",e."type",e."payload",e."createdAt"
		FROM "AgentRunEvent" e JOIN "AgentRun" r ON r."id"=e."runId"
		WHERE e."runId"=$1 AND r."userId"=$2 AND e."sequence">$3
		ORDER BY e."sequence" ASC LIMIT 4096`, runID, userID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]StoredRunEvent, 0)
	for rows.Next() {
		var event StoredRunEvent
		var payload []byte
		if err := rows.Scan(&event.RunID, &event.Sequence, &event.Type, &payload, &event.CreatedAt); err != nil {
			return nil, err
		}
		if json.Unmarshal(payload, &event.Payload) != nil {
			return nil, errors.New("stored run event is invalid")
		}
		result = append(result, event)
	}
	return result, rows.Err()
}
