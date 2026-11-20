package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestVisibilityRESTRejectsInvalidBeforeBrowser(t *testing.T) {
	router := setupRoutes(NewAppServer(nil, ""))
	for _, body := range []string{`{}`, `{"feed_id":"0123456789abcdef01234567","visibility":""}`, `{"feed_id":"../a","visibility":"公开可见"}`, `{"feed_id":"0123456789abcdef01234567","visibility":"friends"}`, `{"feed_id":["0123456789abcdef01234567"],"visibility":"公开可见"}`} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/feeds/visibility", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, r)
		require.Equal(t, 400, w.Code, body)
	}
}

func TestVisibilityMCPSchema(t *testing.T) {
	ctx := context.Background()
	server := InitMCPServer(&AppServer{})
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "visibility-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	defer session.Close()
	list, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	var schema jsonschema.Schema
	for _, tool := range list.Tools {
		if tool.Name == "set_note_visibility" {
			data, err := json.Marshal(tool.InputSchema)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(data, &schema))
			require.True(t, tool.Annotations.IdempotentHint)
		}
	}
	require.Contains(t, schema.Required, "feed_id")
	require.Contains(t, schema.Required, "visibility")
	require.NotContains(t, schema.Required, "xsec_token")
	resolved, err := schema.Resolve(nil)
	require.NoError(t, err)
	for _, visibility := range []string{"公开可见", "仅自己可见", "仅互关好友可见"} {
		require.NoError(t, resolved.Validate(map[string]any{"feed_id": "0123456789abcdef01234567", "visibility": visibility}))
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "set_note_visibility", Arguments: map[string]any{"feed_id": "0123456789abcdef01234567", "visibility": "invalid"}})
	require.NoError(t, err)
	require.True(t, result.IsError)
}
