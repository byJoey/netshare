package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
	"netshare/internal/enrollment"
	"netshare/internal/model"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func apiError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}
func decodeJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		apiError(w, 415, "请求必须使用 JSON")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(out); e != nil {
		apiError(w, 400, "请求内容无效或超过大小限制")
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		apiError(w, 400, "只能提交一个 JSON 对象")
		return false
	}
	return true
}

func (c *Controller) Handler(assets fs.FS) http.Handler {
	m := http.NewServeMux()
	c.registrationRoutes(m)
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "version": model.Version})
	})
	m.HandleFunc("POST /api/enroll/{token}", func(w http.ResponseWriter, r *http.Request) {
		enrollment.Serve(enrollment.Directory(c.cfg.StateFile), w, r, r.PathValue("token"))
	})
	m.HandleFunc("GET /api/session", c.handleSession)
	m.HandleFunc("POST /api/login", c.handleLogin)
	m.HandleFunc("POST /api/logout", c.protected(c.handleLogout, true))
	m.HandleFunc("POST /api/password", c.protected(c.handlePassword, true))
	m.HandleFunc("GET /api/overview", c.protected(c.handleOverview, false))
	m.HandleFunc("GET /api/export", c.protected(c.handleExport, false))
	m.HandleFunc("POST /api/mappings", c.protected(c.handleCreate, true))
	m.HandleFunc("PUT /api/mappings/{id}", c.protected(c.handleUpdate, true))
	m.HandleFunc("DELETE /api/mappings/{id}", c.protected(c.handleDelete, true))
	m.HandleFunc("POST /api/mappings/{id}/probe", c.protected(c.handleProbe, true))
	m.HandleFunc("POST /api/reconcile", c.protected(func(w http.ResponseWriter, r *http.Request) {
		if e := c.Reconcile(r.Context()); e != nil {
			apiError(w, 502, e.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}, true))
	m.HandleFunc("GET /api/", func(w http.ResponseWriter, r *http.Request) { apiError(w, 404, "接口不存在") })
	if assets != nil {
		files := http.StripPrefix("/assets/", http.FileServer(http.FS(assets)))
		m.HandleFunc("GET /assets", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
		m.HandleFunc("GET /assets/", func(w http.ResponseWriter, r *http.Request) {
			name := strings.TrimPrefix(r.URL.Path, "/assets/")
			info, err := fs.Stat(assets, name)
			if err != nil || !info.Mode().IsRegular() || name == "index.html" {
				http.NotFound(w, r)
				return
			}
			files.ServeHTTP(w, r)
		})
		m.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			b, e := fs.ReadFile(assets, "index.html")
			if e != nil {
				http.Error(w, "页面加载失败", 500)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(renderIndex(b, c.cfg.AccessPath))
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		// 弹出层的位置和滚动锁由组件计算样式；脚本仍只允许同源外部文件。
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("Cache-Control", "no-store")
		defer func() {
			if e := recover(); e != nil {
				slog.Error("请求处理异常", "error", e)
				apiError(w, 500, "服务处理失败，请稍后重试")
			}
		}()
		prefix := c.cfg.AccessPath
		if prefix != "" {
			// 在 ServeMux 清理路径之前拒绝编码或穿越，避免重定向和前缀混淆。
			cleanPath := path.Clean(r.URL.Path)
			if r.URL.EscapedPath() != r.URL.Path || cleanPath != r.URL.Path && cleanPath+"/" != r.URL.Path || strings.Contains(r.URL.Path, "\\") {
				http.NotFound(w, r)
				return
			}
			if r.URL.Path == prefix && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
				http.Redirect(w, r, prefix+"/", http.StatusPermanentRedirect)
				return
			}
			if !strings.HasPrefix(r.URL.Path, prefix+"/") {
				http.NotFound(w, r)
				return
			}
			r = r.Clone(r.Context())
			r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
			r.URL.RawPath = ""
		}
		m.ServeHTTP(w, r)
	})
}

func renderIndex(source []byte, prefix string) []byte {
	page := string(source)
	// 资源来自随二进制构建的可信 HTML，前缀已在配置加载时严格验证。
	page = strings.ReplaceAll(page, `src="./static/`, `src="`+prefix+`/assets/static/`)
	page = strings.ReplaceAll(page, `href="./static/`, `href="`+prefix+`/assets/static/`)
	page = strings.ReplaceAll(page, `href="./favicon.svg"`, `href="`+prefix+`/assets/favicon.svg"`)
	// 同时兼容升级前的构建产物，避免前端与后端构建顺序影响可用性。
	page = strings.ReplaceAll(page, `src="/assets/`, `src="`+prefix+`/assets/`)
	page = strings.ReplaceAll(page, `href="/assets/`, `href="`+prefix+`/assets/`)
	page = strings.Replace(page, "</head>", `<meta name="netshare-base-path" content="`+prefix+`"></head>`, 1)
	return []byte(page)
}

func (c *Controller) handleSession(w http.ResponseWriter, r *http.Request) {
	_, s, ok := c.auth.get(r)
	writeJSON(w, 200, map[string]any{"authenticated": ok, "username": s.Username, "csrf_token": s.CSRF, "version": model.Version})
}
func (c *Controller) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !c.validOrigin(r) {
		apiError(w, 403, "登录请求来源无效")
		return
	}
	if !c.auth.allowLogin(clientIP(r)) {
		w.Header().Set("Retry-After", "60")
		apiError(w, 429, "登录尝试过多，请稍后重试")
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if len(in.Password) > 1024 || len(in.Username) > 128 {
		apiError(w, 400, "用户名或密码过长")
		return
	}
	// 与改密串行，避免旧摘要验证完成后在会话撤销之后又创建新会话。
	c.mutation.Lock()
	defer c.mutation.Unlock()
	c.mu.RLock()
	hash := c.cfg.PasswordHash
	user := c.cfg.Username
	c.mu.RUnlock()
	passOK := bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) == nil
	if !passOK || in.Username != user {
		apiError(w, 401, "用户名或密码错误")
		return
	}
	id, s := c.auth.create(user)
	c.setCookie(w, id)
	state := c.snapshot()
	appendEvent(&state, "登录", "管理员已登录", true)
	if e := c.persist(state); e != nil {
		slog.Error("保存登录记录失败", "error", e)
	}
	writeJSON(w, 200, map[string]any{"authenticated": true, "username": s.Username, "csrf_token": s.CSRF, "version": model.Version})
}
func (c *Controller) handleLogout(w http.ResponseWriter, r *http.Request) {
	id, _, _ := c.auth.get(r)
	c.auth.mu.Lock()
	delete(c.auth.sessions, id)
	c.auth.mu.Unlock()
	c.clearCookie(w)
	writeJSON(w, 200, map[string]any{"ok": true})
}
func (c *Controller) handlePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if utf8.RuneCountInString(in.New) < 12 || len(in.New) > 72 {
		apiError(w, 400, "新密码至少 12 个字符，且不能超过 72 字节")
		return
	}
	c.mutation.Lock()
	defer c.mutation.Unlock()
	c.mu.RLock()
	cfg := c.cfg
	c.mu.RUnlock()
	if bcrypt.CompareHashAndPassword([]byte(cfg.PasswordHash), []byte(in.Current)) != nil {
		apiError(w, 403, "当前密码不正确")
		return
	}
	hash, e := bcrypt.GenerateFromPassword([]byte(in.New), 12)
	if e != nil {
		apiError(w, 500, "生成密码摘要失败")
		return
	}
	cfg.PasswordHash = string(hash)
	if c.configPath == "" {
		apiError(w, 500, "未设置配置文件路径")
		return
	}
	if e = atomicJSON(c.configPath, cfg); e != nil {
		apiError(w, 500, "保存新密码失败")
		return
	}
	c.mu.Lock()
	c.cfg.PasswordHash = cfg.PasswordHash
	c.mu.Unlock()
	c.auth.mu.Lock()
	c.auth.sessions = map[string]session{}
	c.auth.mu.Unlock()
	id, s := c.auth.create(cfg.Username)
	c.setCookie(w, id)
	state := c.snapshot()
	appendEvent(&state, "修改密码", "已修改管理员密码并撤销旧会话", true)
	if e = c.persist(state); e != nil {
		slog.Error("保存密码变更审计失败", "error", e)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "csrf_token": s.CSRF})
}
func (c *Controller) handleOverview(w http.ResponseWriter, r *http.Request) {
	s := c.snapshot()
	nodes := c.nodeStatuses(r.Context())
	writeJSON(w, 200, map[string]any{"nodes": nodes, "mappings": s.Mappings, "events": s.Events, "revision": s.Revision, "version": model.Version})
}
func (c *Controller) handleExport(w http.ResponseWriter, r *http.Request) {
	s := c.snapshot()
	w.Header().Set("Content-Disposition", `attachment; filename="netshare-mappings.json"`)
	writeJSON(w, 200, map[string]any{"version": model.Version, "exported_at": time.Now().UTC(), "mappings": s.Mappings})
}

