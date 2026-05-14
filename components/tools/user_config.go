package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Config GORM模型定义，对应configs表
type Config struct {
	ID        uint   `gorm:"primaryKey;autoIncrement"`
	Type      string `gorm:"type:text;not null;uniqueIndex:idx_type_key,priority:1"` // 唯一索引组合
	Key       string `gorm:"type:text;not null;uniqueIndex:idx_type_key,priority:2"` // 唯一索引组合
	Value     string `gorm:"type:text;not null"`
	UpdatedAt int64  `gorm:"not null"`
}

func (Config) TableName() string {
	return "configs"
}

// UserConfig 用户配置管理工具（含获取所有键功能）
func UserConfig(ctx context.Context, args map[string]any) (any, error) {
	// 1. 参数校验
	action, ok := args["action"].(string)
	if !ok || action == "" {
		return nil, fmt.Errorf("action is required (get_preference/set_preference/get_rules/set_rules/list_preference_keys/list_rules_keys)")
	}

	// 2. 获取GORM数据库连接
	db, err := getConfigDB()
	if err != nil {
		return nil, fmt.Errorf("get config db failed: %w", err)
	}

	// 3. 根据 action 执行不同操作
	switch action {
	case "get_preference":
		return getConfig(ctx, db, "preference", args)
	case "set_preference":
		return setConfig(ctx, db, "preference", args)
	case "get_rules":
		return getConfig(ctx, db, "rules", args)
	case "set_rules":
		return setConfig(ctx, db, "rules", args)
	case "list_preference_keys":
		return listConfigKeys(ctx, db, "preference")
	case "list_rules_keys":
		return listConfigKeys(ctx, db, "rules")
	default:
		return nil, fmt.Errorf("invalid action: %s (supported: get_preference/set_preference/get_rules/set_rules/list_preference_keys/list_rules_keys)", action)
	}
}

// listConfigKeys 获取所有配置键（GORM版）
func listConfigKeys(ctx context.Context, db *gorm.DB, configType string) (any, error) {
	var configs []Config
	// 按updated_at倒序查询，仅查key和updated_at字段
	result := db.WithContext(ctx).
		Select("key", "updated_at").
		Where("type = ?", configType).
		Order("updated_at DESC").
		Find(&configs)

	if result.Error != nil {
		return nil, fmt.Errorf("query config keys failed: %w", result.Error)
	}

	var keys []map[string]any
	for _, cfg := range configs {
		keys = append(keys, map[string]any{
			"key":        cfg.Key,
			"updated_at": cfg.UpdatedAt,
		})
	}

	return map[string]any{
		"type":   configType,
		"keys":   keys,
		"total":  len(keys),
		"status": "success",
	}, nil
}

// getConfig 获取配置（GORM版）
func getConfig(ctx context.Context, db *gorm.DB, configType string, args map[string]any) (any, error) {
	key, _ := args["key"].(string)

	if key != "" {
		// 获取单个配置
		var cfg Config
		result := db.WithContext(ctx).
			Where("type = ? AND key = ?", configType, key).
			First(&cfg)

		if result.Error != nil {
			if result.Error == gorm.ErrRecordNotFound {
				return map[string]any{
					"type":   configType,
					"key":    key,
					"value":  nil,
					"status": "not_found",
				}, nil
			}
			return nil, fmt.Errorf("query config failed: %w", result.Error)
		}

		var parsedValue any
		_ = json.Unmarshal([]byte(cfg.Value), &parsedValue)

		return map[string]any{
			"type":   configType,
			"key":    key,
			"value":  parsedValue,
			"status": "success",
		}, nil
	}

	// 获取全部配置
	var configs []Config
	result := db.WithContext(ctx).
		Where("type = ?", configType).
		Find(&configs)

	if result.Error != nil {
		return nil, fmt.Errorf("query all configs failed: %w", result.Error)
	}

	configMap := make(map[string]any)
	for _, cfg := range configs {
		var parsedValue any
		_ = json.Unmarshal([]byte(cfg.Value), &parsedValue)
		configMap[cfg.Key] = parsedValue
	}

	return map[string]any{
		"type":    configType,
		"configs": configMap,
		"status":  "success",
	}, nil
}

// setConfig 设置配置（GORM版，支持UPSERT）
func setConfig(ctx context.Context, db *gorm.DB, configType string, args map[string]any) (any, error) {
	key, ok := args["key"].(string)
	if !ok || key == "" {
		return nil, fmt.Errorf("key is required for set action")
	}

	value, ok := args["value"]
	if !ok {
		return nil, fmt.Errorf("value is required for set action")
	}

	// 序列化value为JSON字符串
	valueJSON, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal value failed: %w", err)
	}

	now := time.Now().Unix()
	cfg := Config{
		Type:      configType,
		Key:       key,
		Value:     string(valueJSON),
		UpdatedAt: now,
	}

	// UPSERT逻辑：存在则更新，不存在则创建
	result := db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "type"}, {Name: "key"}}, // 唯一键组合
			DoUpdates: clause.Assignments(map[string]interface{}{"value": cfg.Value, "updated_at": cfg.UpdatedAt}),
		}).
		Create(&cfg)

	if result.Error != nil {
		return nil, fmt.Errorf("save config failed: %w", result.Error)
	}

	return map[string]any{
		"type":       configType,
		"key":        key,
		"value":      value,
		"status":     "success",
		"updated_at": now,
	}, nil
}

// getConfigDB 获取GORM数据库连接（替代原生sql.DB）
func getConfigDB() (*gorm.DB, error) {
	// 连接SQLite数据库（保持原路径）
	db, err := gorm.Open(sqlite.Open("./user_config.db"), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("open sqlite db failed: %w", err)
	}

	// 自动迁移表结构（替代原CREATE TABLE语句）
	err = db.AutoMigrate(&Config{})
	if err != nil {
		return nil, fmt.Errorf("migrate configs table failed: %w", err)
	}

	return db, nil
}

// userConfigParams 用户配置管理参数定义（保持不变）
var userConfigParams = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"action": map[string]any{
			"type":        "string",
			"description": "操作类型（必填）",
			"enum": []string{
				"get_preference", "set_preference",
				"get_rules", "set_rules",
				"list_preference_keys", "list_rules_keys",
			},
		},
		"key": map[string]any{
			"type":        "string",
			"description": "配置键（get/set可选/必填，list操作不需要）",
		},
		"value": map[string]any{
			"type":        "object",
			"description": "配置值（仅set操作需要，支持任意类型）",
		},
	},
	"required": []string{"action"},
}

// RegisterUserConfigTools 注册用户配置管理工具（保持不变）
func RegisterUserConfigTools(registry *ToolRegistry) {
	registry.MustRegister(ToolMetadata{
		Name:        "user_config",
		Description: "用户配置管理工具，支持获取/设置用户偏好和运行规则，以及查看所有可用配置键",
		Parameters:  userConfigParams,
		Permission:  PermReadWrite,
		Category:    "config",
		Version:     "2.0.0",
		Tags:        []string{"config", "preference", "rules", "user", "list"},
		Timeout:     10 * time.Second,
	}, UserConfig)
}
