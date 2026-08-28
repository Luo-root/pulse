package main

import (
	"fmt"
	"os"

	"github.com/Luo-root/pulse/examples/internal/demoapp"
	"github.com/Luo-root/pulse/llm"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "04-flow-dag: %v\n", err)
		os.Exit(1)
	}
}

func defaultRetrievers() (local, web Retriever) {
	local = memoryRetriever{
		source: "local",
		docs: []Document{
			{Title: "kernel", Content: "卸载即还原，依赖响应式装载。"},
			{Title: "flow", Content: "一次运行一个世界，Requires 是 AND，Skip 是到达。"},
		},
	}
	web = memoryRetriever{
		source: "web",
		docs: []Document{
			{Title: "observer", Content: "E1 Waiting/Running/Finished；桥两条 Record。"},
			{Title: "yaml", Content: "E2 拓扑归属 A：YAML 拥有边，Factory 只给 Run。"},
		},
	}
	return local, web
}

func run() error {
	flags := demoapp.LoadFlagsFromEnv()
	host, err := demoapp.Open(flags)
	if err != nil {
		return err
	}
	defer host.Close()

	local, web := defaultRetrievers()
	fmt.Printf("04-flow-dag host=%s\n", host.HostID())
	fmt.Println("fact 示例：问 kernel / flow / observer；闲聊示例：你好 / 晚饭吃什么")
	return demoapp.Loop(os.Stdin, os.Stdout, func(msg *llm.Message) ([]*llm.Message, error) {
		deps := dagDeps{
			local: local,
			web:   web,
			sink:  host.Sink,
			peak:  &demoapp.FlowPeak{},
			host:  host.HostID(),
			trace: host.NewTraceID(),
		}
		res, dur, err := runDAG(msg.Text(), deps)
		if err != nil {
			return nil, err
		}
		fmt.Print(res.Final)
		fmt.Fprintf(os.Stderr, "summary intent=%s duration_ms=%d alive_nodes_peak=%d\n",
			res.Intent, dur.Milliseconds(), res.Peak)
		return nil, nil
	}, func() int { return 0 })
}