type mappingInput struct {
	Name       string `json:"name"`
	SourceNode string `json:"source_node"`
	TargetNode string `json:"target_node"`
	Protocol   string `json:"protocol"`
	ListenPort string `json:"listen_port"`
	TargetPort string `json:"target_port"`
	Enabled    bool   `json:"enabled"`
}

func (in mappingInput) mapping(id string) model.Mapping {
	listen, target := strings.TrimSpace(in.ListenPort), strings.TrimSpace(in.TargetPort)
	if p, e := model.ParsePorts(listen); e == nil {
		listen = p.String()
	}
	if p, e := model.ParsePorts(target); e == nil {
		target = p.String()
	}
	return model.Mapping{ID: id, Name: strings.TrimSpace(in.Name), SourceNode: in.SourceNode, TargetNode: in.TargetNode, Protocol: in.Protocol, ListenPort: listen, TargetPort: target, Enabled: in.Enabled}
}
func (c *Controller) handleCreate(w http.ResponseWriter, r *http.Request) {
	var in mappingInput
	if !decodeJSON(w, r, &in) {
		return
	}
	c.mutation.Lock()
	defer c.mutation.Unlock()
	s := c.snapshot()
	m := in.mapping(randomID())
	m.CreatedAt = time.Now().UTC()
	m.UpdatedAt = m.CreatedAt
	s.Mappings = append(s.Mappings, m)
	if e := c.commitMappings(r.Context(), s.Mappings, "创建映射", m.Name); e != nil {
		apiError(w, 409, e.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"ok": true, "mapping": m})
}
func (c *Controller) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var in mappingInput
	if !decodeJSON(w, r, &in) {
		return
	}
	c.mutation.Lock()
	defer c.mutation.Unlock()
	s := c.snapshot()
	for i, o := range s.Mappings {
		if o.ID == r.PathValue("id") {
			m := in.mapping(o.ID)
			m.CreatedAt = o.CreatedAt
			m.UpdatedAt = time.Now().UTC()
			s.Mappings[i] = m
			if e := c.commitMappings(r.Context(), s.Mappings, "更新映射", m.Name); e != nil {
				apiError(w, 409, e.Error())
				return
			}
			writeJSON(w, 200, map[string]any{"ok": true, "mapping": m})
			return
		}
	}
	apiError(w, 404, "映射不存在")
}
func (c *Controller) handleDelete(w http.ResponseWriter, r *http.Request) {
	c.mutation.Lock()
	defer c.mutation.Unlock()
	s := c.snapshot()
	for i, o := range s.Mappings {
		if o.ID == r.PathValue("id") {
			s.Mappings = append(s.Mappings[:i], s.Mappings[i+1:]...)
			if e := c.commitMappings(r.Context(), s.Mappings, "删除映射", o.Name); e != nil {
				apiError(w, 409, e.Error())
				return
			}
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
	}
	apiError(w, 404, "映射不存在")
}
func (c *Controller) handleProbe(w http.ResponseWriter, r *http.Request) {
	s := c.snapshot()
	for _, m := range s.Mappings {
		if m.ID == r.PathValue("id") {
			n, _ := c.node(m.SourceNode)
			target, _ := c.node(m.TargetNode)
			p, _ := model.ParsePorts(m.TargetPort)
			protocol := "tcp"
			if m.Protocol == "udp" {
				protocol = "udp"
			}
			result, e := c.agents.Probe(r.Context(), n, model.ProbeRequest{TargetIP: target.PrivateIP, TargetPort: fmt.Sprint(p.Start), Protocol: protocol})
			if e != nil {
				apiError(w, 502, e.Error())
				return
			}
			writeJSON(w, 200, result)
			return
		}
	}
	apiError(w, 404, "映射不存在")
}

func Run(ctx context.Context, path string, assets fs.FS) error {
	cfg, e := LoadConfig(path)
	if e != nil {
		return e
	}
	c, e := New(cfg, nil)
	if e != nil {
		return e
	}
	c.configPath = path
	go c.maintainTopology(ctx)
	srv := &http.Server{Addr: cfg.Listen, Handler: c.Handler(assets), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 120 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		child, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		if e := c.Reconcile(child); e != nil {
			slog.Warn("启动同步未完成，可在界面重试", "error", e)
		}
	}()
	go func() {
		<-ctx.Done()
		child, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(child)
	}()
	slog.Info("控制台已启动", "listen", cfg.Listen, "url", cfg.ExternalURL)
	e = srv.ListenAndServe()
	if errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}
