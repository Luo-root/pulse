package flow

import (
	"context"
	"testing"
)

func TestRegistryRegisterAndLookup(t *testing.T) {
	r := NewRegistry()
	run := func(*RunCtx) error { return nil }
	if err := r.Register("a", run); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("a", run); err == nil {
		t.Fatal("duplicate register should fail")
	}
	got, ok := r.Lookup("a")
	if !ok || got == nil {
		t.Fatal("lookup miss")
	}
	r2 := NewRegistry()
	if _, ok := r2.Lookup("a"); ok {
		t.Fatal("registries must be isolated")
	}
}

func TestRegisterKeyTypeTag(t *testing.T) {
	r := NewRegistry()
	k := NewKey[string]("demo.q")
	if err := RegisterKey(r, k); err != nil {
		t.Fatal(err)
	}
	tag, ok := r.TypeTagOf("demo.q")
	if !ok || tag != "string" {
		t.Fatalf("tag=%q ok=%v", tag, ok)
	}
	ref, err := r.ResolveKey("demo.q", "string")
	if err != nil || ref.name != "demo.q" {
		t.Fatalf("resolve: %v %#v", err, ref)
	}
	if _, err := r.ResolveKey("demo.q", "int"); err == nil {
		t.Fatal("type mismatch should fail")
	}
	if err := RegisterKey(r, NewKey[int]("demo.q")); err == nil {
		t.Fatal("same name different T should fail")
	}
}

func TestSeedByNameAssignable(t *testing.T) {
	r := NewRegistry()
	MustRegisterKey(r, NewKey[string]("demo.s"))
	g := New(context.Background())
	if err := SeedByName(g, r, "demo.s", "string", "hi"); err != nil {
		t.Fatal(err)
	}
	if err := SeedByName(g, r, "demo.s", "string", 1); err == nil {
		t.Fatal("int not assignable to string")
	}
}
