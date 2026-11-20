package xiaohongshu

import (
	"context"
	"fmt"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPublishRetryOnceAfterVerification(t *testing.T) {
	for _, prompt := range []string{"", "验证成功", "验证成功, 正在登录..."} {
		t.Run(prompt, func(t *testing.T) {
			t.Chdir(t.TempDir())
			synctest.Test(t, func(t *testing.T) {
				reads, clicks := 0, 0
				var clickedAt time.Time
				_, err := monitorPublish(context.Background(), time.Second, func(context.Context) publishObservation {
					reads++
					state := publishObservation{URL: urlOfPublic, FormPresent: true}
					if prompt == "" && reads == 1 {
						state.Scan = &publishScan{Text: "扫码验证", capture: func(context.Context) ([]byte, error) { return []byte("qr"), nil }}
						return state
					}
					if prompt != "" {
						state.Prompts = []string{prompt}
					}
					state.retry = func(context.Context) error { clicks++; clickedAt = time.Now(); return nil }
					return state
				})
				require.ErrorContains(t, err, "发布未确认成功")
				require.Equal(t, 1, clicks)
				require.GreaterOrEqual(t, time.Since(clickedAt), 2*time.Minute)
				require.Less(t, time.Since(clickedAt), 2*time.Minute+time.Second)
			})
		})
	}
}

func TestPublishRetrySecondScanGetsFullBudgetAndUpdatesQR(t *testing.T) {
	t.Chdir(t.TempDir())
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var clickedAt, secondDone time.Time
		clicks := 0
		_, err := monitorPublish(context.Background(), publishConfirmTimeout, func(context.Context) publishObservation {
			state := publishObservation{URL: urlOfPublic, FormPresent: true}
			elapsed := time.Since(start)
			round := 1
			scanning := elapsed < 290*time.Second
			if !clickedAt.IsZero() {
				round = 2
				scanning = time.Since(clickedAt) < 290*time.Second
			}
			if scanning {
				// 每轮都经历多次二维码到期；刷新只影响二维码，不点击发布或取消。
				state.Scan = &publishScan{Text: "扫码验证 二维码1分钟失效", capture: func(context.Context) ([]byte, error) {
					return []byte(fmt.Sprintf("qr-%d-%d", round, int(elapsed.Seconds())/50)), nil
				}, refresh: func(context.Context) error { return nil }}
				return state
			}
			if round == 2 && secondDone.IsZero() {
				secondDone = time.Now()
			}
			state.retry = func(context.Context) error { clicks++; clickedAt = time.Now(); return nil }
			return state
		})
		require.ErrorContains(t, err, "发布未确认成功")
		require.Equal(t, 1, clicks)
		require.GreaterOrEqual(t, time.Since(secondDone), 2*time.Minute)
		require.Greater(t, time.Since(start), 11*time.Minute)
		png, err := os.ReadFile("verify_qr.png")
		require.NoError(t, err)
		require.Contains(t, string(png), "qr-2-")
		require.FileExists(t, "verify_qr.json")
	})
}

func TestPublishRetryWaitsForLandingAndStableDraft(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var clickedAt time.Time
		last, err := monitorPublish(context.Background(), time.Second, func(context.Context) publishObservation {
			state := publishObservation{URL: urlOfPublic, FormPresent: true, Prompts: []string{"验证成功"}}
			if !clickedAt.IsZero() && time.Since(clickedAt) > 90*time.Second {
				return publishObservation{URL: urlOfNoteManager, NoteID: "0123456789abcdef01234567"}
			}
			// 页面未就绪的前三秒没有补点能力。
			if time.Since(start) > 3*time.Second {
				state.retry = func(context.Context) error { require.True(t, clickedAt.IsZero()); clickedAt = time.Now(); return nil }
			}
			return state
		})
		require.NoError(t, err)
		require.NotEmpty(t, last.NoteID)
		require.GreaterOrEqual(t, clickedAt.Sub(start), 5*time.Second)
		require.Greater(t, time.Since(clickedAt), 90*time.Second)
	})
}

func TestPublishVerifiedScanDoesNotRefreshMissingQR(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := publishScanWait{}
		scan := &publishScan{Text: "扫码验证\n验证成功, 正在登录...", refresh: func(context.Context) error { t.Fatal("验证成功不能刷新"); return nil }}
		require.NoError(t, w.update(context.Background(), scan))
		time.Sleep(6 * time.Second)
		require.NoError(t, w.update(context.Background(), scan))
	})
}

func TestPublishNoteIDAndTimestamp(t *testing.T) {
	require.Equal(t, "0123456789abcdef01234567", publishURLNoteID(urlOfNoteManager+"?note_id=0123456789abcdef01234567&token=secret"))
	require.Empty(t, publishURLNoteID(urlOfPublic+"&note_id=0123456789abcdef01234567"))
	require.Empty(t, publishURLNoteID(urlOfNoteManager+"?note_id=token-secret"))
	for _, text := range []string{"2026-11-20T19:12:46+08:00", "发布于 2026-11-20 19:12:46", "2026/11/20 19:12:46"} {
		parsed, err := parseManagedPublishTime(text)
		require.NoError(t, err)
		require.Equal(t, "2026-11-20T11:12:46Z", parsed.UTC().Format(time.RFC3339))
	}
	_, err := parseManagedPublishTime("刚刚")
	require.Error(t, err)
	require.GreaterOrEqual(t, PublishTimeout, 18*time.Minute+3*time.Minute+2*publishScanTimeout+3*publishConfirmTimeout+time.Minute)
}

func TestPublishRetryAmbiguousClickStillWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var clickedAt time.Time
		clicks := 0
		last, err := monitorPublish(context.Background(), time.Second, func(context.Context) publishObservation {
			return publishObservation{URL: urlOfPublic, FormPresent: true, Prompts: []string{"验证成功"}, retry: func(context.Context) error {
				clicks++
				clickedAt = time.Now()
				return fmt.Errorf("鼠标提交后连接中断")
			}}
		})
		require.Error(t, err)
		require.Equal(t, 1, clicks)
		require.GreaterOrEqual(t, time.Since(clickedAt), 2*time.Minute)
		require.Contains(t, last.SeenPrompts, "发布补点反馈未确认: 鼠标提交后连接中断")
	})
}
