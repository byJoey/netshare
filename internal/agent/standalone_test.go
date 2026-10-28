package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"netshare/internal/model"
)

func TestFirstNodeCanStartWithEmptyDenyAllTargets(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	cfg.AllowedTargets = []string{}
	runner := &fakeRunner{}
	s, err := NewServer(cfg, runner)
	if err != nil {
		t.Fatal("首台节点应能先启动待配对：", err)
	}
	if err = s.validateRules(nil, nil); err != nil {
		t.Fatal("空规则应该有效：", err)
	}
	if _, err = s.Apply(context.Background(), model.ApplyRequest{Revision: 1, Rules: []model.AgentRule{testRule()}}); err == nil {
		t.Fatal("空白名单不应接受任何非空转发规则")
	}
	if _, err = s.Probe(context.Background(), model.ProbeRequest{TargetIP: testRule().TargetIP, TargetPort: "8080", Protocol: "tcp"}); err == nil {
		t.Fatal("空白名单不应允许探测任何地址")
	}
	if len(runner.mutations) != 0 {
		t.Fatal("拒绝非白名单请求不应修改系统规则")
	}
	// 已存规则也必须服从空白名单，不能借重启绕过。
	data, err := json.Marshal(diskState{NodeID: cfg.Node.ID, Revision: 1, Rules: []model.AgentRule{testRule()}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "state.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = NewServer(cfg, runner); err == nil {
		t.Fatal("空白名单不应加载带有目标的历史规则")
	}
}
