package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ConfigFile MCP 配置文件格式
type ConfigFile struct {
	MCPServers []ServerConfig `json:"mcpServers"`
}

// LoadConfig 从 JSON 文件加载 MCP 配置
func LoadConfig(path string) ([]ServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var config ConfigFile
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// 替换环境变量 ${VAR}
	for i := range config.MCPServers {
		s := &config.MCPServers[i]
		s.Transport.Command = expandEnv(s.Transport.Command)
		for j := range s.Transport.Args {
			s.Transport.Args[j] = expandEnv(s.Transport.Args[j])
		}
		for j := range s.Transport.Env {
			s.Transport.Env[j] = expandEnv(s.Transport.Env[j])
		}
	}

	return config.MCPServers, nil
}

// expandEnv 替换 ${VAR} 为环境变量值
func expandEnv(s string) string {
	return os.Expand(s, func(key string) string {
		return os.Getenv(key)
	})
}

// ConnectFromConfig 从配置文件连接所有 MCP 服务器
func (m *Manager) ConnectFromConfig(ctx context.Context, configPath string) error {
	configs, err := LoadConfig(configPath)
	if err != nil {
		return err
	}

	errs := m.ConnectAll(ctx, configs)
	if len(errs) > 0 {
		var msgs []string
		for _, e := range errs {
			msgs = append(msgs, e.Error())
		}
		return fmt.Errorf("mcp connect errors: %s", strings.Join(msgs, "; "))
	}

	return nil
}
