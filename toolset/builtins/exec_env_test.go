package builtins

import (
	"os"
	"strings"
	"testing"
)

func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

// TestChildEnvAllowlist：白名单默认语义——平台必需键继承，宿主 secret 不
// 泄漏；ExecEnv 按键名追加（值仍取自宿主环境）；非法条目（空串/含 =）忽略。
func TestChildEnvAllowlist(t *testing.T) {
	t.Setenv("PULSE_SECRET_TEST", "leak-me")
	t.Setenv("HOME", "/home/tester")

	// 默认白名单：PATH/HOME 继承，宿主 secret 不见。
	env := childEnv(Options{})
	kv := envMap(env)
	if _, ok := kv["PATH"]; !ok {
		t.Fatal("PATH must be inherited by default")
	}
	if kv["HOME"] != "/home/tester" {
		t.Fatalf("HOME = %q, want inherited value", kv["HOME"])
	}
	if _, ok := kv["PULSE_SECRET_TEST"]; ok {
		t.Fatal("host secret must not leak into child env by default")
	}

	// ExecEnv 追加：键名进白名单，值来自宿主环境；空串与含 = 的条目被忽略。
	env = childEnv(Options{ExecEnv: []string{"PULSE_SECRET_TEST", "", "BAD=val"}})
	kv = envMap(env)
	if kv["PULSE_SECRET_TEST"] != "leak-me" {
		t.Fatalf("ExecEnv allowlisted key: got %q", kv["PULSE_SECRET_TEST"])
	}
	if _, ok := kv["BAD"]; ok {
		t.Fatal("ExecEnv entries containing '=' must be ignored (key-name semantics)")
	}
}

// TestChildEnvNeverFallsBackToInherit：白名单模式下即使宿主环境一个白名单
// 键都没有，也必须返回空环境（非 nil）——os/exec 的 nil Env 语义是继承
// 全量，静默退回等于白名单失效；InheritAll 档才显式返回 nil。
func TestChildEnvNeverFallsBackToInherit(t *testing.T) {
	orig := environ
	t.Cleanup(func() { environ = orig })
	environ = func() []string { return []string{"PULSE_SECRET=1"} }

	env := childEnv(Options{})
	if env == nil {
		t.Fatal("allowlist mode must never return nil (nil means inherit-all in os/exec)")
	}
	if len(env) != 0 {
		t.Fatalf("want empty env, got %v", env)
	}
	if childEnv(Options{ExecEnvInheritAll: true}) != nil {
		t.Fatal("ExecEnvInheritAll must return nil (os/exec inherit-all semantics)")
	}
}

// TestChildEnvInheritAll：InheritAll 恒返回 nil——无论 environ 里有什么。
func TestChildEnvInheritAll(t *testing.T) {
	t.Setenv("PULSE_SECRET_TEST", "leak-me")
	env := childEnv(Options{ExecEnvInheritAll: true})
	if env != nil {
		t.Fatalf("InheritAll must return nil for os/exec inherit-all, got %v", env)
	}
	// 兜底确认 secret 确实在宿主环境里（t.Setenv 生效，上一行 nil 语义才有意义）。
	if _, ok := envMap(os.Environ())["PULSE_SECRET_TEST"]; !ok {
		t.Fatal("test setup broken: PULSE_SECRET_TEST not in host env")
	}
}
