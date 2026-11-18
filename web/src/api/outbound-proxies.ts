import type { OutboundProxy } from "@/types/models";
import http from "@/utils/http";
import { isAxiosError } from "axios";

export type OutboundProxyInput = Pick<OutboundProxy, "name" | "url" | "enabled">;

export const outboundProxiesApi = {
  async list(silent = false): Promise<OutboundProxy[]> {
    const res = await http.get("/outbound-proxies", { silent });
    return res.data || [];
  },
  async batch(
    items: Array<{ url: string; enabled: boolean }>
  ): Promise<{ created: number; skipped: number }> {
    const res = await http.post("/outbound-proxies/batch", { items }, { hideMessage: true });
    return res.data;
  },
  async create(data: OutboundProxyInput): Promise<void> {
    await http.post("/outbound-proxies", data);
  },
  async update(id: number, data: Partial<OutboundProxyInput>): Promise<void> {
    await http.put(`/outbound-proxies/${id}`, data);
  },
  async remove(id: number): Promise<void> {
    await http.delete(`/outbound-proxies/${id}`);
  },
};

export interface SubscriptionDiff {
  added: number;
  updated: number;
  removed: number;
  unchanged: number;
}
export interface SubscriptionInput {
  mode: "manual" | "subscription";
  format: "webshare" | "url_list";
  protocol: "http" | "https" | "socks5" | "socks5h";
  auto_refresh: boolean;
  interval_minutes: number;
  url?: string;
  clear_url?: boolean;
  preview_token?: string;
}
export interface SubscriptionStatus extends Omit<
  SubscriptionInput,
  "url" | "clear_url" | "preview_token"
> {
  has_url: boolean;
  running: boolean;
  last_attempt_at?: string;
  last_success_at?: string;
  next_refresh_at?: string;
  last_error?: string;
  last_result?: SubscriptionDiff;
}
const subscriptionPath = "/outbound-proxies/subscription";

// Only these validation envelopes from subscription actions contain safe details.
export function subscriptionErrorDetail(error: unknown): string | undefined {
  if (!isAxiosError<{ code?: unknown; message?: unknown }>(error)) {
    return;
  }
  const response = error.response;
  const type = response?.headers["content-type"];
  const data = response?.data;
  if (
    response?.status === 400 &&
    typeof type === "string" &&
    type.split(";", 1)[0]?.trim().toLowerCase() === "application/json" &&
    (data?.code === "VALIDATION_FAILED" || data?.code === "INVALID_JSON") &&
    typeof data.message === "string" &&
    data.message.trim()
  ) {
    return data.message;
  }
}
// Subscription failures stay inline; periodic requests must not trigger global UI.
const subscriptionRequest = { silent: true, hideMessage: true, timeout: 45000 };
export const subscriptionApi = {
  async get(): Promise<SubscriptionStatus> {
    return (await http.get(subscriptionPath, { ...subscriptionRequest, timeout: 10000 })).data;
  },
  async save(input: SubscriptionInput): Promise<SubscriptionStatus> {
    return (await http.put(subscriptionPath, input, subscriptionRequest)).data;
  },
  async preview(input: SubscriptionInput): Promise<SubscriptionDiff & { preview_token: string }> {
    return (await http.post(`${subscriptionPath}/preview`, input, subscriptionRequest)).data;
  },
  async refresh(): Promise<SubscriptionDiff> {
    return (await http.post(`${subscriptionPath}/refresh`, undefined, subscriptionRequest)).data;
  },
};
