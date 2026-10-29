package control

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

func normalizeNodeName(value string, optional bool) (string, error) {
	if !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("节点名称不能包含控制字符")
	}
	name := strings.TrimSpace(value)
	if optional && name == "" {
		return "", nil
	}
	if name == "" || utf8.RuneCountInString(name) > 80 {
		return "", fmt.Errorf("节点名称应为 1–80 个字")
	}
	return name, nil
}

func (c *Controller) handleRenameNode(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	name, err := normalizeNodeName(in.Name, false)
	if err != nil {
		apiError(w, 400, err.Error())
		return
	}
	c.mutation.Lock()
	defer c.mutation.Unlock()
	nodes := c.nodes()
	for i, node := range nodes {
		if node.ID != r.PathValue("id") {
			continue
		}
		if node.Name == name {
			writeJSON(w, 200, map[string]string{"id": node.ID, "name": name})
			return
		}
		if c.configPath == "" {
			apiError(w, 500, "未设置配置文件路径，名称未修改")
			return
		}
		nodes[i].Name = name
		c.mu.RLock()
		cfg := c.cfg
		c.mu.RUnlock()
		cfg.Nodes = nodes
		if err = atomicJSON(c.configPath, cfg); err != nil {
			apiError(w, 500, "保存节点名称失败，原名称已保留")
			return
		}
		c.mu.Lock()
		c.cfg.Nodes = nodes
		c.mu.Unlock()
		state := c.snapshot()
		appendEvent(&state, "修改节点名称", node.Name+" → "+name, true)
		if err = c.persist(state); err != nil {
			slog.Error("保存节点改名审计失败", "error", err)
		}
		writeJSON(w, 200, map[string]string{"id": node.ID, "name": name})
		return
	}
	apiError(w, 404, "节点不存在，请刷新后重试")
}
