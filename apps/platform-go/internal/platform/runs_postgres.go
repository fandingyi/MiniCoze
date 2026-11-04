package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

const runColumns = `r."id",r."conversationId",r."agentId",r."userId",r."workspaceId",r."requestHash",r."mode",r."agentVersionId",r."snapshot",r."history",r."inputText",r."status",r."usage",r."errorCode",r."createdAt",r."finishedAt",r."deadlineAt",(SELECT "content" FROM "Message" WHERE "runId"=r."id" AND "role"='ASSISTANT')`

func scanRun(row *sql.Row) (AgentRun, error) {
	var run AgentRun
	var snapshot, history, usage []byte
	err := row.Scan(&run.ID, &run.ConversationID, &run.AgentID, &run.UserID, &run.WorkspaceID, &run.RequestHash, &run.Mode, &run.AgentVersionID, &snapshot, &history, &run.InputText, &run.Status, &usage, &run.ErrorCode, &run.CreatedAt, &run.FinishedAt, &run.DeadlineAt, &run.OutputText)
	run.Usage = usage
	if err != nil {
		return run, mapNoRows(err)
	}
	if err = json.Unmarshal(snapshot, &run.Snapshot); err != nil {
		return run, err
	}
	err = json.Unmarshal(history, &run.History)
	run.Snapshot.AgentID = run.AgentID
	run.Snapshot.WorkspaceID = run.WorkspaceID
	return run, err
}
func (p Postgres) GetRun(ctx context.Context, user, id string) (AgentRun, error) {
	run, err := scanRun(p.DB.QueryRowContext(ctx, `SELECT `+runColumns+` FROM "AgentRun" r JOIN "WorkspaceMember" m ON m."workspaceId"=r."workspaceId" AND m."userId"=r."userId" JOIN "User" u ON u."id"=r."userId" AND u."status"='active' WHERE r."id"=$1 AND r."userId"=$2`, id, user))
	if err != nil {
		return run, err
	}
	if access := publicAccess(ctx); access != nil {
		if run.Snapshot.Public == nil || run.Snapshot.Public.Scope != access.Scope || run.Snapshot.Public.ChannelID != access.ChannelID || run.AgentID != access.AgentID {
			return AgentRun{}, ErrNotFound
		}
		if err = checkPublicAccess(ctx, p.DB, access); err != nil {
			return AgentRun{}, err
		}
	}
	return run, nil
}

