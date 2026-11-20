package main

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestLoginSessionsRejectStalePersistence(t *testing.T) {
	var sessions loginSessions
	old := sessions.start(func() {})
	fresh := sessions.start(func() {})
	calls := 0
	save := func() error { calls++; return nil }
	require.Error(t, sessions.runIfCurrent(old, save))
	require.Zero(t, calls)
	require.NoError(t, sessions.runIfCurrent(fresh, save))
	require.Equal(t, 1, calls)
	sessions.finish(fresh)
	require.Error(t, sessions.runIfCurrent(fresh, save))
	require.Equal(t, 1, calls)
}

func TestCreatorLoginMCPRegistered(t *testing.T) {
	ctx := context.Background()
	server := InitMCPServer(&AppServer{})
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	require.NoError(t, err)
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "login-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	require.NoError(t, err)
	defer session.Close()
	list, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	found := false
	for _, tool := range list.Tools {
		if tool.Name == "get_creator_login_qrcode" {
			found = true
		}
	}
	require.True(t, found)
}
