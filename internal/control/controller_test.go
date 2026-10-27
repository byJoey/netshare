package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"io/fs"
	"netshare/internal/model"
	"netshare/internal/webui"
)

type fakeAgents struct {
	mu       sync.Mutex
	states   map[string]model.ApplyRequest
	fail     string
	failOnce bool
}

func (f *fakeAgents) Status(_ context.Context, n model.Node) (model.AgentStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.states[n.ID]
	return model.AgentStatus{NodeID: n.ID, Revision: s.Revision, SavedRevision: s.Revision, Rules: s.Rules, Forwarding: true}, nil
}
func (f *fakeAgents) Apply(_ context.Context, n model.Node, r model.ApplyRequest) (model.ApplyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Revision < f.states[n.ID].Revision {
		return model.ApplyResult{}, fmt.Errorf("过期版本")
	}
	f.states[n.ID] = r
	if f.fail == n.ID {
		if f.failOnce {
			f.fail = ""
		}
		return model.ApplyResult{}, fmt.Errorf("模拟代理已应用但响应丢失")
	}
	return model.ApplyResult{Revision: r.Revision}, nil
}
func (f *fakeAgents) Probe(_ context.Context, _ model.Node, _ model.ProbeRequest) (model.ProbeResult, error) {
	return model.ProbeResult{Reachable: true, Interface: "eth8"}, nil
}
func testController(t *testing.T) (*Controller, *fakeAgents) {
	t.Helper()
	hash, _ := bcrypt.GenerateFromPassword([]byte("testing-password-123"), 4)
	cfg := Config{Listen: "127.0.0.1:0", ExternalURL: "https://example.test", Username: "admin", PasswordHash: string(hash), StateFile: filepath.Join(t.TempDir(), "state.json"), Nodes: []model.Node{{ID: "a", Name: "机器 A", PublicIP: "203.0.113.1", PrivateIP: "10.0.0.1", PublicInterface: "eth7", PrivateInterface: "eth8", ReservedPorts: []int{443}}, {ID: "b", Name: "机器 B", PublicIP: "203.0.113.2", PrivateIP: "10.0.0.2", PublicInterface: "eth7", PrivateInterface: "eth8"}}}
	f := &fakeAgents{states: map[string]model.ApplyRequest{}}
	c, e := New(cfg, f)
	if e != nil {
		t.Fatal(e)
	}
	return c, f
}
func testMapping(id, source, target, port string) model.Mapping {
	return model.Mapping{ID: id, Name: "测试映射", SourceNode: source, TargetNode: target, Protocol: "both", ListenPort: port, TargetPort: "80", Enabled: true}
}

func TestMappingValidationProtectsPortsAndProtocols(t *testing.T) {
	c, _ := testController(t)
	cases := []struct {
		name  string
		maps  []model.Mapping
		valid bool
	}{
		{"SSH保留", []model.Mapping{testMapping("m1", "a", "b", "22")}, false},
		{"管理台保留", []model.Mapping{testMapping("m1", "a", "b", "443")}, false},
		{"同机无效", []model.Mapping{testMapping("m1", "a", "a", "8080")}, false},
		{"入口同协议重叠", []model.Mapping{testMapping("m1", "a", "b", "8080"), testMapping("m2", "a", "b", "8080")}, false},
		{"两个方向独立", []model.Mapping{testMapping("m1", "a", "b", "8080"), testMapping("m2", "b", "a", "8080")}, true},
		{"禁用规则可共存", []model.Mapping{testMapping("m1", "a", "b", "8080"), func() model.Mapping { m := testMapping("m2", "a", "b", "8080"); m.Enabled = false; return m }()}, true},
		{"非法端口注入", []model.Mapping{testMapping("m1", "a", "b", "8080;delete table ip filter")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := c.validateMappings(tc.maps)
			if (e == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, e)
			}
		})
	}
	m := testMapping("range", "a", "b", "8080-8082")
	m.TargetPort = "80-81"
	if c.validateMappings([]model.Mapping{m}) == nil {
		t.Fatal("应拒绝不等长端口范围")
	}
}

