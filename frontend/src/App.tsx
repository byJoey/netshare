import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type FormEvent,
} from "react";
import {
  ArrowLeftRight,
  Check,
  ChevronRight,
  ChevronsUpDown,
  CircleHelp,
  Eye,
  EyeOff,
  History,
  KeyRound,
  Loader2,
  LogOut,
  Menu as MenuIcon,
  Monitor,
  Moon,
  Network,
  PanelLeftClose,
  PanelLeftOpen,
  RefreshCw,
  Server,
  Settings2,
  ShieldCheck,
  Sun,
  X,
} from "lucide-react";
import type {
  Mapping,
  MappingInput,
  Overview,
  ProbeResult,
  Session,
  View,
} from "./lib/types";
import { api, errorMessage, setApiSession } from "./lib/api";
import { cn, formatTime, mappingInput } from "./lib/utils";
import {
  Alert,
  Badge,
  Button,
  ConfirmDialog,
  Menu,
  MenuContent,
  MenuItem,
  MenuLabel,
  MenuSeparator,
  MenuTrigger,
  Modal,
  Switch,
  type ConfirmState,
} from "./components/ui";
import { MappingEditor } from "./components/mapping-editor";
import { MappingsPage } from "./pages/mappings";
import { NodesPage } from "./pages/nodes";
import { EventsPage } from "./pages/events";

const viewNames: Record<View, string> = {
  mappings: "端口映射",
  nodes: "机器状态",
  events: "操作记录",
  settings: "设置",
};
const viewDescriptions: Record<View, string> = {
  mappings: "",
  nodes: "查看私网连接、代理状态和机器的真实网络信息。",
  events: "查看每次变更与执行结果，失败也有记录。",
  settings: "管理账号安全和工作台偏好。",
};
const navigation = [
  { id: "mappings" as const, icon: ArrowLeftRight },
  { id: "nodes" as const, icon: Server },
  { id: "events" as const, icon: History },
];
const emptyDraft: MappingInput = {
  name: "",
  source_node: "",
  target_node: "",
  protocol: "tcp",
  listen_port: "",
  target_port: "",
  enabled: true,
};
const getView = (): View =>
  ["mappings", "nodes", "events", "settings"].includes(location.hash.slice(1))
    ? (location.hash.slice(1) as View)
    : "mappings";
function readPreference(key: string, fallback: string) {
  try {
    return localStorage.getItem(`netshare:${key}`) || fallback;
  } catch {
    return fallback;
  }
}
function writePreference(key: string, value: string) {
  try {
    localStorage.setItem(`netshare:${key}`, value);
  } catch {
    /* 浏览器禁用存储时仍可使用当前设置。 */
  }
}

