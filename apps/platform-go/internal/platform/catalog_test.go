package platform

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

type fakeCatalog struct {
	user, workspace, id string
	query               PageQuery
	err                 error
	calls               int
}

func (f *fakeCatalog) result(user, workspace, id string, q PageQuery) (json.RawMessage, error) {
	f.calls++
	f.user = user
	f.workspace = workspace
	f.id = id
	f.query = q
	return json.RawMessage(`{"list":[],"total":0,"page":1,"pageSize":20}`), f.err
}
func (f *fakeCatalog) Workspaces(_ context.Context, u string, q PageQuery) (json.RawMessage, error) {
	return f.result(u, "", "", q)
}
func (f *fakeCatalog) WorkspaceDetail(_ context.Context, u, w string) (json.RawMessage, error) {
	return f.result(u, w, "", PageQuery{})
}
func (f *fakeCatalog) Agents(_ context.Context, u string, q PageQuery) (json.RawMessage, error) {
	return f.result(u, q.WorkspaceID, "", q)
}
func (f *fakeCatalog) AgentDetail(_ context.Context, u, id string) (json.RawMessage, error) {
	return f.result(u, "", id, PageQuery{})
}
func (f *fakeCatalog) ConversationDetail(_ context.Context, u, w, id string) (json.RawMessage, error) {
	return f.result(u, w, id, PageQuery{})
}

func TestCatalogAuthorizationAndQueries(t *testing.T) {
	auth, store := setup(t)
	token, _ := auth.Sign(store.user)
	catalog := &fakeCatalog{}
	mux := AuthHandler(auth)
	RegisterCatalog(mux, auth, Access{Store: store}, catalog)
	for _, tc := range []struct {
		path, role string
		err        error
		want       int
	}{
		{"/api/workspaces", "MEMBER", nil, 200},
		{"/api/workspaces/w1", "MEMBER", ErrNotFound, 404},
		{"/api/agents?workspaceId=w1", "MEMBER", nil, 200},
		{"/api/agents?workspaceId=w2", "", nil, 403},
		{"/api/agents?workspaceId=w1&pageSize=101", "MEMBER", nil, 400},
		{"/api/agents?workspaceId=w1&status=oops", "MEMBER", nil, 400},
		{"/api/agents", "MEMBER", nil, 400},
		{"/api/agents/a1", "MEMBER", ErrNotFound, 404},
		{"/api/workspaces/w1/conversations/c1", "MEMBER", ErrNotFound, 404},
	} {
		t.Run(tc.path+tc.role, func(t *testing.T) {
			store.role = tc.role
			catalog.err = tc.err
			catalog.calls = 0
			req := httptest.NewRequest("GET", tc.path, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, req)
			if response.Code != tc.want {
				t.Fatalf("got %d %s", response.Code, response.Body.String())
			}
			if tc.want == 403 || tc.want == 400 {
				if catalog.calls != 0 {
					t.Fatal("invalid request queried catalog")
				}
			} else if catalog.user != "u1" {
				t.Fatal("identity not supplied to query")
			}
		})
	}
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest("GET", "/api/workspaces", nil))
	if response.Code != 401 {
		t.Fatal("missing identity accepted")
	}
}