func TestFailedCrossNodeEditRollsBackBothEvenWhenResponseLost(t *testing.T) {
	c, f := testController(t)
	original := testMapping("mapping", "a", "b", "8080")
	if e := c.commitMappings(context.Background(), []model.Mapping{original}, "创建", "test"); e != nil {
		t.Fatal(e)
	}
	edited := original
	edited.SourceNode = "b"
	edited.TargetNode = "a"
	f.fail = "b"
	f.failOnce = true
	if e := c.commitMappings(context.Background(), []model.Mapping{edited}, "更新", "test"); e == nil {
		t.Fatal("应报告失败")
	}
	s := c.snapshot()
	if len(s.Mappings) != 1 || s.Mappings[0].SourceNode != "a" {
		t.Fatalf("原映射未保留: %+v", s)
	}
	if len(f.states["a"].Rules) != 1 || len(f.states["b"].Rules) != 0 {
		t.Fatalf("远端未回退: %+v", f.states)
	}
	if f.states["a"].Revision != s.Revision || f.states["b"].Revision != s.Revision {
		t.Fatal("回退修订号不一致")
	}
	loaded, e := loadState(c.cfg.StateFile)
	if e != nil || loaded.Mappings[0].SourceNode != "a" {
		t.Fatalf("磁盘原映射未保留: %v", e)
	}
}