export function App() {
  const [session, setSession] = useState<Session | null>(null),
    sessionRef = useRef<Session | null>(null);
  const [booting, setBooting] = useState(true),
    [bootError, setBootError] = useState(""),
    [loginError, setLoginError] = useState("");
  const [overview, setOverview] = useState<Overview | null>(null),
    [pageError, setPageError] = useState("");
  const [view, setView] = useState<View>(getView),
    [collapsed, setCollapsed] = useState(
      () => readPreference("sidebar", "open") === "closed",
    ),
    [mobileNav, setMobileNav] = useState(false);
  const [theme, setTheme] = useState(() => readPreference("theme", "system")),
    [autoRefresh, setAutoRefresh] = useState(
      () => readPreference("auto-refresh", "true") !== "false",
    );
  const [refreshing, setRefreshing] = useState(false),
    [updatedAt, setUpdatedAt] = useState<Date | null>(null);
  const refreshEpoch = useRef(0),
    refreshPending = useRef<Promise<void> | null>(null);
  const [busy, setBusy] = useState(false),
    busyRef = useRef(false);
  const [editor, setEditor] = useState<{
      mapping: Mapping | null;
      initial: MappingInput;
    } | null>(null),
    [formError, setFormError] = useState(""),
    dirtyRef = useRef(false);
  const [confirm, setConfirm] = useState<ConfirmState | null>(null),
    [probes, setProbes] = useState<Record<string, ProbeResult>>({});
  const [passwordOpen, setPasswordOpen] = useState(false),
    [passwordBusy, setPasswordBusy] = useState(false);
  const [toast, setToast] = useState<{
      message: string;
      error: boolean;
    } | null>(null),
    toastTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const notify = useCallback((message: string, error = false) => {
    clearTimeout(toastTimer.current);
    setToast({ message, error });
    toastTimer.current = setTimeout(() => setToast(null), 6500);
  }, []);
  const authenticate = useCallback((value: Session | null) => {
    sessionRef.current = value;
    setApiSession(value);
    setSession(value);
  }, []);
  const resetProtected = useCallback(() => {
    refreshEpoch.current++;
    setOverview(null);
    setPageError("");
    setUpdatedAt(null);
    setEditor(null);
    dirtyRef.current = false;
    setConfirm(null);
    setPasswordOpen(false);
    setMobileNav(false);
    setProbes({});
    setToast(null);
  }, []);
  const refresh = useCallback(async (force = false) => {
    if (!sessionRef.current) return;
    if (refreshPending.current) {
      await refreshPending.current;
      if (!force || !sessionRef.current) return;
    }
    const epoch = refreshEpoch.current;
    setRefreshing(true);
    const task = (async () => {
      try {
        const data = await api<Overview>("/api/overview");
        if (!sessionRef.current || epoch !== refreshEpoch.current) return;
        setOverview({
          ...data,
          nodes: data.nodes || [],
          mappings: data.mappings || [],
          events: data.events || [],
        });
        setPageError("");
        setUpdatedAt(new Date());
      } catch (error) {
        if (sessionRef.current && epoch === refreshEpoch.current)
          setPageError(errorMessage(error));
      }
    })();
    refreshPending.current = task;
    try {
      await task;
    } finally {
      if (refreshPending.current === task) {
        refreshPending.current = null;
        setRefreshing(false);
      }
    }
  }, []);
  const boot = useCallback(async () => {
    setBooting(true);
    setBootError("");
    try {
      const value = await api<Session>("/api/session");
      if (value.authenticated) authenticate(value);
    } catch (error) {
      setBootError(errorMessage(error));
    } finally {
      setBooting(false);
    }
  }, [authenticate]);
  useEffect(() => {
    void boot();
  }, [boot]);
  useEffect(() => {
    if (session) void refresh(true);
  }, [session?.username, refresh]);
  useEffect(() => {
    const expired = () => {
      authenticate(null);
      resetProtected();
      setLoginError("登录已过期，请重新登录。");
    };
    window.addEventListener("netshare:expired", expired);
    return () => window.removeEventListener("netshare:expired", expired);
  }, [authenticate, resetProtected]);
  useEffect(() => {
    const media = matchMedia("(prefers-color-scheme: dark)");
    const apply = () => {
      document.documentElement.classList.toggle(
        "dark",
        theme === "dark" || (theme === "system" && media.matches),
      );
      document.documentElement.dataset.theme = theme;
    };
    apply();
    media.addEventListener("change", apply);
    writePreference("theme", theme);
    return () => media.removeEventListener("change", apply);
  }, [theme]);
  useEffect(() => {
    writePreference("sidebar", collapsed ? "closed" : "open");
  }, [collapsed]);
  useEffect(() => {
    writePreference("auto-refresh", String(autoRefresh));
  }, [autoRefresh]);
  useEffect(() => {
    if (!session || !autoRefresh) return;
    const update = () => {
      if (!document.hidden && !busyRef.current) void refresh();
    };
    const timer = setInterval(update, 15000);
    document.addEventListener("visibilitychange", update);
    return () => {
      clearInterval(timer);
      document.removeEventListener("visibilitychange", update);
    };
  }, [session?.username, autoRefresh, refresh]);
  useEffect(() => {
    const handleHash = () => {
      setView(getView());
      setMobileNav(false);
    };
    const beforeUnload = (event: BeforeUnloadEvent) => {
      if (dirtyRef.current || busyRef.current) {
        event.preventDefault();
        event.returnValue = "";
      }
    };
    const shortcut = (event: KeyboardEvent) => {
      if (
        event.key === "/" &&
        sessionRef.current &&
        getView() === "mappings" &&
        !["INPUT", "TEXTAREA", "SELECT"].includes(
          (event.target as HTMLElement)?.tagName,
        ) &&
        !event.metaKey &&
        !event.ctrlKey &&
        !document.querySelector('[role="dialog"]')
      ) {
        event.preventDefault();
        document.getElementById("mapping-search")?.focus();
      }
    };
    window.addEventListener("hashchange", handleHash);
    window.addEventListener("beforeunload", beforeUnload);
    document.addEventListener("keydown", shortcut);
    return () => {
      window.removeEventListener("hashchange", handleHash);
      window.removeEventListener("beforeunload", beforeUnload);
      document.removeEventListener("keydown", shortcut);
      clearTimeout(toastTimer.current);
    };
  }, []);
  const updateDirty = useCallback((value: boolean) => {
    dirtyRef.current = value;
  }, []);
  function afterDiscard(action: () => void) {
    if (busyRef.current) return;
    if (!dirtyRef.current) {
      action();
      return;
    }
    setConfirm({
      title: "放弃未保存的修改？",
      description: "这次填写的内容还没有保存。关闭后需要重新填写。",
      label: "放弃修改",
      danger: false,
      onConfirm: () => {
        dirtyRef.current = false;
        setConfirm(null);
        action();
      },
    });
  }
  function closeEditor() {
    afterDiscard(() => {
      setEditor(null);
      setFormError("");
    });
  }
  function openEditor(mapping: Mapping | null = null, preset?: MappingInput) {
    afterDiscard(() => {
      setFormError("");
      dirtyRef.current = false;
      setEditor({
        mapping,
        initial:
          preset ||
          (mapping
            ? mappingInput(mapping)
            : {
                ...emptyDraft,
                source_node: overview?.nodes[0]?.id || "",
                target_node: overview?.nodes[1]?.id || "",
              }),
      });
    });
  }
  function reverseDraft(draft: MappingInput) {
    openEditor(null, {
      ...draft,
      name: `${draft.name} · 反向`.slice(0, 80),
      source_node: draft.target_node,
      target_node: draft.source_node,
      listen_port: draft.target_port,
      target_port: draft.listen_port,
      enabled: false,
    });
  }
  async function mutate(action: () => Promise<void>, errorInForm = false) {
    if (busyRef.current || !sessionRef.current) return;
    busyRef.current = true;
    setBusy(true);
    setFormError("");
    refreshEpoch.current++;
    try {
      await action();
    } catch (error) {
      if (sessionRef.current) {
        if (errorInForm) setFormError(errorMessage(error));
        else notify(errorMessage(error), true);
      }
    } finally {
      // 等到新概览读取完成，再允许下一次操作，避免带入刚保存前的旧值。
      try {
        await refresh(true);
      } finally {
        busyRef.current = false;
        setBusy(false);
      }
    }
  }
  function saveMapping(draft: MappingInput) {
    const id = editor?.mapping?.id;
    void mutate(async () => {
      await api(
        id ? `/api/mappings/${encodeURIComponent(id)}` : "/api/mappings",
        { method: id ? "PUT" : "POST", body: draft },
      );
      dirtyRef.current = false;
      setEditor(null);
      if (id)
        setProbes((previous) => {
          const next = { ...previous };
          delete next[id];
          return next;
        });
      notify(draft.enabled ? "映射已保存并应用。" : "映射已保存，暂未启用。");
    }, true);
  }
  function deleteMapping(mapping: Mapping) {
    setConfirm({
      title: "删除这条映射？",
      description: `「${mapping.name}」将从配置中移除。入口停止接收新连接，已有连接会自然结束；目标服务不受影响。`,
      label: "删除映射",
      danger: true,
      onConfirm: () => {
        setConfirm(null);
        void mutate(async () => {
          await api(`/api/mappings/${encodeURIComponent(mapping.id)}`, {
            method: "DELETE",
          });
          if (editor?.mapping?.id === mapping.id) {
            dirtyRef.current = false;
            setEditor(null);
          }
          notify("映射已删除。");
        }, !!editor);
      },
    });
  }
  function toggleMapping(mapping: Mapping) {
    const action = () => {
      setConfirm(null);
      void mutate(async () => {
        await api(`/api/mappings/${encodeURIComponent(mapping.id)}`, {
          method: "PUT",
          body: { ...mappingInput(mapping), enabled: !mapping.enabled },
        });
        notify(
          mapping.enabled ? "映射已停用，新连接将不再转发。" : "映射已启用。",
        );
      });
    };
    if (mapping.enabled)
      setConfirm({
        title: "停用这条映射？",
        description: `「${mapping.name}」将停止接收新连接。已有连接会自然结束。`,
        label: "停用映射",
        onConfirm: action,
      });
    else action();
  }
  function probe(mapping: Mapping) {
    if (mapping.protocol === "udp") {
      notify("UDP 请用实际客户端验证。");
      return;
    }
    void mutate(async () => {
      const result = await api<ProbeResult>(
        `/api/mappings/${encodeURIComponent(mapping.id)}/probe`,
        { method: "POST", timeout: 15000 },
      );
      setProbes((previous) => ({ ...previous, [mapping.id]: result }));
      notify(
        result.reachable
          ? "目标私网 TCP 可达；公网入口请用实际客户端验证。"
          : result.message || "目标私网 TCP 暂不可达。",
        !result.reachable,
      );
    });
  }
  function reconcile() {
    void mutate(async () => {
      await api("/api/reconcile", { method: "POST" });
      notify("规则已重新下发到节点。");
    });
  }
  async function exportConfig() {
    try {
      const result = await api("/api/export");
      const url = URL.createObjectURL(
        new Blob([JSON.stringify(result, null, 2) + "\n"], {
          type: "application/json",
        }),
      );
      const link = document.createElement("a");
      link.href = url;
      link.download = `netshare-${new Date().toISOString().slice(0, 10)}.json`;
      link.click();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
      notify("配置已导出，不含登录凭据。");
    } catch (error) {
      notify(errorMessage(error), true);
    }
  }
  function logout() {
    afterDiscard(() => {
      void mutate(async () => {
        await api("/api/logout", { method: "POST" });
        authenticate(null);
        resetProtected();
        setLoginError("");
      });
    });
  }
  function navigate(next: View) {
    afterDiscard(() => {
      setEditor(null);
      setFormError("");
      dirtyRef.current = false;
      location.hash = next;
      setView(next);
      setMobileNav(false);
    });
  }
  const themeMenu = (
    <Menu>
      <MenuTrigger asChild>
        <Button variant="ghost" size="icon" aria-label="切换主题">
          {theme === "dark" ? (
            <Moon size={18} />
          ) : theme === "light" ? (
            <Sun size={18} />
          ) : (
            <Monitor size={18} />
          )}
        </Button>
      </MenuTrigger>
      <MenuContent>
        <MenuLabel>界面主题</MenuLabel>
        <MenuItem onSelect={() => setTheme("light")}>
          <Sun />
          浅色{theme === "light" && <Check className="menu-check" />}
        </MenuItem>
        <MenuItem onSelect={() => setTheme("dark")}>
          <Moon />
          深色{theme === "dark" && <Check className="menu-check" />}
        </MenuItem>
        <MenuItem onSelect={() => setTheme("system")}>
          <Monitor />
          跟随系统{theme === "system" && <Check className="menu-check" />}
        </MenuItem>
      </MenuContent>
    </Menu>
  );
  if (booting || bootError)
    return (
      <div className="boot-screen">
        <Brand />
        <div className="boot-box">
          {bootError ? (
            <>
              <Alert>{bootError}</Alert>
              <Button variant="outline" onClick={() => void boot()}>
                重新连接
              </Button>
            </>
          ) : (
            <>
              <div className="skeleton boot-skeleton" />
              <p className="muted">正在连接管理台…</p>
            </>
          )}
        </div>
      </div>
    );
  if (!session)
    return (
      <>
        <div className="login-theme">{themeMenu}</div>
        <LoginView
          error={loginError}
          onError={setLoginError}
          onLogin={(value) => {
            authenticate(value);
            setLoginError("");
          }}
        />
      </>
    );
  const nav = (
    <>
      <div className="sidebar-brand">
        <Brand collapsed={collapsed && !mobileNav} />
        <span className="workspace-chevron">
          <ChevronsUpDown size={15} />
        </span>
      </div>
      <div className="sidebar-content">
        <p className="nav-group-label">工作空间</p>
        <nav aria-label="主导航">
          {navigation.map((item) => (
            <button
              key={item.id}
              type="button"
              title={viewNames[item.id]}
              data-view={item.id}
              className={cn("nav-item", view === item.id && "active")}
              onClick={() => navigate(item.id)}
            >
              <item.icon size={18} />
              <span className="nav-label">{viewNames[item.id]}</span>
              {item.id === "mappings" && (
                <span className="nav-count">
                  {overview?.mappings.length || 0}
                </span>
              )}
            </button>
          ))}
        </nav>
        <div className="sidebar-bottom-nav">
          <button
            data-view="settings"
            className={cn("nav-item", view === "settings" && "active")}
            onClick={() => navigate("settings")}
            title="设置"
          >
            <Settings2 size={18} />
            <span className="nav-label">设置</span>
          </button>
          <div className="sidebar-help">
            <ShieldCheck size={17} />
            <div>
              <strong>私网安全连接</strong>
              <p>节点间使用双向 TLS</p>
            </div>
          </div>
        </div>
      </div>
      <Menu>
        <MenuTrigger asChild>
          <button className="sidebar-user" aria-label="账号菜单">
            <span className="avatar">
              {session.username.slice(0, 1).toUpperCase()}
            </span>
            <span className="user-copy">
              <strong>{session.username}</strong>
              <span>管理员</span>
            </span>
            <ChevronsUpDown size={15} className="user-chevron" />
          </button>
        </MenuTrigger>
        <MenuContent>
          <MenuLabel>{session.username} · 管理员</MenuLabel>
          <MenuItem onSelect={() => setPasswordOpen(true)}>
            <KeyRound />
            修改密码
          </MenuItem>
          <MenuItem onSelect={() => navigate("settings")}>
            <Settings2 />
            设置
          </MenuItem>
          <MenuSeparator />
          <MenuItem onSelect={logout} disabled={busy || passwordBusy}>
            <LogOut />
            退出登录
          </MenuItem>
        </MenuContent>
      </Menu>
    </>
  );
  return (
    <div id="app" className={cn("app-shell", collapsed && "sidebar-collapsed")}>
      <a
        href="#main-content"
        onClick={(event) => {
          event.preventDefault();
          document.getElementById("main-content")?.focus();
        }}
        className="skip-link"
      >
        跳到主要内容
      </a>
      <aside className="sidebar">{nav}</aside>
      <Modal
        open={mobileNav}
        onOpenChange={setMobileNav}
        title="工作空间"
        description="NetShare 网络管理台"
        id="mobile-navigation"
        sheet
      >
        <div className="mobile-nav-content">{nav}</div>
      </Modal>
      <div className="main-shell">
        <header className="topbar">
          <div className="topbar-left">
            <Button
              variant="ghost"
              size="icon"
              className="desktop-toggle"
              aria-label={collapsed ? "展开侧栏" : "收起侧栏"}
              onClick={() => setCollapsed(!collapsed)}
            >
              {collapsed ? (
                <PanelLeftOpen size={18} />
              ) : (
                <PanelLeftClose size={18} />
              )}
            </Button>
            <Button
              variant="ghost"
              size="icon"
              className="mobile-toggle"
              aria-label="打开导航"
              onClick={() => setMobileNav(true)}
            >
              <MenuIcon size={19} />
            </Button>
            <span className="topbar-divider" />
            <div className="breadcrumb">
              <span>工作空间</span>
              <ChevronRight size={14} />
              <strong>{viewNames[view]}</strong>
            </div>
          </div>
          <div className="topbar-actions">
            <span className="update-label">
              {updatedAt ? `更新于 ${formatTime(updatedAt)}` : "尚未更新"}
            </span>
            <Button
              id="refresh"
              variant="ghost"
              size="icon"
              onClick={() => void refresh(true)}
              disabled={refreshing || busy}
              aria-label="刷新数据"
            >
              <RefreshCw
                size={17}
                className={refreshing ? "spinning" : undefined}
              />
            </Button>
            {themeMenu}
            <span
              className="avatar avatar-small"
              title={`${session.username} · 管理员`}
            >
              {session.username.slice(0, 1).toUpperCase()}
            </span>
          </div>
        </header>
        <main id="main-content" tabIndex={-1}>
          {pageError && (
            <div className="page-error">
              <Alert>
                {pageError}
                {overview && " 当前显示上次获取的数据。"}
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => void refresh(true)}
                >
                  重试
                </Button>
              </Alert>
            </div>
          )}
          {view !== "mappings" && (
            <div className="page-heading">
              <div>
                <h1>{viewNames[view]}</h1>
                <p>{viewDescriptions[view]}</p>
              </div>
            </div>
          )}
          {!overview ? (
            <div
              id="mapping-loading"
              className="loading-layout"
              aria-label="正在读取概览"
            >
              <div className="skeleton skeleton-title" />
              <div className="skeleton skeleton-network" />
              <div className="skeleton skeleton-toolbar" />
              <div className="skeleton skeleton-table" />
            </div>
          ) : view === "mappings" ? (
            <MappingsPage
              overview={overview}
              busy={busy}
              onCreate={() => openEditor()}
              onJoined={() => void refresh(true)}
              onEdit={(mapping) => openEditor(mapping)}
              onReverse={(mapping) => reverseDraft(mappingInput(mapping))}
              onDelete={deleteMapping}
              onToggle={toggleMapping}
              onProbe={probe}
              probes={probes}
              onExport={() => void exportConfig()}
              onReconcile={reconcile}
              notify={notify}
            />
          ) : view === "nodes" ? (
            <NodesPage overview={overview} onJoined={() => void refresh(true)} />
          ) : view === "events" ? (
            <EventsPage overview={overview} />
          ) : (
            <div className="settings-layout">
              <section className="settings-section">
                <div>
                  <h2>账号安全</h2>
                  <p>当前登录为 {session.username}。</p>
                </div>
                <div className="settings-row">
                  <div>
                    <strong>管理员密码</strong>
                    <p>修改后，其他设备需要重新登录。</p>
                  </div>
                  <Button
                    id="password-change"
                    variant="outline"
                    onClick={() => setPasswordOpen(true)}
                  >
                    <KeyRound size={16} />
                    修改密码
                  </Button>
                </div>
              </section>
              <section className="settings-section">
                <div>
                  <h2>工作台偏好</h2>
                  <p>仅保存在当前浏览器。</p>
                </div>
                <div className="settings-row">
                  <div>
                    <strong>自动刷新</strong>
                    <p>每 15 秒更新状态，编辑中的内容不会被覆盖。</p>
                  </div>
                  <Switch
                    id="auto-refresh"
                    checked={autoRefresh}
                    onCheckedChange={setAutoRefresh}
                    label="自动刷新"
                  />
                </div>
                <div className="settings-row">
                  <div>
                    <strong>界面主题</strong>
                    <p>支持浅色、深色和跟随系统。</p>
                  </div>
                  {themeMenu}
                </div>
              </section>
              <section className="settings-section">
                <div>
                  <h2>关于 NetShare</h2>
                  <p>Go 网络控制器与私网节点代理。</p>
                </div>
                <div className="about-facts">
                  <span>
                    控制台版本 <Badge>v{overview.version}</Badge>
                  </span>
                  <span>
                    配置修订 <code>{overview.revision}</code>
                  </span>
                </div>
                <p className="muted helper">
                  界面参考 Shadcn Admin，采用 MIT 许可。
                </p>
              </section>
            </div>
          )}
        </main>
        <footer className="app-footer">
          <span>
            NetShare{" "}
            <span className="mono">
              v{overview?.version || session.version}
            </span>
          </span>
          <span>
            <ShieldCheck size={12} />
            双向 TLS<span className="footer-divider">/</span>修订{" "}
            {overview?.revision ?? "—"}
          </span>
        </footer>
      </div>
      {editor && overview && (
        <MappingEditor
          open
          mapping={editor.mapping}
          initial={editor.initial}
          nodes={overview.nodes}
          busy={busy}
          error={formError}
          onClose={closeEditor}
          onSave={saveMapping}
          onDelete={deleteMapping}
          onReverse={reverseDraft}
          onDirtyChange={updateDirty}
        />
      )}
      <PasswordDialog
        open={passwordOpen}
        onClose={() => {
          if (!passwordBusy) setPasswordOpen(false);
        }}
        onBusyChange={setPasswordBusy}
        onSaved={(csrf) => {
          if (sessionRef.current)
            authenticate({ ...sessionRef.current, csrf_token: csrf });
          setPasswordOpen(false);
          notify("密码已修改，其他设备的登录已失效。");
          void refresh(true);
        }}
      />
      <ConfirmDialog state={confirm} onClose={() => setConfirm(null)} />
      {toast && (
        <div
          className={cn("toast", toast.error && "toast-error")}
          role={toast.error ? "alert" : "status"}
        >
          {toast.error ? <CircleHelp size={18} /> : <Check size={18} />}
          <span>{toast.message}</span>
          <button aria-label="关闭提示" onClick={() => setToast(null)}>
            <X size={15} />
          </button>
        </div>
      )}
    </div>
  );
}
function Brand({ collapsed = false }: { collapsed?: boolean }) {
  return (
    <div className="brand">
      <span className="brand-mark">
        <Network size={21} />
      </span>
      {!collapsed && (
        <div>
          <strong>NetShare</strong>
          <span>网络管理台</span>
        </div>
      )}
    </div>
  );
}
function LoginView({
  error,
  onError,
  onLogin,
}: {
  error: string;
  onError: (value: string) => void;
  onLogin: (session: Session) => void;
}) {
  const [username, setUsername] = useState("admin"),
    [password, setPassword] = useState(""),
    [visible, setVisible] = useState(false),
    [busy, setBusy] = useState(false);
  async function submit(event: FormEvent) {
    event.preventDefault();
    if (busy) return;
    setBusy(true);
    onError("");
    try {
      const value = await api<Session>("/api/login", {
        method: "POST",
        body: { username: username.trim(), password },
      });
      if (!value.authenticated) throw new Error("登录失败，请重试。");
      setPassword("");
      onLogin(value);
    } catch (value) {
      onError(errorMessage(value));
    } finally {
      setBusy(false);
    }
  }
  return (
    <div id="login-view" className="login-layout">
      <div className="login-brand">
        <Brand />
      </div>
      <section className="login-panel">
        <div className="login-heading">
          <h1>登录工作台</h1>
          <p>管理你的私网与端口映射。</p>
        </div>
        <form id="login-form" onSubmit={submit} className="form-stack">
          {error && <Alert id="login-error">{error}</Alert>}
          <div className="field">
            <label htmlFor="login-username">用户名</label>
            <input
              id="login-username"
              className="input"
              value={username}
              onChange={(event) => setUsername(event.target.value)}
              autoComplete="username"
              maxLength={128}
              required
              disabled={busy}
            />
          </div>
          <div className="field">
            <label htmlFor="login-password">密码</label>
            <div className="password-input">
              <input
                id="login-password"
                className="input"
                type={visible ? "text" : "password"}
                value={password}
                onChange={(event) => setPassword(event.target.value)}
                autoComplete="current-password"
                required
                disabled={busy}
              />
              <Button
                variant="ghost"
                size="icon"
                onClick={() => setVisible(!visible)}
                aria-label={visible ? "隐藏密码" : "显示密码"}
              >
                {visible ? <EyeOff size={16} /> : <Eye size={16} />}
              </Button>
            </div>
          </div>
          <Button
            id="login-submit"
            type="submit"
            className="full-width"
            disabled={busy}
          >
            {busy && <Loader2 size={16} className="spinning" />}
            {busy ? "正在登录…" : "登录"}
          </Button>
        </form>
        <div className="login-security">
          <ShieldCheck size={15} />
          <span>安全会话 · 节点双向 TLS</span>
        </div>
      </section>
      <p className="login-footer">NetShare · 私网连接，统一管理</p>
    </div>
  );
}
function PasswordDialog({
  open,
  onClose,
  onSaved,
  onBusyChange,
}: {
  open: boolean;
  onClose: () => void;
  onSaved: (csrf: string) => void;
  onBusyChange: (value: boolean) => void;
}) {
  const [current, setCurrent] = useState(""),
    [password, setPassword] = useState(""),
    [repeat, setRepeat] = useState(""),
    [busy, setBusy] = useState(false),
    [error, setError] = useState("");
  useEffect(() => {
    if (open) {
      setCurrent("");
      setPassword("");
      setRepeat("");
      setError("");
    }
  }, [open]);
  async function submit(event: FormEvent) {
    event.preventDefault();
    if (busy) return;
    setError("");
    if (
      Array.from(password).length < 12 ||
      new TextEncoder().encode(password).length > 72
    ) {
      setError("新密码至少 12 个字符，最多 72 字节。");
      return;
    }
    if (password !== repeat) {
      setError("两次输入的新密码不一致。");
      return;
    }
    setBusy(true);
    onBusyChange(true);
    try {
      const result = await api<{ csrf_token: string }>("/api/password", {
        method: "POST",
        body: { current_password: current, new_password: password },
      });
      setCurrent("");
      setPassword("");
      setRepeat("");
      onSaved(result.csrf_token);
    } catch (value) {
      setError(errorMessage(value));
    } finally {
      setBusy(false);
      onBusyChange(false);
    }
  }
  return (
    <Modal
      id="password-dialog"
      open={open}
      onOpenChange={(value) => {
        if (!value && !busy) onClose();
      }}
      title="修改密码"
      description="新密码保存后，其他设备的登录会失效。"
      closeDisabled={busy}
      footer={
        <>
          <Button
            id="password-cancel"
            variant="outline"
            onClick={onClose}
            disabled={busy}
          >
            取消
          </Button>
          <Button
            id="password-save"
            type="submit"
            form="password-form"
            disabled={busy}
          >
            {busy ? "正在保存…" : "保存新密码"}
          </Button>
        </>
      }
    >
      <form id="password-form" className="form-stack" onSubmit={submit}>
        {error && <Alert id="password-error">{error}</Alert>}
        <div className="field">
          <label htmlFor="password-current">当前密码</label>
          <input
            id="password-current"
            className="input"
            autoComplete="current-password"
            type="password"
            value={current}
            onChange={(event) => setCurrent(event.target.value)}
            disabled={busy}
            required
          />
        </div>
        <div className="field">
          <label htmlFor="password-new">新密码</label>
          <input
            id="password-new"
            className="input"
            autoComplete="new-password"
            type="password"
            value={password}
            onChange={(event) => setPassword(event.target.value)}
            disabled={busy}
            required
          />
          <p className="field-hint">至少 12 个字符，最多 72 字节。</p>
        </div>
        <div className="field">
          <label htmlFor="password-repeat">确认新密码</label>
          <input
            id="password-repeat"
            className="input"
            autoComplete="new-password"
            type="password"
            value={repeat}
            onChange={(event) => setRepeat(event.target.value)}
            disabled={busy}
            required
          />
        </div>
      </form>
    </Modal>
  );
}
