package platform

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type Postgres struct {
	DB  *sql.DB
	DSN string // Dedicated pgx connections use this for LISTEN/NOTIFY.
}

const runCancellationChannel = "minicoze_run_cancel"

// ListenRunCancellations is best-effort latency reduction, not state
// authority: the per-run database watcher remains in place so a notification
// lost during reconnect cannot leave a run executing indefinitely.
func (p Postgres) ListenRunCancellations(ctx context.Context, onCancel func(string)) {
	if p.DSN == "" || onCancel == nil {
		return
	}
	for ctx.Err() == nil {
		conn, err := pgx.Connect(ctx, p.DSN)
		if err == nil {
			_, err = conn.Exec(ctx, "LISTEN "+runCancellationChannel)
		}
		if err == nil {
			for ctx.Err() == nil {
				notification, waitErr := conn.WaitForNotification(ctx)
				if waitErr != nil {
					break
				}
				if notification.Channel == runCancellationChannel && len(notification.Payload) > 0 && len(notification.Payload) <= 128 {
					onCancel(notification.Payload)
				}
			}
		}
		if conn != nil {
			_ = conn.Close(context.Background())
		}
		if ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
}

func mapNoRows(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

const userColumns = `"id","username","email","passwordHash",COALESCE("phone",''),COALESCE("bio",''),"avatarUrl","status","createdAt","updatedAt"`

func scanUser(row *sql.Row) (User, error) {
	var u User
	var created, updated time.Time
	err := row.Scan(&u.ID, &u.Username, &u.Email, &u.PasswordHash, &u.Phone, &u.Bio, &u.AvatarURL, &u.Status, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	if err != nil {
		return u, err
	}
	zone := time.FixedZone("Asia/Shanghai", 8*3600)
	u.CreatedAt = created.In(zone).Format("2006-01-02T15:04:05.000Z07:00")
	u.UpdatedAt = updated.In(zone).Format("2006-01-02T15:04:05.000Z07:00")
	return u, nil
}
func (p Postgres) UserByEmail(ctx context.Context, email string) (User, error) {
	return scanUser(p.DB.QueryRowContext(ctx, `SELECT `+userColumns+` FROM "User" WHERE "email"=$1`, email))
}
func (p Postgres) UserByID(ctx context.Context, id string) (User, error) {
	return scanUser(p.DB.QueryRowContext(ctx, `SELECT `+userColumns+` FROM "User" WHERE "id"=$1`, id))
}
func (p Postgres) WorkspaceRole(ctx context.Context, userID, workspaceID string) (string, error) {
	var role string
	err := p.DB.QueryRowContext(ctx, `SELECT "role" FROM "WorkspaceMember" WHERE "userId"=$1 AND "workspaceId"=$2`, userID, workspaceID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return role, err
}
func (p Postgres) Agent(ctx context.Context, id string) (Agent, error) {
	var a Agent
	err := p.DB.QueryRowContext(ctx, `SELECT a."id",a."workspaceId",a."status", EXISTS(SELECT 1 FROM "AgentVersion" v WHERE v."id"=a."currentVersionId") FROM "Agent" a WHERE a."id"=$1`, id).Scan(&a.ID, &a.WorkspaceID, &a.Status, &a.HasVersion)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return a, err
}
func (p Postgres) Conversation(ctx context.Context, id string) (Conversation, error) {
	var c Conversation
	err := p.DB.QueryRowContext(ctx, `SELECT "agentId","userId","isPreview" FROM "Conversation" WHERE "id"=$1`, id).Scan(&c.AgentID, &c.UserID, &c.Preview)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return c, err
}
func randomID() (string, error) {
	var b [16]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}
func (p Postgres) EnsureWorkspace(ctx context.Context, userID string) error {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var locked string
	if err = tx.QueryRowContext(ctx, `SELECT "id" FROM "User" WHERE "id"=$1 FOR UPDATE`, userID).Scan(&locked); err != nil {
		return err
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM "WorkspaceMember" WHERE "userId"=$1)`, userID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		workspaceID, err := randomID()
		if err != nil {
			return err
		}
		memberID, err := randomID()
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO "Workspace" ("id","name","description","ownerId","createdAt","updatedAt") VALUES ($1,'我的工作空间','默认工作空间',$2,NOW(),NOW())`, workspaceID, userID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO "WorkspaceMember" ("id","workspaceId","userId","role","createdAt","updatedAt") VALUES ($1,$2,$3,'OWNER',NOW(),NOW())`, memberID, workspaceID, userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
