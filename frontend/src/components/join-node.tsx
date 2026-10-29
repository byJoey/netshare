import { useEffect, useRef, useState, type FormEvent } from "react";
import { Check, Copy, Loader2, Plus, RefreshCw } from "lucide-react";
import { api, errorMessage } from "../lib/api";
import { Alert, Button, Modal } from "./ui";

type JoinState = {
  id: string;
  name?: string;
  command?: string;
  phase: string;
  message: string;
  node_id?: string;
  expires_at: string;
};

export function JoinNodeButton({ onJoined, disabled = false }: { onJoined: () => void; disabled?: boolean }) {
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [ticket, setTicket] = useState<JoinState | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [copied, setCopied] = useState(false);
  const [copyFailed, setCopyFailed] = useState(false);
  const notified = useRef("");
  const onJoinedRef = useRef(onJoined);
  onJoinedRef.current = onJoined;
  const commandRef = useRef<HTMLTextAreaElement>(null);

  async function generate() {
    if (busy) return null;
    if (Array.from(name.trim()).length > 80) {
      setError("节点名称最多 80 个字。");
      return null;
    }
    setBusy(true);
    setError("");
    setCopied(false);
    setCopyFailed(false);
    try {
      const next = await api<JoinState>("/api/node-joins", { method: "POST", body: { name: name.trim() } });
      setTicket(next);
      return next;
    } catch (e) {
      setError(errorMessage(e));
      return null;
    } finally {
      setBusy(false);
    }
  }

  useEffect(() => {
    if (!open || !ticket || ["online", "expired", "revoked"].includes(ticket.phase)) return;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout>;
    const id = ticket.id;
    async function poll() {
      try {
        const next = await api<JoinState>(`/api/node-joins/${id}`, { timeout: 10000 });
        if (stopped) return;
        setError("");
        setTicket(old => old?.id === id ? { ...old, ...next } : old);
        if (next.phase === "online" && notified.current !== id) {
          notified.current = id;
          onJoinedRef.current();
        }
        if (["online", "expired", "revoked"].includes(next.phase)) return;
      } catch (e) {
        if (!stopped) setError(errorMessage(e));
      }
      if (!stopped) timer = setTimeout(() => void poll(), 2500);
    }
    timer = setTimeout(() => void poll(), 1500);
    return () => { stopped = true; clearTimeout(timer); };
  }, [open, ticket?.id, ticket?.phase]);

  useEffect(() => {
    if (!copyFailed || !ticket?.command) return;
    const frame = requestAnimationFrame(() => {
      commandRef.current?.focus();
      commandRef.current?.select();
    });
    return () => cancelAnimationFrame(frame);
  }, [copyFailed, ticket?.id]);

  async function copy(command = ticket?.command) {
    if (!command) return;
    try {
      await navigator.clipboard.writeText(command);
      setCopied(true);
      setCopyFailed(false);
    } catch {
      setCopyFailed(true);
    }
  }

  async function generateAndCopy(event: FormEvent) {
    event.preventDefault();
    const next = await generate();
    if (next) await copy(next.command);
  }

  async function reset() {
    if (busy) return;
    setBusy(true);
    setError("");
    try {
      // 换一条命令先撤销旧的；已经接入的节点不受影响。
      if (ticket && !["online", "expired", "revoked"].includes(ticket.phase)) {
        await api(`/api/node-joins/${ticket.id}`, { method: "DELETE" });
      }
      if (ticket?.phase === "online") setName("");
      setTicket(null);
      setCopied(false);
      setCopyFailed(false);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }

  const finished = ticket?.phase === "online";
  const expired = ticket && ["expired", "revoked"].includes(ticket.phase);
  return <>
    <Button variant="outline" disabled={disabled} onClick={() => setOpen(true)}><Plus size={16} />接入节点</Button>
    <Modal open={open} onOpenChange={setOpen} title="接入一台服务器"
      description="复制命令，在要接入的服务器上以 root 执行。"
      id="join-node-dialog" closeDisabled={busy}
      footer={<>
        {ticket && <Button variant="ghost" disabled={busy} onClick={() => void reset()}><RefreshCw size={15} />{finished ? "再接一台" : "换一条命令"}</Button>}
        <Button variant={ticket ? "default" : "outline"} onClick={() => setOpen(false)} disabled={busy}>{finished ? "完成" : ticket ? "稍后查看" : "取消"}</Button>
      </>}>
      {error && <Alert id="join-error">{error}</Alert>}
      {busy ? <p className="join-status" role="status"><Loader2 className="spinning" size={18} />正在生成命令…</p> : finished ?
        <div className="join-success" role="status"><Check size={28} /><h3>服务器已接入</h3>{ticket.name && <p className="join-node-name">{ticket.name}</p>}<p className="muted">网络信息已自动识别，可以创建映射了。</p></div> : ticket ? <>
          <p className="muted helper">节点名称：{ticket.name || "自动识别"}</p>
          <label className="field-label" htmlFor="join-command">在新服务器执行这条命令</label>
          <textarea ref={commandRef} id="join-command" className="join-command mono" readOnly value={expired ? "命令已失效，请重新生成。" : ticket.command || ""} spellCheck={false} />
          <Button disabled={!!expired || busy} onClick={() => void copy()}><Copy size={16} />{copied ? "已复制" : "复制命令"}</Button>
          {copyFailed && <p className="field-hint">命令已选中，请长按或按 Ctrl+C 复制。</p>}
          <p className="join-status" role="status">{!expired && <Loader2 size={16} className="spinning" />}{ticket.message}</p>
          <p className="muted helper">15 分钟内有效，仅用于一台服务器。安装遇到中断，可重新执行同一条命令。</p>
          <p className="muted helper">服务器需要已开通私网。IP 和网卡会自动识别。</p>
        </> : <form className="form-stack" onSubmit={event => void generateAndCopy(event)}>
          <div className="field">
            <label htmlFor="join-node-name">节点名称（选填）</label>
            <input id="join-node-name" className="input" value={name} onChange={event => setName(event.target.value)} maxLength={160} placeholder="例如：香港入口、美国主机" aria-describedby="join-name-hint" disabled={busy} />
            <p className="field-hint" id="join-name-hint">留空使用服务器名，接入后也能改。</p>
          </div>
          <Button id="join-generate" type="submit" disabled={busy}><Copy size={16} />生成并复制命令</Button>
        </form>}
    </Modal>
  </>;
}
