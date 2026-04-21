package chatmodel

import (
	"context"

	"github.com/Luo-root/pulse/components/schema"
)

type BaseModel interface {
	Generate(ctx context.Context, input []*schema.Message) (*schema.Message, error)
	Stream(ctx context.Context, input []*schema.Message) (*schema.StreamReader, error)
}
