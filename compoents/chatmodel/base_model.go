package chatmodel

import (
	"Pulse/compoents/schema"
	"context"
)

type BaseModel interface {
	Generate(ctx context.Context, input []*schema.Message) (*schema.Message, error)
	Stream(ctx context.Context, input []*schema.Message) (*schema.StreamReader, error)
}
