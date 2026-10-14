package pulse_test

import (
	"context"
	"fmt"
	"strings"

	"github.com/Luo-root/pulse"
)

func ExampleSub() {
	// 子图只通过声明端口接线；downloaded 和 wordCount 留在子图内部。
	childInput := pulse.NewKey[string]("document")
	downloaded := pulse.NewKey[string]("downloaded")
	wordCount := pulse.NewKey[int]("word-count")
	childOutput := pulse.NewKey[string]("summary")
	child, err := pulse.New("document-pipeline", "下载、提取、摘要")
	if err != nil {
		panic(err)
	}
	if err := child.DeclareInput(pulse.NewPort(childInput)); err != nil {
		panic(err)
	}
	if err := child.DeclareOutput(pulse.NewPort(childOutput)); err != nil {
		panic(err)
	}
	calls := [3]int{}
	for _, node := range []*pulse.Node{
		pulse.NewNode("download", "用输入文本模拟下载", pulse.Requires(childInput), pulse.Provides(downloaded), func(rc *pulse.RunCtx) error {
			calls[0]++
			text, err := pulse.Get(rc, childInput)
			if err != nil {
				return err
			}
			return pulse.Set(rc, downloaded, text)
		}),
		pulse.NewNode("extract", "提取单词数", pulse.Requires(downloaded), pulse.Provides(wordCount), func(rc *pulse.RunCtx) error {
			calls[1]++
			text, err := pulse.Get(rc, downloaded)
			if err != nil {
				return err
			}
			return pulse.Set(rc, wordCount, len(strings.Fields(text)))
		}),
		pulse.NewNode("summarize", "生成摘要", pulse.Requires(wordCount), pulse.Provides(childOutput), func(rc *pulse.RunCtx) error {
			calls[2]++
			count, err := pulse.Get(rc, wordCount)
			if err != nil {
				return err
			}
			return pulse.Set(rc, childOutput, fmt.Sprintf("%d words", count))
		}),
	} {
		if err := child.Add(node); err != nil {
			panic(err)
		}
	}

	input := pulse.NewKey[string]("source")
	output := pulse.NewKey[string]("result")
	g, err := pulse.New("documents", "把整张子图作为一步")
	if err != nil {
		panic(err)
	}
	if err := g.DeclareInput(pulse.NewPort(input)); err != nil {
		panic(err)
	}
	if err := g.DeclareOutput(pulse.NewPort(output)); err != nil {
		panic(err)
	}
	if err := pulse.Sub(g, "process", "处理一份文档", []pulse.SubBind{
		pulse.In(input, childInput),
		pulse.Out(childOutput, output),
	}, child); err != nil {
		panic(err)
	}

	// 同一份父/子图定义跑两次：每次创建新的 Execution，业务节点也各执行一次。
	for _, text := range []string{"data driven workflow", "second run"} {
		var in pulse.Inputs
		if err := pulse.Seed(&in, input, text); err != nil {
			panic(err)
		}
		e, err := g.NewExecution(context.Background(), in)
		if err != nil {
			panic(err)
		}
		if err := e.Start(); err != nil {
			panic(err)
		}
		if err := e.Wait(); err != nil {
			panic(err)
		}
		value, state, err := pulse.ReadOutput(e, output)
		if err != nil {
			panic(err)
		}
		fmt.Println("result:", value, state)
	}
	fmt.Println("download/extract/summarize calls:", calls)
	// Output:
	// result: 3 words ready
	// result: 2 words ready
	// download/extract/summarize calls: [2 2 2]
}
