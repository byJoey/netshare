package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"netshare/internal/model"
	"netshare/internal/registration"
)

func namedNodeRequest(t *testing.T, c *Controller, method, endpoint, body string) *httptest.ResponseRecorder {
	t.Helper()
	id, session := c.auth.create(c.cfg.Username)
	return request(t, c.Handler(nil), method, c.cfg.AccessPath+endpoint, body,
		&http.Cookie{Name: "netshare_session", Value: id}, session.CSRF, c.cfg.ExternalURL)
}

func namedTicket(t *testing.T, c *Controller, name string) joinTicket {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"name": name})
	w := namedNodeRequest(t, c, "POST", "/api/node-joins", string(body))
	if w.Code != 200 {
		t.Fatalf("生成命名接入命令失败：%d %s", w.Code, w.Body.String())
	}
	var out struct{ ID string }
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	ticket, err := c.readTicket(out.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func TestNodeRenamePersistsWithoutChangingIdentityOrMappings(t *testing.T) {
	c, _, _ := joinFixture(t)
	if err := c.commitMappings(context.Background(), []model.Mapping{testMapping("keep-mapping", "a", "b", "18080")}, "创建", "命名验收"); err != nil {
		t.Fatal(err)
	}
	before := c.snapshot()
	nodes := c.nodes()
	agents := c.agents.(*fakeAgents)
	rulesBefore, _ := json.Marshal(agents.states)
	w := namedNodeRequest(t, c, "PATCH", "/api/nodes/a/name", `{"name":"  香港入口 😀  "}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	nodes[0].Name = "香港入口 😀"
	if !reflect.DeepEqual(nodes, c.nodes()) {
		t.Fatal("改名改变了节点身份、地址或保留端口")
	}
	after := c.snapshot()
	if before.Revision != after.Revision || !reflect.DeepEqual(before.Mappings, after.Mappings) {
		t.Fatal("改名改变了映射或修订号")
	}
	rulesAfter, _ := json.Marshal(agents.states)
	if string(rulesBefore) != string(rulesAfter) {
		t.Fatal("改名向代理下发了网络变更")
	}
	if event := after.Events[len(after.Events)-1]; event.Action != "修改节点名称" || !event.Success {
		t.Fatal("未记录改名")
	}
	cfg, err := LoadConfig(c.configPath)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(cfg, agents)
	if err != nil || !reflect.DeepEqual(restarted.nodes(), nodes) {
		t.Fatal("重启后节点名丢失", err)
	}
	// 同名允许，映射始终通过不同节点ID引用。
	w = namedNodeRequest(t, c, "PATCH", "/api/nodes/b/name", `{"name":"香港入口 😀"}`)
	if w.Code != 200 || c.nodes()[0].ID == c.nodes()[1].ID {
		t.Fatal("同名影响了节点身份")
	}
}

func TestNodeRenameAuthenticationAndCSRF(t *testing.T) {
	c, _, _ := joinFixture(t)
	id, session := c.auth.create(c.cfg.Username)
	cookie := &http.Cookie{Name: "netshare_session", Value: id}
	for _, tc := range []struct {
		name         string
		cookie       *http.Cookie
		csrf, origin string
		code         int
	}{
		{"未登录", nil, "", c.cfg.ExternalURL, 401},
		{"缺少CSRF", cookie, "", c.cfg.ExternalURL, 403},
		{"跨站", cookie, session.CSRF, "https://other.test", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := request(t, c.Handler(nil), "PATCH", c.cfg.AccessPath+"/api/nodes/a/name", `{"name":"禁止修改"}`, tc.cookie, tc.csrf, tc.origin)
			if w.Code != tc.code {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	if c.nodes()[0].Name != "机器 A" {
		t.Fatal("拒绝的请求仍修改了名称")
	}
}

func TestNodeRenameValidationAndFailedWriteKeepOriginal(t *testing.T) {
	c, _, _ := joinFixture(t)
	before, err := os.ReadFile(c.configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "   ", strings.Repeat("字", 81), "节点\n换行", "\t节点", "节点\x00", "节点\u0085"} {
		body, _ := json.Marshal(map[string]string{"name": name})
		w := namedNodeRequest(t, c, "PATCH", "/api/nodes/a/name", string(body))
		if w.Code != 400 {
			t.Fatalf("接受了非法名称：%q HTTP %d", name, w.Code)
		}
	}
	for _, body := range []string{`{"name":"合法名称","id":"另一个身份"}`, `{"name":"合法名称"} {}`, `{"name":42}`} {
		if w := namedNodeRequest(t, c, "PATCH", "/api/nodes/a/name", body); w.Code != 400 {
			t.Fatal("接受了无效改名请求", w.Code)
		}
	}
	if w := namedNodeRequest(t, c, "PATCH", "/api/nodes/missing/name", `{"name":"不存在"}`); w.Code != 404 {
		t.Fatal("不存在的节点未返回404")
	}
	after, _ := os.ReadFile(c.configPath)
	if string(before) != string(after) {
		t.Fatal("非法请求改动了磁盘配置")
	}
	originalPath := c.configPath
	c.configPath = filepath.Join(t.TempDir(), "directory")
	if err = os.Mkdir(c.configPath, 0700); err != nil {
		t.Fatal(err)
	}
	w := namedNodeRequest(t, c, "PATCH", "/api/nodes/a/name", `{"name":"不能保存"}`)
	if w.Code != 500 || c.nodes()[0].Name != "机器 A" {
		t.Fatal("写入失败后名称未保持原样", w.Code)
	}
	after, _ = os.ReadFile(originalPath)
	if string(before) != string(after) {
		t.Fatal("写入失败改动了原配置")
	}
}

func TestJoinCustomNameSurvivesRetryAndRejoin(t *testing.T) {
	c, server, _ := joinFixture(t)
	ticket := namedTicket(t, c, "  美国主机  ")
	if ticket.Name != "美国主机" {
		t.Fatal("接入名字未保存或规范化")
	}
	client := ticketClient(t, server, c, ticket)
	report, _ := reportFor(t, "10.50.3.21")
	var reply registration.Reply
	if err := client.Request(context.Background(), "register", report, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Node.Name != "美国主机" || reply.Node.ID != report.Node.ID {
		t.Fatal("未采用指定名字，或改动了身份")
	}
	certificate := append([]byte{}, reply.Certificate...)
	w := namedNodeRequest(t, c, "PATCH", "/api/nodes/"+reply.Node.ID+"/name", `{"name":"美国备用机"}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := client.Request(context.Background(), "register", report, &reply); err != nil {
		t.Fatal("原命令重试失败", err)
	}
	node, _ := c.node(report.Node.ID)
	if node.Name != "美国备用机" {
		t.Fatal("原接入命令重试覆盖了后来的改名")
	}
	report.PreviousCertificate = certificate
	for _, name := range []string{"", "重接时指定名称"} {
		ticket = namedTicket(t, c, name)
		if err := ticketClient(t, server, c, ticket).Request(context.Background(), "register", report, &reply); err != nil {
			t.Fatal("原机重新接入失败", err)
		}
		expected := "美国备用机"
		if name != "" {
			expected = name
		}
		node, _ = c.node(report.Node.ID)
		if node.Name != expected || reply.Node.ID != report.Node.ID {
			t.Fatal("重新接入名称优先级或身份错误")
		}
	}
	w = namedNodeRequest(t, c, "GET", "/api/node-joins/"+ticket.ID, "")
	var status struct{ Name string }
	if json.Unmarshal(w.Body.Bytes(), &status) != nil || status.Name != "重接时指定名称" {
		t.Fatal("接入成功状态未返回当前名称")
	}
	cfg, err := LoadConfig(c.configPath)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(cfg, c.agents)
	if err != nil {
		t.Fatal(err)
	}
	node, _ = restarted.node(report.Node.ID)
	if node.Name != "重接时指定名称" {
		t.Fatal("自动接入节点改名未跨重启保存")
	}
}

func TestJoinNameValidationAndAutomaticFallback(t *testing.T) {
	c, server, _ := joinFixture(t)
	for _, name := range []string{strings.Repeat("😀", 81), "名称\n", "\x00"} {
		body, _ := json.Marshal(map[string]string{"name": name})
		if w := namedNodeRequest(t, c, "POST", "/api/node-joins", string(body)); w.Code != 400 {
			t.Fatal("非法名字生成了接入命令", w.Code)
		}
	}
	if _, err := normalizeNodeName(string([]byte{0xff}), false); err == nil {
		t.Fatal("接受了非法UTF-8")
	}
	ticket := namedTicket(t, c, "   ")
	report, _ := reportFor(t, "10.50.3.23")
	var reply registration.Reply
	if err := ticketClient(t, server, c, ticket).Request(context.Background(), "register", report, &reply); err != nil || reply.Node.Name != report.Node.Name {
		t.Fatal("留空未使用自动识别名", err)
	}
	ticket = namedTicket(t, c, strings.Repeat("😀", 80))
	if len([]rune(ticket.Name)) != 80 {
		t.Fatal("未按Unicode字符计算80字边界")
	}
}
