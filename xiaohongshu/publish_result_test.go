package xiaohongshu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPublishSuccessURL(t *testing.T) {
	for _, path := range []string{"/new/note-manager", "/new/home/", "/publish/success", "/publish/publish-success?source=publish"} {
		require.True(t, isPublishSuccessURL("https://creator.xiaohongshu.com"+path), path)
	}
	for _, raw := range []string{"about:blank", "", "https://creator.xiaohongshu.com/publish/publish?success=true", "https://creator.xiaohongshu.com/login", "https://creator.xiaohongshu.com/verify", "https://creator.xiaohongshu.com/new/note-manager-failed", "https://example.com/new/note-manager", "https://creator.xiaohongshu.com.evil/new/home", "https://user@creator.xiaohongshu.com/new/home", "http://creator.xiaohongshu.com/new/home"} {
		require.False(t, isPublishSuccessURL(raw), raw)
	}
}

func TestPublishLengthCounter(t *testing.T) {
	for _, value := range []string{"18/20", "20/20", " 19 / 20 "} {
		require.NoError(t, makeMaxLengthError(value))
	}
	for _, value := range []string{"21/20", "-1/20", "21/0", "标题过长", "abc/20"} {
		require.Error(t, makeMaxLengthError(value))
	}
}

func TestPublishConfirmationPolicy(t *testing.T) {
	for _, text := range []string{"确认发布？\n取消\n发布", "确认发布笔记？\n取消\n发布", "是否发布这篇笔记？\n取消\n发布"} {
		_, ok := safePublishConfirmation(publishDialog{Text: text, Buttons: []string{"取消", "发布"}})
		require.True(t, ok, text)
	}
	for _, text := range []string{"实名认证", "请输入验证码", "同意新的服务协议", "营销授权", "支付会员费", "是否 AI 生成", "声明 AI 内容", "原创声明", "修改标题后发布", "此内容可能违规，确认承担责任", "未知的提示"} {
		t.Run(text, func(t *testing.T) {
			clicked := false
			state := publishObservation{URL: urlOfPublic, FormPresent: true, Dialogs: []publishDialog{{Text: text + "\n发布", Buttons: []string{"发布"}, confirm: func(context.Context, int) error { clicked = true; return nil }}}}
			last, err := monitorPublish(context.Background(), time.Second, func(context.Context) publishObservation { return state })
			require.ErrorContains(t, err, "需要账号本人处理")
			require.Contains(t, last.SeenPrompts, text+"\n发布")
			require.False(t, clicked)
		})
	}
	_, ok := safePublishConfirmation(publishDialog{Text: "确认发布\n发布", Buttons: []string{"发布"}, HasChoices: true})
	require.False(t, ok)
}

func TestPublishSafeConfirmationThenLanding(t *testing.T) {
	clicks := 0
	reads := 0
	last, err := monitorPublish(context.Background(), 2*time.Second, func(context.Context) publishObservation {
		reads++
		if reads > 2 {
			return publishObservation{URL: urlOfNoteManager}
		}
		return publishObservation{URL: urlOfPublic, FormPresent: true, Dialogs: []publishDialog{{Text: "确认发布笔记？\n取消\n发布", Buttons: []string{"取消", "发布"}, confirm: func(_ context.Context, index int) error { require.Equal(t, 1, index); clicks++; return nil }}}}
	})
	require.NoError(t, err)
	require.Equal(t, urlOfNoteManager, last.URL)
	require.Equal(t, 1, clicks, "弹窗未消失时不能反复提交")
	require.NotEmpty(t, last.SeenPrompts)
}

func TestPublishDoesNotAcceptToastOrUnrelatedRedirect(t *testing.T) {
	for _, state := range []publishObservation{
		{URL: urlOfPublic, Prompts: []string{"发布成功"}},
		{URL: urlOfNoteManager, FormPresent: true},
		{URL: urlOfNoteManager, Prompts: []string{"发布失败"}},
		{URL: "https://creator.xiaohongshu.com/login"},
		{URL: urlOfNoteManager, ReadErrors: []string{"DOM 读取失败"}},
	} {
		_, err := monitorPublish(context.Background(), time.Millisecond, func(context.Context) publishObservation { return state })
		require.Error(t, err)
	}
}