func loadRunSnapshot(ctx context.Context, tx *sql.Tx, user string, input RunSubmission) (SavedSnapshot, *string, error) {
	var s SavedSnapshot
	var version *string
	var published []byte
	var status string
	err := tx.QueryRowContext(ctx, `SELECT a."workspaceId",a."model",a."temperature",a."systemPrompt",a."contextLimit",a."status",a."currentVersionId",v."snapshot",EXISTS(SELECT 1 FROM "AgentPluginBinding" b WHERE b."agentId"=a."id") OR EXISTS(SELECT 1 FROM "KnowledgeBaseAgentBinding" k WHERE k."agentId"=a."id") OR EXISTS(SELECT 1 FROM "AgentWorkflow" f WHERE f."agentId"=a."id") FROM "Agent" a JOIN "WorkspaceMember" m ON m."workspaceId"=a."workspaceId" AND m."userId"=$1 LEFT JOIN "AgentVersion" v ON v."id"=a."currentVersionId" AND v."agentId"=a."id" WHERE a."id"=$2 FOR SHARE OF a,m`, user, input.AgentID).Scan(&s.WorkspaceID, &s.Model.Name, &s.Model.Temperature, &s.Instructions, &s.ContextLimit, &status, &version, &published, &s.HasCapabilities)
	if err != nil {
		return s, nil, mapNoRows(err)
	}
	s.AgentID = input.AgentID
	s.SchemaVersion = 1
	s.Tools = []any{}
	s.Knowledge = []any{}
	s.MaxOutputTokens = 2048
	if len(input.Tools) > 0 {
		return s, nil, ErrCapabilities
	}
	if input.Preview {
		version = nil
		if s.HasCapabilities || (input.KnowledgeBaseID != nil && *input.KnowledgeBaseID != "") {
			if err := loadDraftCapabilities(ctx, tx, &s, input.KnowledgeBaseID); err != nil {
				return s, nil, err
			}
		}
		if input.Model != nil {
			s.Model.Name = *input.Model
		}
		if input.SystemPrompt != nil {
			s.Instructions = *input.SystemPrompt
		}
		if input.Temperature != nil {
			s.Model.Temperature = *input.Temperature
		}
		if input.MaxTokens != nil {
			s.MaxOutputTokens = *input.MaxTokens
		}
	} else {
		if status != "ACTIVE" || version == nil || len(published) == 0 {
			return s, nil, ErrUnpublished
		}
		// Legacy clients send draft settings even for published chats. Ignore those
		// settings here: only the server-owned published snapshot is executable.
		var snap struct {
			Agent struct {
				ID           string   `json:"id"`
				WorkspaceID  string   `json:"workspaceId"`
				Model        string   `json:"model"`
				SystemPrompt string   `json:"systemPrompt"`
				Temperature  *float64 `json:"temperature"`
				ContextLimit *int     `json:"contextLimit"`
			} `json:"agent"`
			Plugins   []json.RawMessage `json:"plugins"`
			Workflows []json.RawMessage `json:"workflows"`
			Channels  []json.RawMessage `json:"channels"`
		}
		if json.Unmarshal(published, &snap) != nil || snap.Agent.ID != input.AgentID || snap.Agent.WorkspaceID != s.WorkspaceID || snap.Agent.Temperature == nil || snap.Agent.ContextLimit == nil || snap.Plugins == nil || snap.Workflows == nil || snap.Channels == nil {
			return s, nil, ErrCapabilities
		}
		if err := loadPublishedCapabilities(ctx, tx, &s, published); err != nil {
			return s, nil, err
		}
		s.Model.Name = snap.Agent.Model
		s.Model.Temperature = *snap.Agent.Temperature
		s.Instructions = snap.Agent.SystemPrompt
		s.ContextLimit = *snap.Agent.ContextLimit
	}
	if s.Model.Name == "" || len(s.Model.Name) > 128 || len(s.Instructions) > 64000 || s.Model.Temperature < 0 || s.Model.Temperature > 2 || math.IsNaN(s.Model.Temperature) || math.IsInf(s.Model.Temperature, 0) || s.MaxOutputTokens < 1 || s.MaxOutputTokens > 8192 || s.ContextLimit < 0 {
		return s, nil, ErrCapabilities
	}
	if s.ContextLimit > 50 {
		s.ContextLimit = 50
	}
	return s, version, nil
}

