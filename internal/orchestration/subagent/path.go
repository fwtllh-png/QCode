package subagent

import (
	"fmt"
	"strings"
	"unicode"
)

func (m *Manager) nextPathLocked(parentID, taskName, agentID string) string {
	parent := "/root"
	if owner := m.agents[parentID]; owner != nil && owner.Path != "" {
		parent = owner.Path
	}
	name := pathSegment(taskName)
	candidate := parent + "/" + name
	for _, agents := range []map[string]*Agent{m.agents, m.provisioning} {
		for _, agent := range agents {
			if agent.Path == candidate {
				return fmt.Sprintf("%s_%s", candidate, strings.TrimPrefix(agentID, "agent-"))
			}
		}
	}
	return candidate
}

func pathSegment(value string) string {
	var out strings.Builder
	separator := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			out.WriteRune(r)
			separator = false
		case out.Len() > 0 && !separator:
			out.WriteByte('_')
			separator = true
		}
		if out.Len() >= 48 {
			break
		}
	}
	segment := strings.Trim(out.String(), "_")
	if segment == "" {
		return "agent"
	}
	return segment
}
