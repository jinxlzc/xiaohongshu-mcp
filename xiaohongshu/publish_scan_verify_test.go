package xiaohongshu

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPublishScanLifetime(t *testing.T) {
	for _, tc := range []struct {
		text string
		want time.Duration
	}{
		{"二维码1分钟失效", time.Minute}, {"二维码 25 秒后失效", 25 * time.Second},
		{"剩余 00:42", 42 * time.Second}, {"二维码已过期，点击刷新", 0}, {"扫码验证", time.Minute},
	} {
		require.Equal(t, tc.want, publishScanLifetime(tc.text))
	}
}

func TestPublishScanFilesRefreshAndLanding(t *testing.T) {
	t.Chdir(t.TempDir())
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		refreshed := false
		writes := 0
		scan := &publishScan{Text: "扫码验证 二维码1分钟失效"}
		scan.capture = func(context.Context) ([]byte, error) {
			if refreshed {
				return []byte("new-qr"), nil
			}
			return []byte("old-qr"), nil
		}
		scan.refresh = func(context.Context) error { refreshed = true; return nil }
		last, err := monitorPublish(context.Background(), 15*time.Second, func(context.Context) publishObservation {
			if raw, e := os.ReadFile("verify_qr.json"); e == nil {
				var meta publishQRMetadata
				require.NoError(t, json.Unmarshal(raw, &meta))
				require.NotContains(t, string(raw), "token")
				require.Equal(t, time.Minute, meta.ExpiresAt.Sub(meta.WrittenAt))
				if meta.WrittenAt.After(start) {
					writes++
					if refreshed {
						return publishObservation{URL: urlOfNoteManager}
					}
				}
			}
			return publishObservation{URL: urlOfPublic, FormPresent: true, Scan: scan}
		})
		require.NoError(t, err)
		require.Equal(t, urlOfNoteManager, last.URL)
		require.True(t, refreshed)
		require.Positive(t, writes)
		png, err := os.ReadFile("verify_qr.png")
		require.NoError(t, err)
		require.Equal(t, "new-qr", string(png))
		for _, name := range []string{"verify_qr.png", "verify_qr.json"} {
			info, e := os.Stat(name)
			require.NoError(t, e)
			require.Equal(t, os.FileMode(0600), info.Mode().Perm())
		}
	})
}

func TestPublishScanTimeoutAndRefreshFailure(t *testing.T) {
	for _, mode := range []string{"timeout", "no-refresh", "unchanged", "refresh-error", "parent-canceled"} {
		t.Run(mode, func(t *testing.T) {
			t.Chdir(t.TempDir())
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				version := byte(0)
				scan := &publishScan{Text: "扫码验证 二维码1分钟失效", capture: func(context.Context) ([]byte, error) { return []byte{version}, nil }}
				if mode != "no-refresh" {
					scan.refresh = func(context.Context) error {
						if mode == "refresh-error" {
							return errors.New("刷新不可用")
						}
						if mode != "unchanged" {
							version++
						}
						return nil
					}
				}
				ctx := context.Background()
				if mode == "parent-canceled" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
					defer cancel()
				}
				_, err := monitorPublish(ctx, 15*time.Second, func(context.Context) publishObservation { return publishObservation{URL: urlOfPublic, Scan: scan} })
				require.ErrorContains(t, err, "需要账号本人扫码验证")
				if mode == "timeout" {
					require.InDelta(t, 300, time.Since(start).Seconds(), 1)
				}
			})
		})
	}
}

func TestPublishScanDisappearanceStillRequiresLanding(t *testing.T) {
	for _, landing := range []publishObservation{{URL: urlOfPublic, Prompts: []string{"发布成功"}}, {URL: urlOfNoteManager, FormPresent: true}, {URL: urlOfNoteManager}} {
		t.Run(landing.URL, func(t *testing.T) {
			t.Chdir(t.TempDir())
			synctest.Test(t, func(t *testing.T) {
				reads := 0
				_, err := monitorPublish(context.Background(), time.Second, func(context.Context) publishObservation {
					reads++
					if reads == 1 {
						return publishObservation{URL: urlOfPublic, Scan: &publishScan{Text: "扫码验证", capture: func(context.Context) ([]byte, error) { return []byte("qr"), nil }}}
					}
					return landing
				})
				if landing.URL == urlOfNoteManager && !landing.FormPresent {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			})
		})
	}
}

func TestPublishScanReadFailureKeepsOwnerError(t *testing.T) {
	t.Chdir(t.TempDir())
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		reads := 0
		_, err := monitorPublish(ctx, 15*time.Second, func(context.Context) publishObservation {
			reads++
			if reads == 1 {
				return publishObservation{URL: urlOfPublic, Scan: &publishScan{Text: "扫码验证", capture: func(context.Context) ([]byte, error) { return []byte("qr"), nil }}}
			}
			return publishObservation{URL: urlOfPublic, ReadErrors: []string{"DOM 暂不可用"}}
		})
		require.ErrorContains(t, err, "需要账号本人扫码验证")
	})
}

func TestPublishScanVisibleExpiryOverridesEstimatedExpiry(t *testing.T) {
	t.Chdir(t.TempDir())
	synctest.Test(t, func(t *testing.T) {
		refreshes := 0
		scan := &publishScan{Text: "扫码验证 二维码1分钟失效", RefreshAvailable: true, capture: func(context.Context) ([]byte, error) { return []byte("qr"), nil }, refresh: func(context.Context) error { refreshes++; return nil }}
		wait := publishScanWait{refreshVisible: true}
		require.NoError(t, wait.update(context.Background(), scan))
		require.Zero(t, refreshes)
		scan.Text = "扫码验证 二维码已过期"
		require.NoError(t, wait.update(context.Background(), scan))
		require.Equal(t, 1, refreshes)
	})
}

func TestPublishScanRefreshAllowsTemporaryMissingQR(t *testing.T) {
	t.Chdir(t.TempDir())
	synctest.Test(t, func(t *testing.T) {
		scan := &publishScan{Text: "二维码已过期", capture: func(context.Context) ([]byte, error) { return []byte("old"), nil }, refresh: func(context.Context) error { return nil }}
		wait := publishScanWait{}
		require.NoError(t, wait.update(context.Background(), scan))
		scan.capture = nil
		time.Sleep(time.Second)
		require.NoError(t, wait.update(context.Background(), scan))
		scan.Text = "二维码1分钟失效"
		scan.capture = func(context.Context) ([]byte, error) { return []byte("new"), nil }
		require.NoError(t, wait.update(context.Background(), scan))
		data, err := os.ReadFile("verify_qr.png")
		require.NoError(t, err)
		require.Equal(t, "new", string(data))
	})
}

func TestPublishScanRefreshWaitsForValidNewQR(t *testing.T) {
	t.Chdir(t.TempDir())
	synctest.Test(t, func(t *testing.T) {
		data := []byte("old")
		scan := &publishScan{Text: "二维码已过期", capture: func(context.Context) ([]byte, error) { return data, nil }, refresh: func(context.Context) error { return nil }}
		wait := publishScanWait{}
		require.NoError(t, wait.update(context.Background(), scan))
		data = []byte("new")
		require.NoError(t, wait.update(context.Background(), scan))
		scan.Text = "二维码1分钟失效"
		require.NoError(t, wait.update(context.Background(), scan))
		time.Sleep(6 * time.Second)
		require.NoError(t, wait.update(context.Background(), scan))
	})
}
