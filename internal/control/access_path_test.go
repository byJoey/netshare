package control

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"netshare/internal/model"
	"netshare/internal/webui"
)

func TestAccessPathProtectsEveryRouteAndScopesSession(t *testing.T) {
	c, _ := testController(t)
	c.cfg.AccessPath = "/ns-0123456789abcdef0123456789abcdef"
	c.cfg.CookieSecure = true
	assets, err := fs.Sub(webui.FS, "assets")
	if err != nil {
		t.Fatal(err)
	}
	h := c.Handler(assets)
	prefix := c.cfg.AccessPath
	for _, route := range []string{"/", "/api/session", "/api/overview", "/healthz", "/assets/index.html", prefix + "-similar/", prefix + "//", prefix + "/../api/session", prefix + "/assets/../../api/session", prefix + "/%61pi/session", prefix + "%2fapi/session", "/%6es-0123456789abcdef0123456789abcdef/"} {
		w := request(t, h, "GET", route, "", nil, "", "")
		if w.Code != 404 || w.Header().Get("Location") != "" || strings.Contains(w.Body.String(), prefix) {
			t.Fatalf("隐藏路径未拒绝或泄漏入口：%s HTTP %d", route, w.Code)
		}
	}
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		if w := request(t, h, method, "/api/login", `{}`, nil, "", ""); w.Code != 404 {
			t.Fatalf("旧入口仍接收 %s 请求", method)
		}
	}
	redirect := request(t, h, "GET", prefix, "", nil, "", "")
	if redirect.Code != http.StatusPermanentRedirect || redirect.Header().Get("Location") != prefix+"/" {
		t.Fatal("已知入口应规范化尾斜线")
	}
	page := request(t, h, "GET", prefix+"/", "", nil, "", "")
	if page.Code != 200 || !strings.Contains(page.Body.String(), `<meta name="netshare-base-path" content="`+prefix+`">`) {
		t.Fatal("页面未提供正确前缀")
	}
	refs := regexp.MustCompile(`(?:src|href)="(`+regexp.QuoteMeta(prefix)+`/assets/[^"#]+)"`).FindAllStringSubmatch(page.Body.String(), -1)
	if len(refs) < 2 || strings.Contains(page.Body.String(), `src="./static/`) || strings.Contains(page.Body.String(), `href="/assets/`) {
		t.Fatal("前缀页面静态资源引用不完整")
	}
	for _, ref := range refs {
		if w := request(t, h, "GET", ref[1], "", nil, "", ""); w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("前缀资源不可读：%s", ref[1])
		}
	}
	if w := request(t, h, "GET", prefix+"/healthz", "", nil, "", ""); w.Code != 200 {
		t.Fatal("前缀健康检查不可用")
	}
	w := request(t, h, "POST", prefix+"/api/login", `{"username":"admin","password":"testing-password-123"}`, nil, "", "https://example.test")
	if w.Code != 200 || len(w.Result().Cookies()) != 1 {
		t.Fatal("前缀登录失败")
	}
	cookie := w.Result().Cookies()[0]
	if cookie.Path != prefix+"/" || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("前缀会话作用域或安全属性无效")
	}
	var session map[string]any
	if err = json.Unmarshal(w.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	csrf := session["csrf_token"].(string)
	if w = request(t, h, "GET", prefix+"/api/overview", "", cookie, "", ""); w.Code != 200 {
		t.Fatal("前缀会话无法访问概览")
	}
	if w = request(t, h, "POST", prefix+"/api/reconcile", "", cookie, "", ""); w.Code != 403 {
		t.Fatal("前缀下缺少 CSRF 仍应拒绝")
	}
	if w = request(t, h, "POST", prefix+"/api/reconcile", "", cookie, csrf, "https://evil.test"); w.Code != 403 {
		t.Fatal("前缀下仍应拒绝跨站写请求")
	}
	if w = request(t, h, "POST", prefix+"/api/logout", "", cookie, csrf, "https://example.test"); w.Code != 200 || w.Result().Cookies()[0].Path != prefix+"/" {
		t.Fatal("退出登录未清理相同作用域会话")
	}
}

func testAccessConfig(t *testing.T) (Config, string) {
	t.Helper()
	c, _ := testController(t)
	cfg := c.cfg
	for i := range cfg.Nodes {
		cfg.Nodes[i].AgentURL = "https://" + cfg.Nodes[i].PrivateIP + ":31880"
	}
	path := filepath.Join(t.TempDir(), "controller.json")
	if err := atomicJSON(path, cfg); err != nil {
		t.Fatal(err)
	}
	return cfg, path
}

func TestAccessPathConfigurationAndRotationPreserveConfig(t *testing.T) {
	cfg, path := testAccessConfig(t)
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err = AccessPathCommand([]string{"show", "--config", path}, &output); err != nil || output.String() != cfg.ExternalURL+"/\n" {
		t.Fatal("旧配置入口显示失败", err)
	}
	output.Reset()
	if err = AccessPathCommand([]string{"rotate", "--config", path}, &output); err != nil {
		t.Fatal(err)
	}
	rotated, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.AccessPath == "" || model.ValidateAccessPath(rotated.AccessPath) != nil || output.String() != cfg.ExternalURL+rotated.AccessPath+"/\n" {
		t.Fatal("入口更换结果无效")
	}
	previousPath := rotated.AccessPath
	rotated.AccessPath = ""
	if !reflect.DeepEqual(cfg, rotated) {
		t.Fatal("更换入口误改了其他配置")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Fatal("更换入口未保留文件权限")
	}
	a, b := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
	if a.Uid != b.Uid || a.Gid != b.Gid {
		t.Fatal("更换入口未保留所有者")
	}
	output.Reset()
	if err = AccessPathCommand([]string{"rotate", "--config", path}, &output); err != nil {
		t.Fatal(err)
	}
	again, err := LoadConfig(path)
	if err != nil || again.AccessPath == previousPath {
		t.Fatal("更换入口必须生成新随机值")
	}
	link := filepath.Join(t.TempDir(), "controller-link.json")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err = AccessPathCommand([]string{"rotate", "--config", link}, &output); err == nil {
		t.Fatal("入口更换应拒绝替换符号链接")
	}
}

func TestLoadConfigRejectsInvalidAccessPathAndOrigin(t *testing.T) {
	cfg, path := testAccessConfig(t)
	for _, value := range []string{"/", "/admin", "/api", "/abcdefghijklmnop/", "/abcdefghijklmnop/child", "/abcdefghijklmnop%2f", "/abcdefghijklmnop.", "/abcdefghijklmnop?x", " /abcdefghijklmnop", "/_abcdefghijklmnop", "/" + strings.Repeat("a", 97)} {
		cfg.AccessPath = value
		if err := atomicJSON(path, cfg); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Fatalf("接受了非法入口 %q", value)
		}
	}
	cfg.AccessPath = "/abcdefghijklmnop"
	for _, origin := range []string{"https://example.test?secret=1", "https://example.test#fragment", "https://example.test/path", "https://user@example.test"} {
		cfg.ExternalURL = origin
		if err := atomicJSON(path, cfg); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Fatal("接受了包含路径或凭据的 origin")
		}
	}
}

func TestRandomAccessPathsAreIndependentAndValid(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		value, err := model.NewAccessPath()
		if err != nil || model.ValidateAccessPath(value) != nil || len(value) != 36 || !strings.HasPrefix(value, "/ns-") {
			t.Fatal("随机入口生成失败")
		}
		if seen[value] {
			t.Fatal("连续生成的随机入口重复")
		}
		seen[value] = true
	}
}
