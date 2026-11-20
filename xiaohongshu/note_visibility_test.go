package xiaohongshu

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateNoteVisibility(t *testing.T) {
	for _, v := range []string{"公开可见", "仅自己可见", "仅互关好友可见"} {
		require.NoError(t, ValidateNoteVisibility("0123456789abcdef01234567", v))
	}
	for _, v := range []string{"", "公开", "private"} {
		require.Error(t, ValidateNoteVisibility("0123456789abcdef01234567", v))
	}
	for _, id := range []string{"", "../x", "abc\"", "123"} {
		require.Error(t, ValidateNoteVisibility(id, "公开可见"))
	}
}

func TestChangeNoteVisibility(t *testing.T) {
	for _, tc := range []struct {
		name, before, after string
		writeErr            error
		wantErr, skip       bool
	}{
		{name: "幂等", before: "仅自己可见", after: "仅自己可见", skip: true},
		{name: "修改后回读", before: "公开可见", after: "仅自己可见"},
		{name: "未生效", before: "公开可见", after: "公开可见", wantErr: true},
		{name: "未知原状态", before: "", wantErr: true},
		{name: "点击失败", before: "公开可见", writeErr: errors.New("click"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads, writes := 0, 0
			result, err := changeNoteVisibility(context.Background(), "0123456789abcdef01234567", "仅自己可见", func() (string, error) {
				reads++
				if reads == 1 {
					return tc.before, nil
				}
				return tc.after, nil
			}, func() error { writes++; return tc.writeErr })
			if tc.wantErr {
				require.Error(t, err)
				require.Nil(t, result)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.before, result.Before)
				require.Equal(t, tc.after, result.After)
				if tc.skip {
					require.Equal(t, "skipped", result.Status)
					require.Zero(t, writes)
				} else {
					require.Equal(t, "updated", result.Status)
					require.Equal(t, 1, writes)
					require.GreaterOrEqual(t, reads, 2)
				}
			}
		})
	}
}

func TestVisibilityReadFailureAndCancellation(t *testing.T) {
	calls := 0
	write := func() error { calls++; return nil }
	_, err := changeNoteVisibility(context.Background(), "0123456789abcdef01234567", "公开可见", func() (string, error) { return "", errors.New("read failure") }, write)
	require.ErrorContains(t, err, "read failure")
	require.Zero(t, calls)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = changeNoteVisibility(ctx, "0123456789abcdef01234567", "公开可见", func() (string, error) { t.Fatal("取消后不应读取页面"); return "", nil }, write)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, calls)
}
