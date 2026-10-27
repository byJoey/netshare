import type { Session } from "./types";

// 入口由服务端注入；不从浏览器地址或持久存储猜测管理路径。
const configuredBasePath =
  document.querySelector<HTMLMetaElement>('meta[name="netshare-base-path"]')
    ?.content || "";
if (
  configuredBasePath &&
  !/^\/[A-Za-z0-9][A-Za-z0-9_-]{15,95}$/.test(configuredBasePath)
) {
  throw new Error("管理入口配置无效，请联系管理员。");
}
const basePath = configuredBasePath;

let session: Session | null = null;
export function setApiSession(value: Session | null) {
  session = value;
}
export class ApiError extends Error {
  constructor(
    message: string,
    public status: number,
  ) {
    super(message);
  }
}
export async function api<T>(
  path: string,
  options: { method?: string; body?: unknown; timeout?: number } = {},
): Promise<T> {
  const method = options.method || "GET";
  const abort = new AbortController();
  const timeout = window.setTimeout(
    () => abort.abort(),
    options.timeout ?? (method === "GET" ? 30000 : 120000),
  );
  const headers: Record<string, string> = { Accept: "application/json" };
  if (options.body !== undefined) headers["Content-Type"] = "application/json";
  if (method !== "GET" && session?.csrf_token)
    headers["X-CSRF-Token"] = session.csrf_token;
  try {
    const response = await fetch(basePath + path, {
      method,
      headers,
      credentials: "same-origin",
      cache: "no-store",
      signal: abort.signal,
      body:
        options.body === undefined ? undefined : JSON.stringify(options.body),
    });
    const value = await response.json().catch(() => ({}));
    if (!response.ok) {
      if (response.status === 401 && path !== "/api/login")
        window.dispatchEvent(new Event("netshare:expired"));
      throw new ApiError(
        value.error || `请求失败（HTTP ${response.status}）`,
        response.status,
      );
    }
    return value as T;
  } catch (error) {
    if (error instanceof DOMException && error.name === "AbortError")
      throw new Error(
        method === "GET"
          ? "请求超时，请稍后重试。"
          : "请求超时，请先刷新确认实际结果，再重试。",
      );
    throw error;
  } finally {
    window.clearTimeout(timeout);
  }
}
export const errorMessage = (error: unknown) =>
  error instanceof Error ? error.message : "操作失败，请稍后重试。";
