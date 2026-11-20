package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPublishSuccessResponsesIncludeNoteID(t *testing.T) {
	for _, response := range []any{
		PublishResponse{Title: "原稿", NoteID: "0123456789abcdef01234567", Status: "发布完成"},
		PublishVideoResponse{Title: "原稿", NoteID: "0123456789abcdef01234567", Status: "发布完成"},
	} {
		raw, err := json.Marshal(response)
		require.NoError(t, err)
		var result map[string]any
		require.NoError(t, json.Unmarshal(raw, &result))
		require.Equal(t, "0123456789abcdef01234567", result["note_id"])
		require.Equal(t, "原稿", result["title"])
		require.Equal(t, "发布完成", result["status"])
		require.NotContains(t, result, "token")
	}
	raw, err := json.Marshal(PublishResponse{Status: "发布完成"})
	require.NoError(t, err)
	require.NotContains(t, string(raw), "note_id")
}
