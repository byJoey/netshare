package agent

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"netshare/internal/model"
)

func TestAutomaticTargetsAreAtomicAndSurviveRestart(t *testing.T) {
	s, runner := newTestServer(t)
	targets := []string{"10.42.0.12"}
	rule := testRule()
	rule.TargetIP = targets[0]
	req := model.ApplyRequest{Revision: 1, Rules: []model.AgentRule{rule}, AllowedTargets: &targets}
	if _, err := s.Apply(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if s.allowed["10.42.0.11"] || !s.allowed[targets[0]] {
		t.Fatal("白名单未切换")
	}
	again, err := NewServer(s.cfg, runner)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.allowed, again.allowed) {
		t.Fatal("重启丢失自动白名单")
	}
	original := s.writeFile
	s.writeFile = func(string, []byte, os.FileMode) error { return errors.New("模拟磁盘故障") }
	targets = []string{"10.42.0.13"}
	rule.TargetIP = targets[0]
	if _, err = s.Apply(context.Background(), model.ApplyRequest{Revision: 2, Rules: []model.AgentRule{rule}, AllowedTargets: &targets}); err == nil {
		t.Fatal("写失败却成功")
	}
	s.writeFile = original
	if !s.allowed["10.42.0.12"] || s.allowed["10.42.0.13"] || s.state.Revision != 1 {
		t.Fatal("失败未回退白名单")
	}
	for _, invalid := range [][]string{{"1.1.1.1"}, {s.cfg.Node.PrivateIP}, {"10.42.0.12", "10.42.0.12"}} {
		if _, err = s.Apply(context.Background(), model.ApplyRequest{Revision: 3, AllowedTargets: &invalid}); err == nil {
			t.Fatal("接受非法自动白名单")
		}
	}
	empty := []string{}
	if _, err = s.Apply(context.Background(), model.ApplyRequest{Revision: 2, Rules: []model.AgentRule{}, AllowedTargets: &empty}); err != nil {
		t.Fatal(err)
	}
	again, err = NewServer(s.cfg, runner)
	if err != nil || len(again.allowed) != 0 {
		t.Fatal("空白名单重启又恢复旧目标", err)
	}
}
