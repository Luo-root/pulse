package kernel

import "testing"

// mustDerive ?????????????????????
func mustDerive(t *testing.T, c *Context) *Context {
	t.Helper()
	child, err := c.Derive()
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	return child
}
