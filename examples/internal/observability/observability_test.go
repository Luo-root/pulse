package observability

import (
	"testing"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
)

func TestPluginProvidesAndUnloads(t *testing.T) {
	sink := &MemorySink{}
	host := kernel.New()
	fiber, err := kernel.Use(host, Plugin("trace-1", sink))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := kernel.Get(host, ServiceKey); !ok {
		t.Fatal("reporter not provided")
	}
	fiber.Close()
	if _, ok := kernel.Get(host, ServiceKey); ok {
		t.Fatal("reporter should be gone after plugin close")
	}
	host.Dispose()
}

func TestInputSummaryCountsModalities(t *testing.T) {
	msg := llm.User(
		llm.Text("hi"),
		llm.ImageURL("https://example.com/a.png", "image/png"),
		llm.Media("application/pdf", []byte("%PDF")),
	)
	sum := InputSummary([]*llm.Message{msg})
	if sum["text_parts"] != 1 || sum["image_parts"] != 1 || sum["custom_parts"] != 1 {
		t.Fatalf("summary = %#v", sum)
	}
}