func (p Postgres) BeginRun(ctx context.Context, user, key string, input RunSubmission) (AgentRun, bool, error) {
	var run AgentRun
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return run, false, err
	}
	defer tx.Rollback()
	access := publicAccess(ctx)
	if access != nil {
		if access.OwnerID != user || access.AgentID != input.AgentID || input.Preview {
			return run, false, ErrForbidden
		}
		if err = checkPublicAccess(ctx, tx, access); err != nil {
			return run, false, err
		}
		if input.ConversationID != "" && !strings.HasPrefix(input.ConversationID, access.prefix()) {
			return run, false, ErrNotFound
		}
		key = "public:" + digest(access.Scope+":"+key)
	} else if strings.HasPrefix(input.ConversationID, "public:") {
		return run, false, ErrForbidden
	}
	// Lock a logical request before allocating IDs. The unique index remains the
	// durable invariant; the advisory lock makes concurrent replay deterministic.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, user+":"+key); err != nil {
		return run, false, err
	}
	existing, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM "AgentRun" r WHERE r."userId"=$1 AND r."requestKey"=$2`, user, key))
	if err == nil {
		if access != nil && (existing.Snapshot.Public == nil || existing.Snapshot.Public.Scope != access.Scope) {
			return run, false, ErrNotFound
		}
		if existing.RequestHash != input.fingerprint() {
			return run, false, ErrKeyConflict
		}
		var member bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM "WorkspaceMember" WHERE "userId"=$1 AND "workspaceId"=$2)`, user, existing.WorkspaceID).Scan(&member); err != nil {
			return run, false, err
		}
		if !member {
			return run, false, ErrForbidden
		}
		return existing, true, tx.Commit()
	}
	if !errors.Is(err, ErrNotFound) {
		return run, false, err
	}
	snapshot, version, err := loadRunSnapshot(ctx, tx, user, input)
	if err != nil {
		return run, false, err
	}
	snapshot.Public = access
	run = AgentRun{AgentID: input.AgentID, UserID: user, WorkspaceID: snapshot.WorkspaceID, RequestHash: input.fingerprint(), Mode: "published", AgentVersionID: version, Snapshot: snapshot, History: []HistoryMessage{}, InputText: input.Message, Status: "CREATED"}
	if input.Preview {
		run.Mode = "draft"
	}
	run.ID, err = randomID()
	if err != nil {
		return run, false, err
	}
	run.ConversationID = input.ConversationID
	if run.ConversationID == "" {
		run.ConversationID, err = randomID()
		if err != nil {
			return run, false, err
		}
		if access != nil {
			run.ConversationID = access.prefix() + run.ConversationID
		}
		title := []rune(input.Message)
		if len(title) > 50 {
			title = title[:50]
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO "Conversation"("id","agentId","userId","title","isPreview","createdAt","updatedAt") VALUES($1,$2,$3,$4,$5,NOW(),NOW())`, run.ConversationID, input.AgentID, user, string(title), input.Preview)
		if err != nil {
			return run, false, err
		}
	} else {
		var owner, agent string
		var preview bool
		err = tx.QueryRowContext(ctx, `SELECT "userId","agentId","isPreview" FROM "Conversation" WHERE "id"=$1 FOR UPDATE`, run.ConversationID).Scan(&owner, &agent, &preview)
		if errors.Is(err, sql.ErrNoRows) {
			return run, false, ErrNotFound
		}
		if err != nil {
			return run, false, err
		}
		if owner != user || agent != input.AgentID || preview != input.Preview {
			return run, false, ErrForbidden
		}
	}
	var busy bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM "AgentRun" WHERE "conversationId"=$1 AND "status" IN ('CREATED','RUNNING'))`, run.ConversationID).Scan(&busy); err != nil {
		return run, false, err
	}
	if busy {
		return run, false, ErrRunBusy
	}
	if err = acquirePublicQuota(ctx, tx, access); err != nil {
		return run, false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT m."role",LEFT(m."content",32000) FROM "Message" m LEFT JOIN "AgentRun" r ON r."id"=m."runId" WHERE m."conversationId"=$1 AND m."role" IN ('USER','ASSISTANT') AND (m."runId" IS NULL OR r."status"='COMPLETED') ORDER BY m."createdAt" DESC,m."id" DESC LIMIT $2`, run.ConversationID, snapshot.ContextLimit)
	if err != nil {
		return run, false, err
	}
	total := 0
	for rows.Next() {
		var role, content string
		if err = rows.Scan(&role, &content); err != nil {
			rows.Close()
			return run, false, err
		}
		if total+len(content) > 64000 {
			break
		}
		total += len(content)
		if role == "USER" {
			role = "user"
		} else {
			role = "assistant"
		}
		run.History = append(run.History, HistoryMessage{role, content})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return run, false, err
	}
	for i, j := 0, len(run.History)-1; i < j; i, j = i+1, j-1 {
		run.History[i], run.History[j] = run.History[j], run.History[i]
	}
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		return run, false, err
	}
	historyJSON, _ := json.Marshal(run.History)
	err = tx.QueryRowContext(ctx, `INSERT INTO "AgentRun"("id","conversationId","agentId","userId","workspaceId","requestKey","requestHash","mode","agentVersionId","snapshot","history","inputText","status","createdAt","updatedAt","deadlineAt") VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'CREATED',NOW(),NOW(),NOW()+interval '150 seconds') RETURNING "createdAt","deadlineAt"`, run.ID, run.ConversationID, run.AgentID, user, run.WorkspaceID, key, run.RequestHash, run.Mode, version, string(snapshotJSON), string(historyJSON), input.Message).Scan(&run.CreatedAt, &run.DeadlineAt)
	if err != nil {
		return run, false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO "Message"("id","conversationId","role","content","runId","createdAt") VALUES($1,$2,'USER',$3,$4,GREATEST(clock_timestamp(),(SELECT MAX("createdAt")+interval '1 millisecond' FROM "Message" WHERE "conversationId"=$2)))`, run.ID+"-user", run.ConversationID, input.Message, run.ID); err != nil {
		return run, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE "Conversation" SET "updatedAt"=NOW() WHERE "id"=$1`, run.ConversationID); err != nil {
		return run, false, err
	}
	return run, false, tx.Commit()
}

func (p Postgres) StartRun(ctx context.Context, id string) (AgentRun, error) {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return AgentRun{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE "AgentRun" SET "status"='RUNNING',"startedAt"=NOW(),"updatedAt"=NOW() WHERE "id"=$1 AND "status"='CREATED' AND "deadlineAt">NOW()`, id); err != nil {
		return AgentRun{}, err
	}
	run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM "AgentRun" r WHERE r."id"=$1`, id))
	if err != nil {
		return run, err
	}
	return run, tx.Commit()
}
func (p Postgres) FinishRun(ctx context.Context, id, status string, result RunResult) (AgentRun, error) {
	if status != "COMPLETED" && status != "FAILED" && status != "CANCELLED" {
		return AgentRun{}, errors.New("invalid terminal state")
	}
	if !utf8.ValidString(result.Text) {
		return AgentRun{}, errors.New("invalid UTF-8")
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return AgentRun{}, err
	}
	defer tx.Rollback()
	run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM "AgentRun" r WHERE r."id"=$1 FOR UPDATE OF r`, id))
	if err != nil {
		return run, err
	}
	if run.terminal() {
		return run, tx.Commit()
	}
	if status == "COMPLETED" {
		if run.Status != "RUNNING" {
			return run, errors.New("run not running")
		}
		var usage any
		if len(result.Usage) > 0 {
			if !json.Valid(result.Usage) {
				return run, errors.New("invalid usage")
			}
			usage = string(result.Usage)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO "Message"("id","conversationId","role","content","model","tokenUsage","runId","createdAt") VALUES($1,$2,'ASSISTANT',$3,$4,$5,$6,GREATEST(clock_timestamp(),(SELECT MAX("createdAt")+interval '1 millisecond' FROM "Message" WHERE "conversationId"=$2)))`, run.assistantID(), run.ConversationID, result.Text, run.Snapshot.Model.Name, usage, run.ID)
		if err != nil {
			return run, err
		}
	}
	var usage any
	if status == "COMPLETED" && len(result.Usage) > 0 {
		usage = string(result.Usage)
	}
	var code any
	if status != "COMPLETED" {
		code = result.ErrorCode
		if code == "" {
			code = fmt.Sprintf("RUN_%s", status)
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE "AgentRun" SET "status"=$2,"usage"=$3,"errorCode"=$4,"finishedAt"=NOW(),"updatedAt"=NOW() WHERE "id"=$1`, id, status, usage, code); err != nil {
		return run, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE "Conversation" SET "updatedAt"=NOW() WHERE "id"=$1`, run.ConversationID); err != nil {
		return run, err
	}
	run, err = scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM "AgentRun" r WHERE r."id"=$1`, id))
	if err != nil {
		return run, err
	}
	return run, tx.Commit()
}
func (p Postgres) ExpireRuns(ctx context.Context) (int64, error) {
	result, err := p.DB.ExecContext(ctx, `WITH expired AS (SELECT "id" FROM "AgentRun" WHERE "status" IN ('CREATED','RUNNING') AND "deadlineAt"<=NOW() ORDER BY "deadlineAt" LIMIT 100 FOR UPDATE SKIP LOCKED) UPDATE "AgentRun" r SET "status"='FAILED',"errorCode"='RUN_EXPIRED',"finishedAt"=NOW(),"updatedAt"=NOW() FROM expired WHERE r."id"=expired."id"`)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// Cleanup must survive a disconnected HTTP request; DB operations remain bounded.
func finishDetached(store RunStore, id, status string, result RunResult) (AgentRun, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return store.FinishRun(ctx, id, status, result)
}
