package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

func readJSON(row *sql.Row) (json.RawMessage, error) {
	var data json.RawMessage
	err := row.Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return data, err
}

// Prisma timestamps are stored as UTC timestamp without time zone.
const workspaceJSON = `to_jsonb(w) || jsonb_build_object('role',m."role",'createdAt',to_char(w."createdAt"+interval '8 hours','YYYY-MM-DD"T"HH24:MI:SS.MS')||'+08:00','updatedAt',to_char(w."updatedAt"+interval '8 hours','YYYY-MM-DD"T"HH24:MI:SS.MS')||'+08:00')`
const agentJSON = `(to_jsonb(a)-'currentVersionId') || jsonb_build_object('createdAt',to_char(a."createdAt"+interval '8 hours','YYYY-MM-DD"T"HH24:MI:SS.MS')||'+08:00','updatedAt',to_char(a."updatedAt"+interval '8 hours','YYYY-MM-DD"T"HH24:MI:SS.MS')||'+08:00')`

func (p Postgres) Workspaces(ctx context.Context, user string, q PageQuery) (json.RawMessage, error) {
	return readJSON(p.DB.QueryRowContext(ctx, `WITH visible AS (SELECT `+workspaceJSON+` AS data,w."updatedAt",w."id" FROM "Workspace" w JOIN "WorkspaceMember" m ON m."workspaceId"=w."id" WHERE m."userId"=$1), page AS (SELECT data FROM visible ORDER BY "updatedAt" DESC,"id" LIMIT $2 OFFSET $3) SELECT jsonb_build_object('list',COALESCE((SELECT jsonb_agg(data) FROM page),'[]'::jsonb),'total',(SELECT count(*) FROM visible),'page',$4::int,'pageSize',$2::int)`, user, q.Size, (q.Page-1)*q.Size, q.Page))
}
func (p Postgres) WorkspaceDetail(ctx context.Context, user, id string) (json.RawMessage, error) {
	return readJSON(p.DB.QueryRowContext(ctx, `SELECT `+workspaceJSON+` FROM "Workspace" w JOIN "WorkspaceMember" m ON m."workspaceId"=w."id" WHERE m."userId"=$1 AND w."id"=$2`, user, id))
}
func (p Postgres) Agents(ctx context.Context, user string, q PageQuery) (json.RawMessage, error) {
	return readJSON(p.DB.QueryRowContext(ctx, `WITH visible AS (SELECT `+agentJSON+` AS data,a."updatedAt",a."id" FROM "Agent" a JOIN "WorkspaceMember" m ON m."workspaceId"=a."workspaceId" WHERE m."userId"=$1 AND a."workspaceId"=$2 AND ($3='' OR a."status"::text=$3) AND ($4='' OR strpos(lower(a."name"),lower($4))>0 OR strpos(lower(COALESCE(a."description",'')),lower($4))>0)), page AS (SELECT data FROM visible ORDER BY "updatedAt" DESC,"id" LIMIT $5 OFFSET $6) SELECT jsonb_build_object('list',COALESCE((SELECT jsonb_agg(data) FROM page),'[]'::jsonb),'total',(SELECT count(*) FROM visible),'page',$7::int,'pageSize',$5::int)`, user, q.WorkspaceID, q.Status, q.Keyword, q.Size, (q.Page-1)*q.Size, q.Page))
}
func (p Postgres) AgentDetail(ctx context.Context, user, id string) (json.RawMessage, error) {
	return readJSON(p.DB.QueryRowContext(ctx, `SELECT `+agentJSON+` FROM "Agent" a JOIN "WorkspaceMember" m ON m."workspaceId"=a."workspaceId" WHERE m."userId"=$1 AND a."id"=$2`, user, id))
}
func (p Postgres) ConversationDetail(ctx context.Context, user, workspace, id string) (json.RawMessage, error) {
	return readJSON(p.DB.QueryRowContext(ctx, `SELECT to_jsonb(c) || jsonb_build_object('createdAt',to_char(c."createdAt",'YYYY-MM-DD"T"HH24:MI:SS.MS')||'Z','updatedAt',to_char(c."updatedAt",'YYYY-MM-DD"T"HH24:MI:SS.MS')||'Z','agent',to_jsonb(a),'messages',COALESCE((SELECT jsonb_agg(to_jsonb(msg) || jsonb_build_object('createdAt',to_char(msg."createdAt",'YYYY-MM-DD"T"HH24:MI:SS.MS')||'Z') ORDER BY msg."createdAt",msg."id") FROM "Message" msg WHERE msg."conversationId"=c."id"),'[]'::jsonb)) FROM "Conversation" c JOIN "Agent" a ON a."id"=c."agentId" JOIN "WorkspaceMember" m ON m."workspaceId"=a."workspaceId" AND m."userId"=$1 WHERE c."userId"=$1 AND a."workspaceId"=$2 AND c."id"=$3`, user, workspace, id))
}
