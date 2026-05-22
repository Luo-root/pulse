package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Config struct {
	ID        uint   `gorm:"primaryKey;autoIncrement"`
	Type      string `gorm:"type:text;not null;uniqueIndex:idx_type_key,priority:1"`
	Key       string `gorm:"type:text;not null;uniqueIndex:idx_type_key,priority:2"`
	Value     string `gorm:"type:text;not null"`
	UpdatedAt int64  `gorm:"not null"`
}

func (Config) TableName() string { return "configs" }

// ============================================================================
// DB 单例
// ============================================================================

var (
	configDB     *gorm.DB
	configDBOnce sync.Once
	configDBErr  error
	ConfigDBPath = "./user_config.db" // 可在 init 时修改
)

func getConfigDB() (*gorm.DB, error) {
	configDBOnce.Do(func() {
		configDB, configDBErr = gorm.Open(sqlite.Open(ConfigDBPath), &gorm.Config{})
		if configDBErr != nil {
			return
		}
		configDBErr = configDB.AutoMigrate(&Config{})
	})
	return configDB, configDBErr
}

// ============================================================================
// UserConfig 工具
// ============================================================================

func UserConfig(ctx context.Context, args map[string]any) (any, error) {
	action, ok := args["action"].(string)
	if !ok || action == "" {
		return nil, fmt.Errorf("action is required (get_preference/set_preference/get_rules/set_rules/list_preference_keys/list_rules_keys)")
	}

	db, err := getConfigDB()
	if err != nil {
		return nil, fmt.Errorf("get config db failed: %w", err)
	}

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
		return nil, fmt.Errorf("invalid action: %s", action)
	}
}

func listConfigKeys(ctx context.Context, db *gorm.DB, configType string) (any, error) {
	var configs []Config
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
		keys = append(keys, map[string]any{"key": cfg.Key, "updated_at": cfg.UpdatedAt})
	}
	return map[string]any{"type": configType, "keys": keys, "total": len(keys), "status": "success"}, nil
}

func getConfig(ctx context.Context, db *gorm.DB, configType string, args map[string]any) (any, error) {
	key, _ := args["key"].(string)
	if key != "" {
		var cfg Config
		result := db.WithContext(ctx).Where("type = ? AND key = ?", configType, key).First(&cfg)
		if result.Error != nil {
			if result.Error == gorm.ErrRecordNotFound {
				return map[string]any{"type": configType, "key": key, "value": nil, "status": "not_found"}, nil
			}
			return nil, fmt.Errorf("query config failed: %w", result.Error)
		}
		var parsedValue any
		_ = json.Unmarshal([]byte(cfg.Value), &parsedValue)
		return map[string]any{"type": configType, "key": key, "value": parsedValue, "status": "success"}, nil
	}

	var configs []Config
	result := db.WithContext(ctx).Where("type = ?", configType).Find(&configs)
	if result.Error != nil {
		return nil, fmt.Errorf("query all configs failed: %w", result.Error)
	}
	configMap := make(map[string]any)
	for _, cfg := range configs {
		var parsedValue any
		_ = json.Unmarshal([]byte(cfg.Value), &parsedValue)
		configMap[cfg.Key] = parsedValue
	}
	return map[string]any{"type": configType, "configs": configMap, "status": "success"}, nil
}

func setConfig(ctx context.Context, db *gorm.DB, configType string, args map[string]any) (any, error) {
	key, ok := args["key"].(string)
	if !ok || key == "" {
		return nil, fmt.Errorf("key is required for set action")
	}
	value, ok := args["value"]
	if !ok {
		return nil, fmt.Errorf("value is required for set action")
	}
	valueJSON, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal value failed: %w", err)
	}
	now := time.Now().Unix()
	cfg := Config{Type: configType, Key: key, Value: string(valueJSON), UpdatedAt: now}
	result := db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "type"}, {Name: "key"}},
			DoUpdates: clause.Assignments(map[string]interface{}{"value": cfg.Value, "updated_at": cfg.UpdatedAt}),
		}).
		Create(&cfg)
	if result.Error != nil {
		return nil, fmt.Errorf("save config failed: %w", result.Error)
	}
	return map[string]any{"type": configType, "key": key, "value": value, "status": "success", "updated_at": now}, nil
}

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
		"key":   map[string]any{"type": "string", "description": "配置键（get/set 可选/必填，list 不需要）"},
		"value": map[string]any{"type": "object", "description": "配置值（仅 set 操作需要）"},
	},
	"required": []string{"action"},
}

func RegisterUserConfigTools(registry *ToolRegistry) {
	registry.MustRegister(ToolMetadata{
		Name:        "user_config",
		Description: "用户配置管理工具，支持获取/设置用户偏好和运行规则",
		Parameters:  userConfigParams,
		Permission:  PermReadWrite,
		Category:    "config",
		Version:     "2.0.0",
		Tags:        []string{"config", "preference", "rules", "user"},
		Timeout:     10 * time.Second,
	}, UserConfig)
}
