package eval

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
)

// defaultSeedBase 是 property test 的默认种子基值。固定值保证 CI 与本地
// 每次运行产生相同序列——property test 的价值在「确定性探索一条随机
// 路径 + 失败可回放」，不在每次换花样。
const defaultSeedBase = 20260902

// seedFor 返回给定测试名的确定性种子。EVAL_SEED 环境变量（十进制整数）
// 优先——手动探索新失败路径时设置它，失败信息会带出该值。
func seedFor(testName string) int64 {
	if v := os.Getenv("EVAL_SEED"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	h := uint64(defaultSeedBase)
	for i := 0; i < len(testName); i++ {
		h = h*131 + uint64(testName[i])
	}
	return int64(h)
}

// rng 是 property test 的可复现随机源。
type rng struct {
	*rand.Rand
	seed int64
}

func newRng(seed int64) *rng {
	return &rng{Rand: rand.New(rand.NewPCG(uint64(seed), uint64(seed>>1))), seed: seed}
}

// failf 生成带 seed 的错误信息——所有 property 断言失败都必须经由它，
// 保证「失败信息即可回放凭据」。
func (r *rng) failf(format string, args ...any) string {
	return fmt.Sprintf("seed=%d: %s", r.seed, fmt.Sprintf(format, args...))
}

// pick 均匀选择一个元素。
func pick[T any](r *rng, items []T) T {
	return items[r.IntN(len(items))]
}

// text 生成一段可读伪文本（字母 + 空格），长度 [1, maxRunes]。
func (r *rng) text(maxRunes int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz"
	n := 1 + r.IntN(maxRunes)
	b := make([]byte, n)
	for i := range b {
		if i > 0 && i%8 == 0 && r.IntN(4) == 0 {
			b[i] = ' '
			continue
		}
		b[i] = alphabet[r.IntN(len(alphabet))]
	}
	return string(b)
}

// randStr 生成不含空白与换行的短标识符（工具名 / ID / namespace 片段）。
func (r *rng) randStr(maxLen int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	n := 1 + r.IntN(maxLen)
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.IntN(len(alphabet))]
	}
	return string(b)
}
