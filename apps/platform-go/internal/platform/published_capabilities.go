package platform

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
)

// Missing knowledge in old snapshots means no published knowledge permissions.
// Never fill missing publication data from the mutable draft bindings.
type PublishedCapabilities struct {
	Plugins []struct {
		PluginID   string `json:"pluginId"`
		PluginCode string `json:"pluginCode"`
		Status     string `json:"status"`
		AutoInvoke bool   `json:"autoInvoke"`
		Config     struct {
			Disabled []string `json:"disabledTools"`
		} `json:"config"`
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	} `json:"plugins"`
	Knowledge []struct {
		ID      string `json:"knowledgeBaseId"`
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	} `json:"knowledge"`
	Workflows []struct {
		Enabled bool `json:"enabled"`
	} `json:"workflows"`
}

func loadPublishedCapabilities(ctx context.Context, tx *sql.Tx, s *SavedSnapshot, raw []byte) error {
	var published PublishedCapabilities
	if json.Unmarshal(raw, &published) != nil {
		return ErrCapabilities
	}
	for _, workflow := range published.Workflows {
		if workflow.Enabled {
			return ErrCapabilities
		}
	}
	seen := map[string]bool{}
	add := func(c Capability, description, argument string) error {
		if seen[c.Name] || len(s.Capabilities) >= 8 {
			return ErrCapabilities
		}
		seen[c.Name] = true
		s.Capabilities = append(s.Capabilities, c)
		s.Tools = append(s.Tools, functionDefinition(c.Name, description, argument))
		return nil
	}
	for _, plugin := range published.Plugins {
		if plugin.Status != "ACTIVE" || !plugin.AutoInvoke {
			continue
		}
		for _, tool := range plugin.Tools {
			code := ""
			for _, candidate := range []string{"time_now", "echo_text"} {
				if tool.Function.Name == "system_tools__"+candidate {
					code = candidate
				}
			}
			disabled := false
			for _, v := range plugin.Config.Disabled {
				if v == code {
					disabled = true
				}
			}
			if disabled {
				continue
			}
			if plugin.PluginCode != "system_tools" || tool.Type != "function" || code == "" {
				return ErrCapabilities
			}
			var toolID string
			err := tx.QueryRowContext(ctx, `SELECT t."id" FROM "PluginTool" t JOIN "PluginDefinition" p ON p."id"=t."pluginId" WHERE p."id"=$1 AND p."workspaceId"=$2 AND p."isBuiltin"=true AND p."type"='BUILTIN' AND p."code"='system_tools' AND t."code"=$3 AND t."meta"->>'handler'=$3`, plugin.PluginID, s.WorkspaceID, code).Scan(&toolID)
			if err != nil {
				return mapNoRows(err)
			}
			description, argument := "Return current UTC time", ""
			if code == "echo_text" {
				description, argument = "Echo the supplied text", "text"
			}
			if err = add(Capability{tool.Function.Name, "builtin", plugin.PluginID, toolID, code}, description, argument); err != nil {
				return err
			}
		}
	}
	for _, knowledge := range published.Knowledge {
		if !knowledge.Enabled {
			continue
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM "KnowledgeBase" WHERE "id"=$1 AND "workspaceId"=$2)`, knowledge.ID, s.WorkspaceID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrForbidden
		}
		sum := sha256.Sum256([]byte(knowledge.ID))
		name := "knowledge__" + hex.EncodeToString(sum[:8])
		label := []rune(knowledge.Name)
		if len(label) > 300 {
			label = label[:300]
		}
		if err := add(Capability{Name: name, Kind: "knowledge", ResourceID: knowledge.ID}, "Search exact keywords in published knowledge base: "+string(label), "query"); err != nil {
			return err
		}
	}
	return nil
}
