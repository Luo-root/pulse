package main

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/compose"
)

// buildEinoRecovery 供宿主独立恢复命令调用，不注册为 plain/observe 计时模式。
//
// 限于 chain；CheckpointAt 是已完成前缀长度，取值 [1, N-1]。
// pause=true：RunID 必须尚无 checkpoint，在下一个节点之前主动 interrupt，run 返回
// 可由 einoInterrupted 识别的错误；只有 Store.Set 确认成功后才有可恢复 checkpoint。
// pause=false：RunID 必须已有 checkpoint，重建同图并从该 checkpoint 恢复。
// 两进程须保持图、rounds、版本和 RunID 一致；宿主负责持久 Store、关闭与身份校验。
//
// Store 原样保存 Eino 默认 serializer 产生的 []byte，无须解析或重编 checkpoint JSON。
// Get(context.Context,string)([]byte,bool,error) / Set(context.Context,string,[]byte)error。
// 普通成功运行不会逐节点写 checkpoint；这只测主动中断后的恢复，不代表任意 crash 恢复。
func buildEinoRecovery(ctx context.Context, spec *graphSpec, cfg Config, p *probe, store compose.CheckPointStore, pause bool) (*einoRunner, error) {
	if store == nil || cfg.RunID == "" {
		return nil, fmt.Errorf("eino recovery: store and run ID are required")
	}
	if spec == nil || spec.shape != "chain" || cfg.CheckpointAt < 1 || cfg.CheckpointAt >= len(spec.names) {
		return nil, fmt.Errorf("eino recovery: chain and checkpoint prefix in [1,N-1] required")
	}
	_, exists, err := store.Get(ctx, cfg.RunID)
	if err != nil {
		return nil, fmt.Errorf("eino recovery: check checkpoint: %w", err)
	}
	if pause && exists {
		return nil, fmt.Errorf("eino recovery: checkpoint %q already exists", cfg.RunID)
	}
	if !pause && !exists {
		return nil, fmt.Errorf("eino recovery: checkpoint %q does not exist", cfg.RunID)
	}
	options := []compose.GraphCompileOption{compose.WithCheckPointStore(store)}
	if pause {
		options = append(options, compose.WithInterruptBeforeNodes([]string{spec.names[cfg.CheckpointAt]}))
	}
	r, err := buildEinoGraph(ctx, spec, cfg, p, options...)
	if err != nil {
		return nil, err
	}
	r.options = append(r.options, compose.WithCheckPointID(cfg.RunID))
	return r, nil
}

func einoInterrupted(err error) bool {
	_, ok := compose.ExtractInterruptInfo(err)
	return ok
}
