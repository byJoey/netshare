import { useEffect, useRef, useState } from "react";
import { Check, Copy, Loader2, Plus, RefreshCw } from "lucide-react";
import { api, errorMessage } from "../lib/api";
import { Alert, Button, Modal } from "./ui";

type JoinState = {
  id: string;
  command?: string;
  phase: string;
  message: string;
  node_id?: string;
  expires_at: string;
};

export function JoinNodeButton({ onJoined, disabled = false }: { onJoined: () => void; disabled?: boolean }) {
  const [open, setOpen] = useState(false);
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
    setBusy(true);
    setError("");
    setCopied(false);
    setCopyFailed(false);
    try {
      // 换一条命令先撤销旧的；已经登记的节点不受影响。
      if (ticket && ticket.phase !== "online" && ticket.phase !== "expired" && ticket.phase !== "revoked") {
        await api(`/api/node-joins/${ticket.id}`, { method: "DELETE" });
        setTicket(null);
      }
      setTicket(await api<JoinState>("/api/node-joins", { method: "POST" }));
    } catch (e) {
      setError(errorMessage(e));
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

  async function copy() {
    if (!ticket?.command) return;
    try {
      await navigator.clipboard.writeText(ticket.command);
      setCopied(true);
      setCopyFailed(false);
    } catch {
      commandRef.current?.focus();
      commandRef.current?.select();
      setCopyFailed(true);
    }
  }

  const finished = ticket?.phase === "online";
  const expired = ticket && ["expired", "revoked"].includes(ticket.phase);
  return <>
    <Button variant="outline" disabled={disabled} onClick={() => {
      setOpen(true);
      if (!ticket) void generate();
    }}><Plus size={16} />接入节点</Button>
    <Modal open={open} onOpenChange={setOpen} title="接入一台服务器"
      description="复制命令，在要接入的服务器上以 root 执行。"
      id="join-node-dialog" closeDisabled={busy}
      footer={<>
        <Button variant="ghost" disabled={busy} onClick={() => void generate()}><RefreshCw size={15} />{finished ? "再接一台" : "换一条命令"}</Button>
        <Button onClick={() => setOpen(false)} disabled={busy}>{finished ? "完成" : "稍后查看"}</Button>
      </>}>
      {error && <Alert>{error}</Alert>}
      {busy ? <p className="join-status" role="status"><Loader2 className="spin" size={18} />正在生成命令…</p> : finished ?
        <div className="join-success" role="status"><Check size={28} /><h3>服务器已接入</h3><p className="muted">网络信息已自动识别，可以创建映射了。</p></div> : ticket ? <>
          <label className="field-label" htmlFor="join-command">在新服务器执行这条命令</label>
          <textarea ref={commandRef} id="join-command" className="join-command mono" readOnly value={expired ? "命令已失效，请重新生成。" : ticket.command || ""} spellCheck={false} />
          <Button disabled={!!expired || busy} onClick={() => void copy()}><Copy size={16} />{copied ? "已复制" : "复制命令"}</Button>
          {copyFailed && <p className="field-hint">命令已选中，请长按或按 Ctrl+C 复制。</p>}
          <p className="join-status" role="status">{!expired && <Loader2 size={16} className="spin" />}{ticket.message}</p>
          <p className="muted helper">15 分钟内有效，仅用于一台服务器。安装遇到中断，可重新执行同一条命令。</p>
          <p className="muted helper">服务器需要已开通私网。IP、网卡和名称会自动识别。</p>
        </> : <p className="muted">生成后复制命令到服务器执行即可。</p>}
    </Modal>
  </>;
}
