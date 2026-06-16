package tools

import (
	"context"
	"fmt"
	"testing"

	"github.com/Luo-root/pulse/components/schema"
)

// TestUserConfigV2 测试 GORM 版用户配置管理工具
func TestUserConfigV2(t *testing.T) {
	// ====================== 0. 准备：用临时内存数据库测试（可选，避免污染正式文件） ======================
	// 如果你想测试正式的 ./chat.db，可以跳过这一步，直接用原代码的 getConfigDB
	// 这里演示用内存数据库，测试结束自动清理
	testDB, err := getConfigDB()
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}
	// 自动迁移测试表
	if err := testDB.AutoMigrate(&Config{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}

	// ====================== 1. 创建工具注册中心 ======================
	registry := NewToolRegistry()

	// 2. 注册 user_config 工具
	RegisterUserConfigTools(registry)

	// ====================== 测试 1：设置用户偏好 ======================
	t.Log("=== 测试 1：设置用户偏好 ===")
	setPrefCall := schema.ToolCall{
		ID: "test_set_pref",
		Function: schema.FunctionCall{
			Name: "user_config",
			Arguments: `{
				"action": "set_preference",
				"key": "theme",
				"value": "dark"
			}`,
		},
	}
	result := registry.Execute(context.Background(), setPrefCall)
	if result.IsError {
		t.Fatalf("设置用户偏好失败：%s", result.Content)
	}
	t.Logf("✅ 设置用户偏好成功：%s", result.Content)

	// ====================== 测试 2：设置另一个用户偏好 ======================
	t.Log("\n=== 测试 2：设置另一个用户偏好 ===")
	setPrefCall2 := schema.ToolCall{
		ID: "test_set_pref2",
		Function: schema.FunctionCall{
			Name: "user_config",
			Arguments: `{
				"action": "set_preference",
				"key": "language",
				"value": "zh-CN"
			}`,
		},
	}
	result = registry.Execute(context.Background(), setPrefCall2)
	if result.IsError {
		t.Fatalf("设置用户偏好失败：%s", result.Content)
	}
	t.Logf("✅ 设置用户偏好成功：%s", result.Content)

	// ====================== 测试 3：获取所有用户偏好键（新功能） ======================
	t.Log("\n=== 测试 3：获取所有用户偏好键 ===")
	listPrefKeysCall := schema.ToolCall{
		ID: "test_list_pref_keys",
		Function: schema.FunctionCall{
			Name: "user_config",
			Arguments: `{
				"action": "list_preference_keys"
			}`,
		},
	}
	result = registry.Execute(context.Background(), listPrefKeysCall)
	if result.IsError {
		t.Fatalf("获取用户偏好键失败：%s", result.Content)
	}
	t.Logf("✅ 获取用户偏好键成功：%s", result.Content)

	// ====================== 测试 4：设置运行规则 ======================
	t.Log("\n=== 测试 4：设置运行规则 ===")
	setRulesCall := schema.ToolCall{
		ID: "test_set_rules",
		Function: schema.FunctionCall{
			Name: "user_config",
			Arguments: `{
				"action": "set_rules",
				"key": "max_tool_calls",
				"value": 10
			}`,
		},
	}
	result = registry.Execute(context.Background(), setRulesCall)
	if result.IsError {
		t.Fatalf("设置运行规则失败：%s", result.Content)
	}
	t.Logf("✅ 设置运行规则成功：%s", result.Content)

	// ====================== 测试 5：获取所有运行规则键（新功能） ======================
	t.Log("\n=== 测试 5：获取所有运行规则键 ===")
	listRulesKeysCall := schema.ToolCall{
		ID: "test_list_rules_keys",
		Function: schema.FunctionCall{
			Name: "user_config",
			Arguments: `{
				"action": "list_rules_keys"
			}`,
		},
	}
	result = registry.Execute(context.Background(), listRulesKeysCall)
	if result.IsError {
		t.Fatalf("获取运行规则键失败：%s", result.Content)
	}
	t.Logf("✅ 获取运行规则键成功：%s", result.Content)

	// ====================== 打印完整测试结果 ======================
	fmt.Println("\n────────────────────────────────────────────────────────────────")
	fmt.Println("🎉 所有测试通过！")
	fmt.Println("📝 测试内容：")
	fmt.Println("   1. 设置用户偏好（theme=dark）")
	fmt.Println("   2. 设置用户偏好（language=zh-CN）")
	fmt.Println("   3. 获取所有用户偏好键")
	fmt.Println("   4. 设置运行规则（max_tool_calls=10）")
	fmt.Println("   5. 获取所有运行规则键")
	fmt.Println("────────────────────────────────────────────────────────────────")
}
