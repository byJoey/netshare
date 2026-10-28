package bootstrap

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"netshare/internal/control"
)

func TestStandaloneDirectHTTPInitializesAndLogsIn(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "direct-http")
	opt := options{dir: dir, standalone: true, externalURL: "http://192.0.2.10:18440", listen: "0.0.0.0:18440", agentPort: defaultAgentPort}
	if err := initialize(opt); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "controller/controller.json")
	cfg, err := control.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != opt.listen || cfg.ExternalURL != opt.externalURL || cfg.CookieSecure || len(cfg.Nodes) != 0 || cfg.AccessPath == "" {
		t.Fatal("IP 直连初始化结果不正确")
	}
	cfg.CAFile = filepath.Join(dir, "controller/ca.pem")
	cfg.CertFile = filepath.Join(dir, "controller/controller.pem")
	cfg.KeyFile = filepath.Join(dir, "controller/controller-key.pem")
	cfg.StateFile = filepath.Join(dir, "state.json")
	c, err := control.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	password, err := os.ReadFile(filepath.Join(dir, "controller/admin-password.txt"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": strings.TrimSpace(string(password))})
	r := httptest.NewRequest(http.MethodPost, cfg.ExternalURL+cfg.AccessPath+"/api/login", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", cfg.ExternalURL)
	w := httptest.NewRecorder()
	c.Handler(nil).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP 登录失败：%d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != cfg.AccessPath+"/" {
		t.Fatal("HTTP 登录 Cookie 不可用或保护属性丢失")
	}
	// 未授权跨站请求仍然拒绝，允许 HTTP 不得跳过登录/Origin 校验。
	r = httptest.NewRequest(http.MethodPost, cfg.ExternalURL+cfg.AccessPath+"/api/login", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://evil.example")
	w = httptest.NewRecorder()
	c.Handler(nil).ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal("直接 HTTP 模式不得接受跨站登录")
	}
}

func TestDirectHTTPRejectsAmbiguousOrUnsafeInputs(t *testing.T) {
	cases := []struct {
		name   string
		change func(*options)
	}{
		{"隐式旧模式", func(o *options) { o.standalone = false; o.nodesFile = writeNodes(t, testNodes()) }},
		{"同时域名", func(o *options) { o.domain = "netshare.example.test" }},
		{"错误协议", func(o *options) { o.externalURL = "https://192.0.2.10:18440" }},
		{"缺少端口", func(o *options) { o.externalURL = "http://192.0.2.10" }},
		{"端口不一致", func(o *options) { o.externalURL = "http://192.0.2.10:18441" }},
		{"非IP地址", func(o *options) { o.externalURL = "http://netshare.example.test:18440" }},
		{"不可访问IP", func(o *options) { o.externalURL = "http://0.0.0.0:18440" }},
		{"地址带路径", func(o *options) { o.externalURL = "http://192.0.2.10:18440/a" }},
		{"地址带凭据", func(o *options) { o.externalURL = "http://user:password@192.0.2.10:18440" }},
		{"IPv6监听", func(o *options) { o.listen = "[::]:18440" }},
		{"主机名监听", func(o *options) { o.listen = "localhost:18440" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "absent")
			o := options{dir: dir, standalone: true, externalURL: "http://192.0.2.10:18440", listen: "0.0.0.0:18440", agentPort: defaultAgentPort}
			tc.change(&o)
			if err := initialize(o); err == nil {
				t.Fatal("应拒绝无效直连配置")
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("无效输入不应留下凭据")
			}
		})
	}
}