func TestPublishNoClickOnAmbiguousOrIncompleteObservation(t *testing.T) {
	clicked := false
	d := publishDialog{Text: "确认发布\n发布", Buttons: []string{"发布"}, confirm: func(context.Context, int) error { clicked = true; return nil }}
	for _, state := range []publishObservation{
		{URL: urlOfPublic, Dialogs: []publishDialog{d, d}},
		{URL: urlOfPublic, Dialogs: []publishDialog{d}, ReadErrors: []string{"DOM 读取失败"}},
	} {
		_, err := monitorPublish(context.Background(), time.Millisecond, func(context.Context) publishObservation { return state })
		require.Error(t, err)
		require.False(t, clicked)
	}
}

func TestPublishFailureKeepsTransientPrompt(t *testing.T) {
	reads := 0
	last, err := monitorPublish(context.Background(), 310*time.Millisecond, func(context.Context) publishObservation {
		reads++
		state := publishObservation{URL: urlOfPublic, FormPresent: true}
		if reads == 1 {
			state.Prompts = []string{"请检查标题"}
		}
		return state
	})
	require.Error(t, err)
	require.GreaterOrEqual(t, reads, 2)
	message := publishFailureMessage(err, last, "/tmp/diag/test")
	require.Contains(t, message, "请检查标题")
	require.Contains(t, message, "URL=https://creator.xiaohongshu.com/publish/publish")
	require.NotContains(t, message, "source=official")
}

func TestPublishDiagnosticFilesAndRedaction(t *testing.T) {
	root := filepath.Join(t.TempDir(), "diag")
	now := time.Now()
	data := publishDiagnostic{StartedAt: now, CapturedAt: now, Error: "校验失败 token=error-secret", publishObservation: publishObservation{
		URL:     "https://creator.xiaohongshu.com/publish/publish?xsec_token=url-secret&unknown=other-secret#fragment-secret",
		Title:   "Authorization: Bearer title-secret",
		Buttons: []publishButtonState{{Version: "widget", Text: "cookie: sid=button-secret", Attributes: map[string]string{"is-publish": "true", "submit-disabled": "false"}}},
		Prompts: []string{"标题校验未通过", `{"xsec_token":"prompt-secret"}`, "localStorage=storage-secret", "Bearer bearer-secret"},
		Dialogs: []publishDialog{{Text: "AI 内容需本人确认", Buttons: []string{"发布"}}},
	}}
	png := []byte{137, 80, 78, 71, 13, 10, 26, 10}
	dir, err := savePublishDiagnostic(root, data, func() ([]byte, error) { return png, nil })
	require.NoError(t, err)
	got, err := os.ReadFile(filepath.Join(dir, "page.png"))
	require.NoError(t, err)
	require.Equal(t, png, got)
	raw, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	require.NoError(t, err)
	require.Contains(t, string(raw), "标题校验未通过")
	require.Contains(t, string(raw), "AI 内容需本人确认")
	require.NotContains(t, string(raw), "-secret")
	require.Contains(t, string(raw), `"submit-disabled": "false"`)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0700), info.Mode().Perm())
	info, err = os.Stat(filepath.Join(dir, "metadata.json"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	second, err := savePublishDiagnostic(root, data, func() ([]byte, error) { return nil, errors.New("截图失败 token=screenshot-secret") })
	require.NoError(t, err)
	require.NotEqual(t, dir, second)
	raw, err = os.ReadFile(filepath.Join(second, "metadata.json"))
	require.NoError(t, err)
	require.Contains(t, string(raw), "screenshot_error")
	require.NotContains(t, string(raw), "-secret")
	_, err = os.Stat(filepath.Join(second, "page.png"))
	require.True(t, os.IsNotExist(err))
	_, err = savePublishDiagnostic(filepath.Join(dir, "metadata.json", "invalid"), data, func() ([]byte, error) { t.Fatal("不应截图"); return nil, nil })
	require.Error(t, err)
}

func TestPublishFailureErrorRedaction(t *testing.T) {
	cause := errors.New("发布失败\nAuthorization: Bearer cause-secret")
	state := publishObservation{URL: "https://creator.xiaohongshu.com/publish/publish?token=url-secret", Prompts: []string{"标题校验未通过", "cookie: sid=prompt-secret"}}
	wrapped := &publishDiagnosticError{cause: cause, message: publishFailureMessage(cause, state, "/diag/test")}
	require.ErrorIs(t, wrapped, cause)
	require.Contains(t, wrapped.Error(), "标题校验未通过")
	require.NotContains(t, wrapped.Error(), "-secret")
}
