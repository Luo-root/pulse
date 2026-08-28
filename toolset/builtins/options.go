package builtins

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// 默认上限（产品参数，不是行业标准）。
const (
	DefaultReadLimit     = 200
	DefaultMaxReadBytes  = 200 * 1024
	DefaultMaxLineRunes  = 2000
	DefaultGlobLimit     = 100
	DefaultGrepLimit     = 100
	DefaultLSLimit       = 500
	DefaultExecTimeout   = 60 * time.Second
	DefaultMaxExecBytes  = 30_000
	DefaultSourcePrefix  = "builtins"
)

// Options 控制 builtins 装配与路径/输出边界。
type Options struct {
	// Root 是默认工作区根（相对路径解析基准）。必填。
	Root string
	// WriteRoots 限制 edit/write 目标；空则仅允许 Root。
	WriteRoots []string
	// ForbidRead 拒绝 read/ls/glob/grep 进入的路径前缀（绝对路径）。
	ForbidRead []string
	// Enabled 非空时只注册列出的工具名；空=全部 P0。
	Enabled []string
	// SourcePrefix 写入 Registration.Source，默认 "builtins"。
	SourcePrefix string
	// ReadLimit / MaxReadBytes / MaxLineRunes 控制 read。
	ReadLimit    int
	MaxReadBytes int
	MaxLineRunes int
	// GlobLimit / GrepLimit / LSLimit 控制搜索与列表。
	GlobLimit int
	GrepLimit int
	LSLimit   int
	// ExecTimeout / MaxExecBytes 控制 exec。
	ExecTimeout  time.Duration
	MaxExecBytes int
}

func (o Options) withDefaults() (Options, error) {
	if o.Root == "" {
		return Options{}, fmt.Errorf("builtins: Root is required")
	}
	abs, err := filepath.Abs(o.Root)
	if err != nil {
		return Options{}, fmt.Errorf("builtins: resolve Root: %w", err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return Options{}, fmt.Errorf("builtins: Root: %w", err)
	}
	if !st.IsDir() {
		return Options{}, fmt.Errorf("builtins: Root is not a directory: %s", abs)
	}
	o.Root = abs

	if len(o.WriteRoots) == 0 {
		o.WriteRoots = []string{abs}
	} else {
		wr := make([]string, 0, len(o.WriteRoots))
		for _, r := range o.WriteRoots {
			a, err := filepath.Abs(r)
			if err != nil {
				return Options{}, fmt.Errorf("builtins: WriteRoots: %w", err)
			}
			wr = append(wr, a)
		}
		o.WriteRoots = wr
	}

	fr := make([]string, 0, len(o.ForbidRead))
	for _, r := range o.ForbidRead {
		a, err := filepath.Abs(r)
		if err != nil {
			return Options{}, fmt.Errorf("builtins: ForbidRead: %w", err)
		}
		fr = append(fr, a)
	}
	o.ForbidRead = fr

	if o.SourcePrefix == "" {
		o.SourcePrefix = DefaultSourcePrefix
	}
	if o.ReadLimit <= 0 {
		o.ReadLimit = DefaultReadLimit
	}
	if o.MaxReadBytes <= 0 {
		o.MaxReadBytes = DefaultMaxReadBytes
	}
	if o.MaxLineRunes <= 0 {
		o.MaxLineRunes = DefaultMaxLineRunes
	}
	if o.GlobLimit <= 0 {
		o.GlobLimit = DefaultGlobLimit
	}
	if o.GrepLimit <= 0 {
		o.GrepLimit = DefaultGrepLimit
	}
	if o.LSLimit <= 0 {
		o.LSLimit = DefaultLSLimit
	}
	if o.ExecTimeout <= 0 {
		o.ExecTimeout = DefaultExecTimeout
	}
	if o.MaxExecBytes <= 0 {
		o.MaxExecBytes = DefaultMaxExecBytes
	}
	return o, nil
}
