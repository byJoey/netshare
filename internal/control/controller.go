package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"netshare/internal/model"
	"netshare/internal/registration"
)

type Controller struct {
	cfg        Config
	configPath string
	agents     Agents
	mu         sync.RWMutex
	mutation   sync.Mutex
	state      State
	auth       *authState
	joinMu     sync.Mutex
	signNode   func(context.Context, registration.SignRequest) (registration.SignReply, error)
}

func New(cfg Config, agents Agents) (*Controller, error) {
	if agents == nil {
		client, e := agentHTTPClient(cfg)
		if e != nil {
			return nil, e
		}
		agents = &remoteAgents{client: client}
	}
	s, e := loadState(cfg.StateFile)
	if e != nil {
		return nil, e
	}
	c := &Controller{cfg: cfg, agents: agents, state: s, auth: newAuth()}
	_, statErr := os.Stat(cfg.StateFile)
	fresh := os.IsNotExist(statErr)
	if fresh {
		// 状态误删不能被当作首次安装，空配置会覆盖节点上的现有业务。
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, n := range c.nodeStatuses(ctx) {
			if !n.Online || n.Status == nil {
				return nil, fmt.Errorf("首次初始化必须确认所有节点为空：%s 暂不可用；若之前已使用，请恢复控制台状态备份", n.Name)
			}
			if n.Status.SavedRevision > 0 || len(n.Status.Rules) > 0 {
				return nil, fmt.Errorf("控制台状态文件缺失，但 %s 已有历史配置，已拒绝空同步；请恢复状态备份", n.Name)
			}
		}
	}
	if e = c.validateMappings(s.Mappings); e != nil {
		return nil, fmt.Errorf("现有映射配置无效: %w", e)
	}
	// 在第一次下发之前落空状态，避免初始化中途退出后仅节点版本前进。
	if fresh {
		if e = atomicJSON(cfg.StateFile, s); e != nil {
			return nil, fmt.Errorf("保存初始控制台状态失败: %w", e)
		}
	}
	return c, nil
}

func randomID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func (c *Controller) snapshot() State {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.state
	s.Mappings = append([]model.Mapping{}, s.Mappings...)
	s.Events = append([]model.AuditEvent{}, s.Events...)
	return s
}
func (c *Controller) node(id string) (model.Node, bool) {
	for _, n := range c.nodes() {
		if n.ID == id {
			return n, true
		}
	}
	return model.Node{}, false
}

func (c *Controller) nodes() []model.Node {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]model.Node{}, c.cfg.Nodes...)
}

func (c *Controller) targets(id string) []string {
	result := []string{}
	for _, node := range c.nodes() {
		if node.ID != id {
			result = append(result, node.PrivateIP)
		}
	}
	return result
}

func (c *Controller) applyRequest(n model.Node, revision uint64, mappings []model.Mapping) model.ApplyRequest {
	targets := c.targets(n.ID)
	return model.ApplyRequest{Revision: revision, Rules: c.rules(mappings, n.ID), AllowedTargets: &targets}
}
func (c *Controller) rules(maps []model.Mapping, node string) []model.AgentRule {
	rs := []model.AgentRule{}
	for _, m := range maps {
		if m.SourceNode != node {
			continue
		}
		t, _ := c.node(m.TargetNode)
		rs = append(rs, model.AgentRule{ID: m.ID, Name: m.Name, Protocol: m.Protocol, ListenPort: m.ListenPort, TargetPort: m.TargetPort, TargetIP: t.PrivateIP, Enabled: m.Enabled})
	}
	return rs
}

func (c *Controller) validateMappings(maps []model.Mapping) error {
	if len(maps) > 200 {
		return fmt.Errorf("最多支持 200 条映射")
	}
	ids := map[string]bool{}
	for i, m := range maps {
		if !model.ValidID(m.ID) || ids[m.ID] {
			return fmt.Errorf("映射 ID 无效或重复")
		}
		ids[m.ID] = true
		if strings.TrimSpace(m.Name) == "" || utf8.RuneCountInString(m.Name) > 80 {
			return fmt.Errorf("映射名称应为 1–80 个字")
		}
		s, ok := c.node(m.SourceNode)
		if !ok {
			return fmt.Errorf("入口机器不存在")
		}
		if _, ok = c.node(m.TargetNode); !ok || m.SourceNode == m.TargetNode {
			return fmt.Errorf("目标必须是另一台已配置机器")
		}
		if e := model.ValidatePorts(m.Protocol, m.ListenPort, m.TargetPort); e != nil {
			return e
		}
		p, _ := model.ParsePorts(m.ListenPort)
		for _, port := range append([]int{22}, s.ReservedPorts...) {
			if p.Contains(port) {
				return fmt.Errorf("%s 的端口 %d 已保留，不能用于映射", s.Name, port)
			}
		}
		if !m.Enabled {
			continue
		}
		for j := 0; j < i; j++ {
			o := maps[j]
			if o.Enabled && o.SourceNode == m.SourceNode && model.ProtocolsOverlap(o.Protocol, m.Protocol) {
				q, _ := model.ParsePorts(o.ListenPort)
				if p.Overlaps(q) {
					return fmt.Errorf("入口端口与映射「%s」重叠", o.Name)
				}
			}
		}
	}
	return nil
}

