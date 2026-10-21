package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"
)

type PageQuery struct {
	Page, Size                   int
	WorkspaceID, Status, Keyword string
}
type Catalog interface {
	Workspaces(context.Context, string, PageQuery) (json.RawMessage, error)
	WorkspaceDetail(context.Context, string, string) (json.RawMessage, error)
	Agents(context.Context, string, PageQuery) (json.RawMessage, error)
	AgentDetail(context.Context, string, string) (json.RawMessage, error)
	ConversationDetail(context.Context, string, string, string) (json.RawMessage, error)
}

func pagination(r *http.Request) (PageQuery, error) {
	q := PageQuery{Page: 1, Size: 20, WorkspaceID: r.URL.Query().Get("workspaceId"), Status: r.URL.Query().Get("status"), Keyword: r.URL.Query().Get("keyword")}
	var err error
	if v := r.URL.Query().Get("page"); v != "" {
		q.Page, err = strconv.Atoi(v)
		if err != nil {
			return q, err
		}
	}
	if v := r.URL.Query().Get("pageSize"); v != "" {
		q.Size, err = strconv.Atoi(v)
		if err != nil {
			return q, err
		}
	}
	if q.Page < 1 || q.Page > 1000000 || q.Size < 1 || q.Size > 100 || utf8.RuneCountInString(q.Keyword) > 50 {
		return q, errors.New("invalid pagination")
	}
	if q.Status != "" && q.Status != "DRAFT" && q.Status != "ACTIVE" && q.Status != "ARCHIVED" {
		return q, errors.New("invalid status")
	}
	return q, nil
}

func RegisterCatalog(mux *http.ServeMux, auth *Auth, access Access, store Catalog) {
	register := func(pattern string, fn func(context.Context, string, *http.Request) (json.RawMessage, error)) {
		mux.Handle(pattern, auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			user := r.Context().Value(userKey{}).(User)
			data, err := fn(ctx, user.ID, r)
			switch {
			case errors.Is(err, ErrForbidden):
				respond(w, 403, 40300, "无权访问该资源", nil)
			case errors.Is(err, ErrNotFound):
				respond(w, 404, 40400, "资源不存在或无权访问", nil)
			case errors.Is(err, errQuery):
				respond(w, 400, 40000, "查询参数错误", nil)
			case err != nil:
				respond(w, 503, 50000, "数据服务暂不可用", nil)
			default:
				respond(w, 200, 0, "success", data)
			}
		})))
	}
	register("GET /api/workspaces", func(ctx context.Context, user string, r *http.Request) (json.RawMessage, error) {
		q, err := pagination(r)
		if err != nil {
			return nil, errQuery
		}
		return store.Workspaces(ctx, user, q)
	})
	register("GET /api/workspaces/{workspaceId}", func(ctx context.Context, user string, r *http.Request) (json.RawMessage, error) {
		return store.WorkspaceDetail(ctx, user, r.PathValue("workspaceId"))
	})
	register("GET /api/agents", func(ctx context.Context, user string, r *http.Request) (json.RawMessage, error) {
		q, err := pagination(r)
		if err != nil || q.WorkspaceID == "" {
			return nil, errQuery
		}
		if err = access.Workspace(ctx, user, q.WorkspaceID, false); err != nil {
			return nil, err
		}
		return store.Agents(ctx, user, q)
	})
	register("GET /api/agents/{agentId}", func(ctx context.Context, user string, r *http.Request) (json.RawMessage, error) {
		return store.AgentDetail(ctx, user, r.PathValue("agentId"))
	})
	register("GET /api/workspaces/{workspaceId}/conversations/{conversationId}", func(ctx context.Context, user string, r *http.Request) (json.RawMessage, error) {
		return store.ConversationDetail(ctx, user, r.PathValue("workspaceId"), r.PathValue("conversationId"))
	})
}

var errQuery = errors.New("invalid query")
