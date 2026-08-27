package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Luo-root/pulse/examples/internal/demoapp"
	"github.com/Luo-root/pulse/llm"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "01-chat: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	flags := demoapp.LoadFlagsFromEnv()
	host, err := demoapp.Open(flags)
	if err != nil {
		return err
	}
	defer host.Close()
	fmt.Printf("01-chat provider=%s model=%s scripted=%v\n", flags.Provider, flags.Model, flags.Scripted)
	return demoapp.Loop(os.Stdin, os.Stdout, func(msg *llm.Message) ([]*llm.Message, error) {
		started := time.Now()
		resp, err := host.Model.Generate(context.Background(), llm.NewRequest(msg))
		if err != nil {
			return nil, err
		}
		fmt.Println(resp.Message.Text())
		fmt.Fprintf(os.Stderr, "chat turn duration_ms=%d finish=%s\n", time.Since(started).Milliseconds(), resp.FinishReason)
		return []*llm.Message{resp.Message}, nil
	}, func() int { return 0 })
}
