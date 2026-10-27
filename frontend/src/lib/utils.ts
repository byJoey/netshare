import type { Mapping, NodeStatus } from "./types";
export const cn = (...values: (string | false | undefined)[]) =>
  values.filter(Boolean).join(" ");
export function formatTime(value: string | Date, detailed = false) {
  const date = new Date(value);
  if (!Number.isFinite(date.getTime()) || date.getFullYear() < 2000)
    return "尚无记录";
  return new Intl.DateTimeFormat(
    "zh-CN",
    detailed
      ? {
          month: "2-digit",
          day: "2-digit",
          hour: "2-digit",
          minute: "2-digit",
          second: "2-digit",
          hour12: false,
        }
      : {
          hour: "2-digit",
          minute: "2-digit",
          second: "2-digit",
          hour12: false,
        },
  ).format(date);
}
export function protocolLabel(value: string) {
  return value === "both" ? "TCP + UDP" : value.toUpperCase();
}
export function mappingInput(mapping: Mapping) {
  const {
    name,
    source_node,
    target_node,
    protocol,
    listen_port,
    target_port,
    enabled,
  } = mapping;
  return {
    name,
    source_node,
    target_node,
    protocol,
    listen_port,
    target_port,
    enabled,
  };
}
export function parsePorts(value: string): [number, number] {
  if (!/^\d+(?:-\d+)?$/.test(value.trim()))
    throw new Error("端口格式应为 8080 或 8080-8090。");
  const [start, end = start] = value.trim().split("-").map(Number);
  if (start < 1 || end > 65535 || end < start)
    throw new Error("端口必须在 1–65535 之间，结束端口不能小于起始端口。");
  if (end - start > 999) throw new Error("单条映射最多包含 1000 个端口。");
  return [start, end];
}
function samePorts(a: string, b: string) {
  try {
    return JSON.stringify(parsePorts(a)) === JSON.stringify(parsePorts(b));
  } catch {
    return false;
  }
}
export function mappingState(mapping: Mapping, nodes: NodeStatus[]) {
  const node = nodes.find((n) => n.id === mapping.source_node);
  if (!node?.online || !node.status)
    return { label: "入口离线", tone: "destructive" };
  const applied = node.status.rules?.find((rule) => rule.id === mapping.id);
  if (!mapping.enabled)
    return applied?.enabled
      ? { label: "停用待同步", tone: "warning" }
      : { label: "已停用", tone: "secondary" };
  if (!node.status.forwarding) return { label: "转发未开启", tone: "warning" };
  const target = nodes.find((n) => n.id === mapping.target_node);
  const matches =
    applied?.enabled &&
    applied.protocol === mapping.protocol &&
    samePorts(applied.listen_port, mapping.listen_port) &&
    samePorts(applied.target_port, mapping.target_port) &&
    applied.target_ip === target?.private_ip;
  return matches
    ? { label: "已应用", tone: "success" }
    : { label: "待同步", tone: "warning" };
}
