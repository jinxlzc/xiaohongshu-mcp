package main

import (
	"context"
	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"strings"
)

func (s *AppServer) getCreatorLoginQrcodeHandler(c *gin.Context) {
	result, err := s.xiaohongshuService.GetCreatorLoginQrcode(c.Request.Context())
	if err != nil {
		respondError(c, http.StatusInternalServerError, "CREATOR_QRCODE_FAILED", "获取创作者登录二维码失败", err.Error())
		return
	}
	respondSuccess(c, result, "获取创作者登录二维码成功")
}

func registerCreatorLoginTools(server *mcp.Server, app *AppServer) {
	mcp.AddTool(server, &mcp.Tool{Name: "get_creator_login_qrcode", Description: "获取创作者中心独立登录二维码（Base64 PNG），等待扫码并保存两站会话；新取码会替换待扫码会话。"}, withPanicRecovery("get_creator_login_qrcode", func(ctx context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		result, err := app.xiaohongshuService.GetCreatorLoginQrcode(ctx)
		if err != nil {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil, nil
		}
		if result.IsLoggedIn {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "创作者中心已登录"}}}, nil, nil
		}
		return convertToMCPResult(&MCPToolResult{Content: []MCPContent{
			{Type: "text", Text: "请用小红书 App 扫码登录创作者中心，有效期 " + result.Timeout},
			{Type: "image", MimeType: "image/png", Data: strings.TrimPrefix(result.Img, "data:image/png;base64,")},
		}}), nil, nil
	}))
}
