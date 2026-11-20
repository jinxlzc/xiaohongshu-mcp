package main

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

type SetNoteVisibilityRequest struct {
	FeedID     string `json:"feed_id" binding:"required" jsonschema:"已发布笔记的24位ID，仅支持当前账号的单篇笔记"`
	XsecToken  string `json:"xsec_token,omitempty" jsonschema:"可选；创作者管理页路径无需此令牌"`
	Visibility string `json:"visibility" binding:"required" jsonschema:"目标可见范围：公开可见、仅自己可见、仅互关好友可见"`
}

func (s *XiaohongshuService) SetNoteVisibility(ctx context.Context, req SetNoteVisibilityRequest) (*xiaohongshu.NoteVisibilityResult, error) {
	if err := xiaohongshu.ValidateNoteVisibility(req.FeedID, req.Visibility); err != nil {
		return nil, err
	}
	b := newBrowser()
	defer b.Close()
	page := b.NewPage()
	defer page.Close()
	result, err := xiaohongshu.SetNoteVisibility(ctx, page, req.FeedID, req.Visibility)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *AppServer) setNoteVisibilityHandler(c *gin.Context) {
	var req SetNoteVisibilityRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "INVALID_REQUEST", "请求参数错误", err.Error())
		return
	}
	if err := xiaohongshu.ValidateNoteVisibility(req.FeedID, req.Visibility); err != nil {
		respondError(c, http.StatusBadRequest, "INVALID_REQUEST", "请求参数错误", err.Error())
		return
	}
	result, err := s.xiaohongshuService.SetNoteVisibility(c.Request.Context(), req)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "SET_VISIBILITY_FAILED", "修改可见范围失败", err.Error())
		return
	}
	respondSuccess(c, result, "笔记可见范围已确认")
}

func registerNoteVisibilityTools(server *mcp.Server, app *AppServer) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "set_note_visibility", Description: "修改自己已发布的单篇笔记可见范围。已是目标状态返回 skipped，修改后重新加载确认，返回 before/after。需要创作者中心登录。",
		InputSchema: map[string]any{"type": "object", "required": []string{"feed_id", "visibility"}, "properties": map[string]any{
			"feed_id":    map[string]any{"type": "string", "pattern": "^[0-9a-fA-F]{24}$", "description": "已发布笔记 ID"},
			"xsec_token": map[string]any{"type": "string", "description": "可选，管理页无需此令牌"},
			"visibility": map[string]any{"type": "string", "enum": []string{"公开可见", "仅自己可见", "仅互关好友可见"}},
		}},
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: boolPtr(true)},
	}, withPanicRecovery("set_note_visibility", func(ctx context.Context, _ *mcp.CallToolRequest, req SetNoteVisibilityRequest) (*mcp.CallToolResult, any, error) {
		result, err := app.xiaohongshuService.SetNoteVisibility(ctx, req)
		if err != nil {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil, nil
		}
		data, _ := json.Marshal(result)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil, nil
	}))
}
