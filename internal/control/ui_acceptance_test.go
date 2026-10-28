package control

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"netshare/internal/model"
	"netshare/internal/webui"
)

// 只使用内存代理，不接触真实网卡、防火墙、SSH 或生产 API。
type uiAcceptanceAgents struct{ *fakeAgents }

func (a *uiAcceptanceAgents) Status(ctx context.Context, node model.Node) (model.AgentStatus, error) {
	status, err := a.fakeAgents.Status(ctx, node)
	status.Version = model.Version
	status.Listeners = []model.Listener{}
	status.Interfaces = []model.InterfaceStats{}
	status.Warnings = []string{}
	return status, err
}

func (a *uiAcceptanceAgents) Probe(_ context.Context, _ model.Node, _ model.ProbeRequest) (model.ProbeResult, error) {
	return model.ProbeResult{Reachable: false, Message: "本机隔离验收：未连接真实目标服务。", Interface: "eth8"}, nil
}

// 显式开启后持续提供真实控制器与嵌入式前端，供浏览器验收使用。
// 普通 go test 会跳过；配置、状态和随机密码均为临时测试数据。
func TestUIAcceptanceServer(t *testing.T) {
	if os.Getenv("NETSHARE_UI_ACCEPTANCE") != "1" {
		t.Skip("仅供显式开启的本机隔离 UI 验收")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:19111")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	c, agents := testController(t)
	c.agents = &uiAcceptanceAgents{fakeAgents: agents}
	c.cfg.Listen = "127.0.0.1:19111"
	c.cfg.ExternalURL = "http://127.0.0.1:19111"
	c.cfg.CookieSecure = false
	if os.Getenv("NETSHARE_UI_ACCEPTANCE_DEMO") == "1" {
		// 博客配图仅用文档地址与内存代理，不能当作真实网络实测。
		c.cfg.Nodes[0].Name = "演示节点 A"
		c.cfg.Nodes[1].Name = "演示节点 B"
		c.cfg.Nodes[0].ReservedPorts = []int{22, 80, 443, 31880}
		c.cfg.Nodes[1].ReservedPorts = []int{22, 31880}
		now := time.Now().UTC()
		mappings := []model.Mapping{
			{ID: "demo-web-entry", Name: "演示：Web 备用入口", SourceNode: "a", TargetNode: "b", Protocol: "both", ListenPort: "18080", TargetPort: "8080", Enabled: true, CreatedAt: now, UpdatedAt: now},
			{ID: "demo-monitor-entry", Name: "演示：监控面板", SourceNode: "b", TargetNode: "a", Protocol: "tcp", ListenPort: "19090", TargetPort: "9090", Enabled: true, CreatedAt: now, UpdatedAt: now},
		}
		if err = c.commitMappings(context.Background(), mappings, "演示初始化", "仅供本机界面演示，不连接真实网络"); err != nil {
			t.Fatal(err)
		}
	}
	if os.Getenv("NETSHARE_UI_ACCEPTANCE_RANDOM_PATH") == "1" {
		c.cfg.AccessPath, err = model.NewAccessPath()
		if err != nil {
			t.Fatal(err)
		}
	}
	password := randomID() + randomID()[:16]
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	c.cfg.PasswordHash = string(hash)
	c.configPath = filepath.Join(t.TempDir(), "controller.json")
	if err = atomicJSON(c.configPath, c.cfg); err != nil {
		t.Fatal(err)
	}
	passwordFile := os.Getenv("NETSHARE_UI_ACCEPTANCE_PASSWORD_FILE")
	if passwordFile == "" {
		cwd, cwdErr := os.Getwd()
		if cwdErr != nil {
			t.Fatal(cwdErr)
		}
		passwordFile = filepath.Clean(filepath.Join(cwd, "../../../../work/netshare-qa-password.txt"))
	}
	if err = os.MkdirAll(filepath.Dir(passwordFile), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(passwordFile, []byte(password+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(passwordFile)
	if err = os.Chmod(passwordFile, 0600); err != nil {
		t.Fatal(err)
	}
	entryFile := filepath.Join(filepath.Dir(passwordFile), "netshare-qa-entry.txt")
	if err = os.WriteFile(entryFile, []byte(c.cfg.ExternalURL+c.cfg.AccessPath+"/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(entryFile)
	assets, err := fs.Sub(webui.FS, "assets")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{
		Handler:           c.Handler(assets),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	t.Log("本机隔离 UI 验收已就绪：http://127.0.0.1:19111；用户名 admin，随机密码仅保存在受限文件。")
	if err = server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
}
