package xiaohongshu

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSafeCreatorAgreement(t *testing.T) {
	for _, tc := range []struct {
		body, button string
		want         bool
	}{
		{"请阅读创作者服务协议", "同意", true},
		{"隐私政策更新", "我知道了", true},
		{"开通付费会员服务协议", "同意", false},
		{"同意接收营销信息", "同意", false},
		{"创作者服务协议", "同意并支付", false},
		{"删除笔记", "同意", false},
	} {
		require.Equal(t, tc.want, safeCreatorAgreement(tc.body, tc.button), tc.body)
	}
}
