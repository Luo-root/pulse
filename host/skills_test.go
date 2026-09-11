package host

import (
	"context"
	"errors"

	"github.com/Luo-root/pulse/skills"
)

// stubLoader 是 skills.Loader 的测试桩：一个固定 skill（frontend-design）。
type stubLoader struct{}

func (stubLoader) List(context.Context) ([]skills.Meta, error) {
	return []skills.Meta{{Name: "frontend-design", Description: "frontend design procedure"}}, nil
}

func (stubLoader) Load(_ context.Context, name string) (skills.Content, error) {
	if name != "frontend-design" {
		return skills.Content{}, errors.New("unknown skill")
	}
	return skills.Content{Name: name, Body: "# frontend design\nprocedure body", Directory: "C:/skills/frontend-design"}, nil
}

func (stubLoader) ListResources(context.Context, string, string, int) (skills.ResourcePage, error) {
	return skills.ResourcePage{}, nil
}

func (stubLoader) ReadFile(context.Context, string, string) ([]byte, error) {
	return nil, errors.New("not implemented in stub")
}

func newStubLoader() skills.Loader { return stubLoader{} }
