export interface Session {
  authenticated: boolean;
  username: string;
  csrf_token: string;
  version: string;
}
export interface MappingInput {
  name: string;
  source_node: string;
  target_node: string;
  protocol: string;
  listen_port: string;
  target_port: string;
  enabled: boolean;
}
export interface Mapping extends MappingInput {
  id: string;
  created_at: string;
  updated_at: string;
}
export interface AgentRule {
  id: string;
  name: string;
  protocol: string;
  listen_port: string;
  target_port: string;
  target_ip: string;
  enabled: boolean;
}
export interface Listener {
  protocol: string;
  address: string;
  port: number;
  process?: string;
}
export interface AgentStatus {
  node_id: string;
  version: string;
  revision: number;
  saved_revision: number;
  applied_at: string;
  forwarding: boolean;
  rules: AgentRule[];
  listeners: Listener[];
  interfaces: { name: string; rx_bytes: number; tx_bytes: number }[];
  uptime_seconds: number;
  warnings: string[];
}
export interface NodeStatus {
  id: string;
  name: string;
  public_ip: string;
  private_ip: string;
  public_interface: string;
  private_interface: string;
  reserved_ports: number[];
  online: boolean;
  error?: string;
  latency_ms: number;
  status?: AgentStatus;
}
export interface AuditEvent {
  id: string;
  time: string;
  action: string;
  detail: string;
  success: boolean;
}
export interface Overview {
  nodes: NodeStatus[];
  mappings: Mapping[];
  events: AuditEvent[];
  revision: number;
  version: string;
}
export interface ProbeResult {
  reachable: boolean;
  latency_ms: number;
  message: string;
  interface: string;
}
export type View = "mappings" | "nodes" | "events" | "settings";
