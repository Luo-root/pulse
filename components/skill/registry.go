package skill

import (
	"sync"
)

// SkillRegistry Skill 注册表（内存索引）
type SkillRegistry struct {
	mu     sync.RWMutex
	skills map[string]*Skill
}

// NewSkillRegistry 创建 Skill 注册表
func NewSkillRegistry() *SkillRegistry {
	return &SkillRegistry{
		skills: make(map[string]*Skill),
	}
}

// Register 注册 Skill
func (sr *SkillRegistry) Register(skill *Skill) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.skills[skill.Name] = skill
}

// Get 获取 Skill
func (sr *SkillRegistry) Get(name string) (*Skill, bool) {
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	skill, ok := sr.skills[name]
	return skill, ok
}

// List 列出所有 Skill
func (sr *SkillRegistry) List() []*Skill {
	sr.mu.RLock()
	defer sr.mu.RUnlock()

	result := make([]*Skill, 0, len(sr.skills))
	for _, skill := range sr.skills {
		result = append(result, skill)
	}
	return result
}

// Remove 移除 Skill
func (sr *SkillRegistry) Remove(name string) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	delete(sr.skills, name)
}

func (sr *SkillRegistry) Names() []string {
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	names := make([]string, 0, len(sr.skills))
	for name := range sr.skills {
		names = append(names, name)
	}
	return names
}

// GetInstructionalSkills 获取所有指令型 Skill
func (sr *SkillRegistry) GetInstructionalSkills() []*Skill {
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	var result []*Skill
	for _, s := range sr.skills {
		if s.Type == SkillTypeInstruction {
			result = append(result, s)
		}
	}
	return result
}