func TestStatePersistsAndCorruptionNeverBecomesEmpty(t *testing.T) {
	c, f := testController(t)
	m := testMapping("mapping", "a", "b", "8080")
	if e := c.commitMappings(context.Background(), []model.Mapping{m}, "创建", "test"); e != nil {
		t.Fatal(e)
	}
	again, e := New(c.cfg, f)
	if e != nil || len(again.snapshot().Mappings) != 1 {
		t.Fatal("重启丢失状态", e)
	}
	info, _ := os.Stat(c.cfg.StateFile)
	if info.Mode().Perm() != 0600 {
		t.Fatal("状态文件权限不正确")
	}
	if e = os.WriteFile(c.cfg.StateFile, []byte("broken"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = New(c.cfg, f); e == nil {
		t.Fatal("损坏状态文件不应被覆盖为空配置")
	}
	b, _ := os.ReadFile(c.cfg.StateFile)
	if string(b) != "broken" {
		t.Fatal("原损坏文件被覆盖")
	}
}

func request(t *testing.T, h http.Handler, method, path, body string, cookie *http.Cookie, csrf, origin string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	r.RemoteAddr = "192.0.2.10:40000"
	r.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func login(t *testing.T, h http.Handler) (*http.Cookie, string) {
	t.Helper()
	w := request(t, h, "POST", "/api/login", `{"username":"admin","password":"testing-password-123"}`, nil, "", "https://example.test")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var s struct {
		CSRF string `json:"csrf_token"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &s)
	return w.Result().Cookies()[0], s.CSRF
}

func TestAuthenticationCSRFAndStrictBody(t *testing.T) {
	c, _ := testController(t)
	h := c.Handler(nil)
	if w := request(t, h, "GET", "/api/overview", "", nil, "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request(t, h, "POST", "/api/login", `{"username":"admin","password":"testing-password-123"}`, nil, "", "https://evil.test"); w.Code != 403 {
		t.Fatal("跨站登录应被拒绝")
	}
	cookie, csrf := login(t, h)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("会话cookie缺少保护")
	}
	body := `{"name":"网页服务","source_node":"a","target_node":"b","protocol":"tcp","listen_port":"8080","target_port":"80","enabled":true}`
	if w := request(t, h, "POST", "/api/mappings", body, cookie, "", ""); w.Code != 403 {
		t.Fatal("缺失CSRF应拒绝", w.Code)
	}
	if w := request(t, h, "POST", "/api/mappings", body, cookie, csrf, "https://evil.test"); w.Code != 403 {
		t.Fatal("跨站变更应拒绝", w.Code)
	}
	if w := request(t, h, "POST", "/api/mappings", strings.TrimSuffix(body, "}")+`,"command":"rm"}`, cookie, csrf, ""); w.Code != 400 {
		t.Fatal("未知字段应拒绝", w.Code)
	}
	w := request(t, h, "POST", "/api/mappings", body, cookie, csrf, "")
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	s := c.snapshot()
	id := s.Mappings[0].ID
	if w = request(t, h, "POST", "/api/mappings/"+id+"/probe", "", cookie, csrf, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = request(t, h, "DELETE", "/api/mappings/"+id, "", cookie, csrf, ""); w.Code != 200 || len(c.snapshot().Mappings) != 0 {
		t.Fatal("删除失败", w.Code, w.Body.String())
	}
	if w = request(t, h, "POST", "/api/logout", "", cookie, csrf, ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w = request(t, h, "GET", "/api/overview", "", cookie, "", ""); w.Code != 401 {
		t.Fatal("退出后会话仍有效")
	}
}

func TestLoginRateLimitAndNoPasswordInExport(t *testing.T) {
	c, _ := testController(t)
	h := c.Handler(nil)
	cookie, _ := login(t, h)
	w := request(t, h, "GET", "/api/export", "", cookie, "", "")
	if strings.Contains(w.Body.String(), "password") || strings.Contains(w.Body.String(), "PRIVATE KEY") {
		t.Fatal("导出含凭据")
	}
	for i := 0; i < 8; i++ {
		w = request(t, h, "POST", "/api/login", `{"username":"admin","password":"wrong"}`, nil, "", "")
	}
	if w.Code != 429 {
		t.Fatal("登录限速未触发", w.Code)
	}
}

func TestPasswordChangePersistsAndRevokesOtherSessions(t *testing.T) {
	c, _ := testController(t)
	c.configPath = filepath.Join(t.TempDir(), "config.json")
	h := c.Handler(nil)
	old, _ := login(t, h)
	active, csrf := login(t, h)
	w := request(t, h, "POST", "/api/password", `{"current_password":"testing-password-123","new_password":"changed-password-456"}`, active, csrf, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = request(t, h, "GET", "/api/overview", "", old, "", ""); w.Code != 401 {
		t.Fatal("旧会话未撤销")
	}
	b, e := os.ReadFile(c.configPath)
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(b, []byte("changed-password-456")) {
		t.Fatal("配置保存了密码明文")
	}
	var cfg Config
	_ = json.Unmarshal(b, &cfg)
	if bcrypt.CompareHashAndPassword([]byte(cfg.PasswordHash), []byte("changed-password-456")) != nil {
		t.Fatal("新密码未持久化")
	}
}

func TestMissingControllerStateNeverErasesAgentHistory(t *testing.T) {
	c, f := testController(t)
	if e := os.Remove(c.cfg.StateFile); e != nil {
		t.Fatal(e)
	}
	f.states["a"] = model.ApplyRequest{Revision: 19, Rules: []model.AgentRule{{ID: "existing", Enabled: true}}}
	if _, e := New(c.cfg, f); e == nil {
		t.Fatal("状态缺失时必须阻止清空已有节点")
	}
	if f.states["a"].Revision != 19 || len(f.states["a"].Rules) != 1 {
		t.Fatal("检查过程修改了代理")
	}
}

func TestFreshControllerPersistsBeforeFirstAgentChange(t *testing.T) {
	c, f := testController(t)
	s, e := loadState(c.cfg.StateFile)
	if e != nil {
		t.Fatal(e)
	}
	if s.Revision != 0 || len(s.Mappings) != 0 || len(f.states) != 0 {
		t.Fatal("初始化不应更改节点规则")
	}
	if _, e = os.Stat(c.cfg.StateFile); e != nil {
		t.Fatal("首次下发前必须已落初始状态", e)
	}
}

func TestLoginWaitsForPasswordChangeBeforeCheckingHash(t *testing.T) {
	c, _ := testController(t)
	h := c.Handler(nil)
	c.mutation.Lock()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- request(t, h, "POST", "/api/login", `{"username":"admin","password":"testing-password-123"}`, nil, "", "")
	}()
	select {
	case <-done:
		c.mutation.Unlock()
		t.Fatal("登录未与改密串行")
	case <-time.After(40 * time.Millisecond):
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte("changed-password-456"), 4)
	c.mu.Lock()
	c.cfg.PasswordHash = string(hash)
	c.mu.Unlock()
	c.mutation.Unlock()
	if w := <-done; w.Code != 401 {
		t.Fatal("旧密码在改密后又创建了会话", w.Code)
	}
}

func TestDiskFailureRollsBackAndRetainsHigherRevisionInMemory(t *testing.T) {
	c, f := testController(t)
	blocker := filepath.Join(t.TempDir(), "blocked")
	if e := os.WriteFile(blocker, []byte("not a directory"), 0600); e != nil {
		t.Fatal(e)
	}
	c.cfg.StateFile = filepath.Join(blocker, "state.json")
	e := c.commitMappings(context.Background(), []model.Mapping{testMapping("mapping", "a", "b", "8080")}, "创建", "test")
	if e == nil || !strings.Contains(e.Error(), "保存回退状态失败") {
		t.Fatal("未反馈双重磁盘错误", e)
	}
	if len(c.snapshot().Mappings) != 0 || len(f.states["a"].Rules) != 0 {
		t.Fatal("未回退原映射")
	}
	if c.snapshot().Revision != f.states["a"].Revision {
		t.Fatal("内存未保留回退修订号")
	}
}

func TestProductionRoutesWithEmbeddedAssets(t *testing.T) {
	c, _ := testController(t)
	assets, e := fs.Sub(webui.FS, "assets")
	if e != nil {
		t.Fatal(e)
	}
	h := c.Handler(assets)
	for _, path := range []string{"/", "/healthz", "/api/session"} {
		w := request(t, h, "GET", path, "", nil, "", "")
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	page := request(t, h, "GET", "/", "", nil, "", "")
	// 读取真实入口引用，覆盖哈希资源及嵌套目录，避免只验证旧文件名。
	refs := regexp.MustCompile(`(?:src|href)="(/assets/[^"#]+)"`).FindAllStringSubmatch(page.Body.String(), -1)
	var hasScript, hasStyle bool
	for _, ref := range refs {
		path := ref[1]
		w := request(t, h, "GET", path, "", nil, "", "")
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("入口引用的静态资源不可用 %s: %d", path, w.Code)
		}
		if strings.HasSuffix(path, ".js") {
			hasScript = true
			if !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
				t.Fatalf("脚本类型错误 %s: %s", path, w.Header().Get("Content-Type"))
			}
		}
		if strings.HasSuffix(path, ".css") {
			hasStyle = true
			if !strings.Contains(w.Header().Get("Content-Type"), "text/css") {
				t.Fatalf("样式类型错误 %s: %s", path, w.Header().Get("Content-Type"))
			}
		}
	}
	if !hasScript || !hasStyle {
		t.Fatal("入口缺少可用的脚本或样式")
	}
	if !strings.Contains(page.Header().Get("Content-Security-Policy"), "script-src 'self';") {
		t.Fatal("静态界面必须拒绝内联脚本与外部脚本")
	}
	for _, path := range []string{"/.env", "/controller.json", "/api/unknown"} {
		w := request(t, h, "GET", path, "", nil, "", "")
		if w.Code != 404 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
}

func TestForwardedClientIPOnlyTrustedFromLocalProxy(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/login", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "192.0.2.9")
	if got := clientIP(r); got != "192.0.2.9" {
		t.Fatal(got)
	}
	r.RemoteAddr = "192.0.2.8:1234"
	if got := clientIP(r); got != "192.0.2.8" {
		t.Fatal("信任了外部伪造头", got)
	}
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "192.0.2.9, evil")
	if got := clientIP(r); got != "127.0.0.1" {
		t.Fatal("接受了非法地址链", got)
	}
}
