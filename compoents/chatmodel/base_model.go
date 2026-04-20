package chatmodel

import (
	"context"
	"pulse/compoents/schema"
)

type BaseModel interface {
	Generate(ctx context.Context, input []*schema.Message) (*schema.Message, error)
	Stream(ctx context.Context, input []*schema.Message) (*schema.StreamReader, error)
}
