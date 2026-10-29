import { JoinNodeButton } from "../components/join-node";
import type { ReactNode } from "react";
import {
  AlertCircle,
  ArrowDown,
  ArrowUp,
  Check,
  ChevronDown,
  Globe2,
  Network,
  Server,
  ShieldCheck,
} from "lucide-react";
import type { NodeStatus, Overview } from "../lib/types";

function formatBytes(value: number) {
  if (!Number.isFinite(value) || value < 0) return "—";
  if (value < 1024) return `${value} B`;
  const units = ["KiB", "MiB", "GiB", "TiB", "PiB"];
  const index = Math.min(
    Math.floor(Math.log(value) / Math.log(1024)) - 1,
    units.length - 1,
  );
  return `${(value / 1024 ** (index + 1)).toFixed(2)} ${units[index]}`;
}

function formatUptime(value: number) {
  const seconds = Math.max(0, Math.floor(value || 0));
  if (seconds < 60) return `${seconds} 秒`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟`;
  if (seconds < 86400)
    return `${Math.floor(seconds / 3600)} 小时 ${Math.floor((seconds % 3600) / 60)} 分钟`;
  return `${Math.floor(seconds / 86400)} 天 ${Math.floor((seconds % 86400) / 3600)} 小时`;
}

function formatDate(value?: string) {
  if (!value) return "尚未应用";
  const date = new Date(value);
  if (!Number.isFinite(date.getTime()) || date.getFullYear() < 2000)
    return "尚未应用";
  return new Intl.DateTimeFormat("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  }).format(date);
}

function Fact({
  label,
  children,
  detail,
}: {
  label: string;
  children: ReactNode;
  detail?: ReactNode;
}) {
  return (
    <div>
      <dt>{label}</dt>
      <dd>
        {children}
        {detail && <span className="muted fact-detail">{detail}</span>}
      </dd>
    </div>
  );
}

function NodePanel({ node }: { node: NodeStatus }) {
  const status = node.status;
  const ports = [...new Set([22, ...(node.reserved_ports || [])])].sort(
    (a, b) => a - b,
  );
  const rules = status?.rules || [];
  const listeners = status?.listeners || [];
  const interfaces = status?.interfaces || [];
  const warnings = status?.warnings || [];
  return (
    <article
      className="panel node-panel"
      aria-labelledby={`node-title-${node.id}`}
    >
      <header className="panel-header node-heading">
        <div className="node-title">
          <span className="node-icon">
            <Server size={20} aria-hidden="true" />
          </span>
          <div>
            <h2 id={`node-title-${node.id}`}>{node.name}</h2>
            <p className="muted mono">{node.id}</p>
          </div>
        </div>
        <div className="node-head-status">
          <span
            className={`badge ${node.online ? "badge-success" : "badge-destructive"}`}
          >
            {node.online ? (
              <Check size={12} aria-hidden="true" />
            ) : (
              <AlertCircle size={12} aria-hidden="true" />
            )}
            {node.online ? "代理在线" : "代理离线"}
          </span>
          {node.online && (
            <span className="muted mono">{node.latency_ms} ms</span>
          )}
        </div>
      </header>
      <div className="node-body">
        <div className="node-addresses">
          <div>
            <span className="muted address-label">
              <Globe2 size={15} aria-hidden="true" />
              公网入口
            </span>
            <code>{node.public_ip}</code>
            <span className="muted mono">{node.public_interface}</span>
          </div>
          <div>
            <span className="muted address-label">
              <Network size={15} aria-hidden="true" />
              私网地址
            </span>
            <code>{node.private_ip}</code>
            <span className="muted mono">{node.private_interface}</span>
          </div>
        </div>
        {!node.online && (
          <div className="info-banner banner-error" role="alert">
            <AlertCircle size={17} aria-hidden="true" />
            <div>
              <strong>节点暂时无法连接</strong>
              <p>{node.error || "检查节点代理服务和私网连通性。"}</p>
            </div>
          </div>
        )}
        {warnings.length > 0 && (
          <div className="info-banner banner-warning" role="status">
            <AlertCircle size={17} aria-hidden="true" />
            <div>
              <strong>节点需要留意</strong>
              <ul>
                {warnings.map((warning, index) => (
                  <li key={`${index}-${warning}`}>{warning}</li>
                ))}
              </ul>
            </div>
          </div>
        )}
        <dl className="detail-grid">
          <Fact label="IPv4 转发">
            {status ? (
              <span
                className={status.forwarding ? "text-success" : "text-warning"}
              >
                {status.forwarding ? "已开启" : "未开启"}
              </span>
            ) : (
              "—"
            )}
          </Fact>
          <Fact
            label="生效规则"
            detail={status ? `共 ${rules.length} 条规则` : undefined}
          >
            {status ? `${rules.filter((rule) => rule.enabled).length} 条` : "—"}
          </Fact>
          <Fact
            label="应用修订"
            detail={status ? `已保存 ${status.saved_revision}` : undefined}
          >
            {status ? <span className="mono">{status.revision}</span> : "—"}
          </Fact>
          <Fact
            label="代理运行"
            detail={status ? `v${status.version}` : undefined}
          >
            {status ? formatUptime(status.uptime_seconds) : "—"}
          </Fact>
          <Fact label="最近应用">
            {status ? formatDate(status.applied_at) : "—"}
          </Fact>
        </dl>
        <div className="node-section">
          <h3>
            <ShieldCheck size={15} aria-hidden="true" />
            保留端口
          </h3>
          <div className="port-list">
            {ports.map((port) => (
              <span className="badge badge-secondary mono" key={port}>
                {port}
              </span>
            ))}
          </div>
          <p className="muted helper">
            这些端口不会分配给映射；新规则还会检查本机监听冲突。
          </p>
        </div>
        {status && (
          <>
            <details className="node-disclosure" open>
              <summary>
                <span>
                  网卡累计流量{" "}
                  <span className="muted">{interfaces.length} 张网卡</span>
                </span>
                <ChevronDown size={16} aria-hidden="true" />
              </summary>
              {interfaces.length ? (
                <div className="table-wrap">
                  <table className="data-table compact-table">
                    <thead>
                      <tr>
                        <th scope="col">网卡</th>
                        <th scope="col">
                          <span className="inline-label">
                            <ArrowDown size={13} aria-hidden="true" />
                            累计接收
                          </span>
                        </th>
                        <th scope="col">
                          <span className="inline-label">
                            <ArrowUp size={13} aria-hidden="true" />
                            累计发送
                          </span>
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {interfaces.map((iface) => (
                        <tr key={iface.name}>
                          <td className="mono">{iface.name}</td>
                          <td className="mono">
                            {formatBytes(iface.rx_bytes)}
                          </td>
                          <td className="mono">
                            {formatBytes(iface.tx_bytes)}
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              ) : (
                <p className="muted helper">暂未读取到网卡计数。</p>
              )}
              <p className="muted helper">
                系统网卡总量，包含该机器的其他流量。
              </p>
            </details>
            <details className="node-disclosure">
              <summary>
                <span>
                  监听端口 <span className="muted">{listeners.length} 个</span>
                </span>
                <ChevronDown size={16} aria-hidden="true" />
              </summary>
              {listeners.length ? (
                <div className="table-wrap">
                  <table className="data-table compact-table">
                    <thead>
                      <tr>
                        <th scope="col">协议</th>
                        <th scope="col">地址</th>
                        <th scope="col">端口</th>
                        <th scope="col">进程</th>
                      </tr>
                    </thead>
                    <tbody>
                      {listeners.map((listener, index) => (
                        <tr
                          key={`${listener.protocol}-${listener.address}-${listener.port}-${index}`}
                        >
                          <td>
                            <span className="badge badge-secondary">
                              {listener.protocol.toUpperCase()}
                            </span>
                          </td>
                          <td className="mono">{listener.address}</td>
                          <td className="mono">{listener.port}</td>
                          <td className="listener-process">
                            {listener.process || "—"}
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              ) : (
                <p className="muted helper">暂未读取到监听端口。</p>
              )}
            </details>
          </>
        )}
      </div>
    </article>
  );
}

export function NodesPage({ overview, onJoined }: { overview: Overview; onJoined: () => void }) {
  const nodes = overview.nodes || [];
  const online = nodes.filter((node) => node.online).length;
  if (!nodes.length)
    return (
      <div className="panel empty-state">
        <Server size={28} aria-hidden="true" />
        <h2>尚未接入机器</h2>
        <p className="muted">
          点“接入节点”，复制命令到服务器执行，信息会自动出现在这里。
        </p>
        <JoinNodeButton onJoined={onJoined} />
      </div>
    );
  return (
    <div className="page-stack">
      <div className="section-meta">
        <span>
          {online === nodes.length ? (
            <Check size={15} aria-hidden="true" />
          ) : (
            <AlertCircle size={15} aria-hidden="true" />
          )}
          {online} / {nodes.length} 台代理在线
        </span>
        <JoinNodeButton onJoined={onJoined} disabled={nodes.length >= 32} />
      </div>
      <div className="node-grid">
        {nodes.map((node) => (
          <NodePanel key={node.id} node={node} />
        ))}
      </div>
      <div className="info-banner">
        <Network size={17} aria-hidden="true" />
        <p>
          平台分配私网后，系统里也需要配置好网卡和路由。代理在线说明私网管理通道已接通；服务映射的实际状态仍以映射页为准。
        </p>
      </div>
    </div>
  );
}