func (c *Controller) persist(s State) error {
	if e := atomicJSON(c.cfg.StateFile, s); e != nil {
		return e
	}
	c.mu.Lock()
	c.state = s
	c.mu.Unlock()
	return nil
}
func appendEvent(s *State, action, detail string, success bool) {
	s.Events = append(s.Events, model.AuditEvent{ID: randomID(), Time: time.Now().UTC(), Action: action, Detail: detail, Success: success})
	if len(s.Events) > 500 {
		s.Events = append([]model.AuditEvent{}, s.Events[len(s.Events)-500:]...)
	}
}
func (c *Controller) audit(action, detail string, success bool) {
	c.mutation.Lock()
	defer c.mutation.Unlock()
	s := c.snapshot()
	appendEvent(&s, action, detail, success)
	if e := c.persist(s); e != nil {
		slog.Error("保存操作记录失败", "error", e)
	}
}

func (c *Controller) rollback(old State, nodes []model.Node, rev uint64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	var failures []string
	for _, n := range nodes {
		_, e := c.agents.Apply(ctx, n, c.applyRequest(n, rev, old.Mappings))
		if e != nil {
			failures = append(failures, e.Error())
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("规则回退未全部完成，请在机器恢复后点击重新同步: %s", strings.Join(failures, "；"))
	}
	return nil
}

func (c *Controller) commitMappings(ctx context.Context, maps []model.Mapping, action, detail string) error {
	if e := c.validateMappings(maps); e != nil {
		return e
	}
	old := c.snapshot()
	next := old
	next.Mappings = maps
	next.Revision = old.Revision + 1
	var applied []model.Node
	for _, n := range c.nodes() {
		before, after := c.rules(old.Mappings, n.ID), c.rules(maps, n.ID)
		if reflect.DeepEqual(before, after) {
			continue
		}
		// 把本次失败的机器也加入回退，覆盖响应丢失但代理已经应用的情况。
		applied = append(applied, n)
		if _, e := c.agents.Apply(ctx, n, c.applyRequest(n, next.Revision, maps)); e != nil {
			re := c.rollback(old, applied, next.Revision+1)
			old.Revision = next.Revision + 1
			appendEvent(&old, action, e.Error(), false)
			pe := c.persist(old)
			if pe != nil {
				c.mu.Lock()
				c.state = old
				c.mu.Unlock()
				pe = fmt.Errorf("保存回退状态失败，请修复磁盘后重新同步: %w", pe)
			}
			if re != nil || pe != nil {
				return fmt.Errorf("%w；%v", e, errors.Join(re, pe))
			}
			return fmt.Errorf("应用失败，已恢复原映射: %w", e)
		}
	}
	appendEvent(&next, action, detail, true)
	if e := c.persist(next); e != nil {
		re := c.rollback(old, applied, next.Revision+1)
		old.Revision = next.Revision + 1
		appendEvent(&old, action, "保存配置失败，已请求回退: "+e.Error(), false)
		pe := c.persist(old)
		if pe != nil {
			c.mu.Lock()
			c.state = old
			c.mu.Unlock()
			pe = fmt.Errorf("保存回退状态失败，请修复磁盘后重新同步: %w", pe)
		}
		if re != nil || pe != nil {
			return fmt.Errorf("保存配置失败: %v；%w", e, errors.Join(re, pe))
		}
		return fmt.Errorf("保存配置失败，已回退远端规则: %w", e)
	}
	return nil
}

func (c *Controller) Reconcile(ctx context.Context) error {
	c.mutation.Lock()
	defer c.mutation.Unlock()
	s := c.snapshot()
	// 恢复旧控制台备份时，代理可能持有更高的修订号，先对齐再下发。
	for _, status := range c.nodeStatuses(ctx) {
		if status.Status != nil && status.Status.SavedRevision > s.Revision {
			s.Revision = status.Status.SavedRevision
		}
		if status.Status != nil && status.Status.Revision > s.Revision {
			s.Revision = status.Status.Revision
		}
	}
	s.Revision++
	var failures []string
	for _, n := range c.nodes() {
		if _, e := c.agents.Apply(ctx, n, c.applyRequest(n, s.Revision, s.Mappings)); e != nil {
			failures = append(failures, e.Error())
		}
	}
	detail := "已重新同步全部机器"
	if len(failures) > 0 {
		detail = strings.Join(failures, "；")
	}
	appendEvent(&s, "重新同步", detail, len(failures) == 0)
	if e := c.persist(s); e != nil {
		return e
	}
	if len(failures) > 0 {
		return fmt.Errorf("%s", detail)
	}
	return nil
}

func (c *Controller) nodeStatuses(ctx context.Context) []model.NodeStatus {
	nodes := c.nodes()
	out := make([]model.NodeStatus, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func(i int, n model.Node) {
			defer wg.Done()
			t := time.Now()
			child, cancel := context.WithTimeout(ctx, 4*time.Second)
			defer cancel()
			s, e := c.agents.Status(child, n)
			ns := model.NodeStatus{Node: n, LatencyMS: time.Since(t).Milliseconds()}
			ns.AgentURL = ""
			if e != nil {
				ns.Error = e.Error()
			} else if s.NodeID != n.ID {
				ns.Error = "代理机器身份与配置不符"
			} else {
				ns.Online = true
				ns.Status = &s
			}
			out[i] = ns
		}(i, n)
	}
	wg.Wait()
	return out
}
