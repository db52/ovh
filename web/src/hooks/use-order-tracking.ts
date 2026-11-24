import { queryOptions, useQuery } from "@tanstack/react-query";
import { api } from "@/lib/api";
import { qk } from "@/lib/query";
import i18n from "@/i18n";

export type OrderTrackingTarget =
  | { kind: "history"; historyId: string; orderId: string }
  | { kind: "account"; accountId: string; orderId: string; listedStatus?: string };

/** OVH order IDs are positive decimal int64s. Never repair or round an identifier. */
function validOrderId(id?: string): id is string {
  return typeof id === "string" && /^[1-9]\d{0,18}$/.test(id) &&
    (id.length < 19 || id <= "9223372036854775807");
}

export function historyTrackingTarget(historyId?: string, orderId?: string): OrderTrackingTarget | null {
  if (typeof historyId !== "string" || !historyId || historyId.trim() !== historyId || historyId === "." || historyId === ".." || !validOrderId(orderId)) return null;
  return { kind: "history", historyId, orderId };
}

export function accountTrackingTarget(accountId: string | undefined, orderId: number | string, listedStatus?: string): OrderTrackingTarget | null {
  // The account list currently emits JSON numbers. Reject numbers that have already lost precision.
  const id = typeof orderId === "string" ? orderId : Number.isSafeInteger(orderId) ? String(orderId) : undefined;
  if (typeof accountId !== "string" || !accountId || accountId.trim() !== accountId || !validOrderId(id)) return null;
  return { kind: "account", accountId, orderId: id, ...(listedStatus && listedStatus !== "unknown" ? { listedStatus } : {}) };
}

export interface OrderTracking {
  orderId: string;
  orderStatus?: string;
  orderStatusAt?: string;
  statusSource: "live" | "cached" | "unknown";
  detailState: "available" | "empty" | "unavailable" | "forbidden";
  steps: { step: string; status: string; history: { date: string; label: string; description: string }[] }[];
  checkedAt: string;
}

/** Use only the original row's status when the live read has no status; never invent its timestamp or change the query cache. */
export function trackingDisplayData(data: OrderTracking, target: OrderTrackingTarget): OrderTracking {
  if (target.kind === "account" && data.orderId === target.orderId && target.listedStatus && data.statusSource === "unknown" && !data.orderStatus) {
    return { ...data, statusSource: "cached", orderStatus: target.listedStatus, orderStatusAt: undefined };
  }
  return data;
}

export function orderTrackingQuery(target: OrderTrackingTarget) {
  return queryOptions({
    queryKey: qk.orderTracking(target.kind, target.kind === "history" ? target.historyId : target.accountId, target.orderId),
    queryFn: async ({ signal }) => {
      // History resolves its own owner on the server. The account route always uses the listed order's scope.
      const { data } = target.kind === "history"
        ? await api.get<OrderTracking>(`/purchase-history/${encodeURIComponent(target.historyId)}/tracking`, { signal })
        : await api.get<OrderTracking>(`/ovh/account/orders/${target.orderId}/tracking`, { signal, params: { account: target.accountId } });
      if (data?.orderId !== target.orderId || !["live", "cached", "unknown"].includes(data.statusSource) ||
          !["available", "empty", "unavailable", "forbidden"].includes(data.detailState) ||
          (data.detailState === "available" && !Array.isArray(data.steps))) {
        throw new Error(i18n.t("orderTracking.loadError"));
      }
      return data;
    },
    staleTime: 30_000,
    gcTime: 2 * 60_000,
    retry: false,
    refetchInterval: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });
}

/** Mounted by the open dialog only; closed rows don't even subscribe to a query. */
export function useOrderTracking(target: OrderTrackingTarget) {
  return useQuery(orderTrackingQuery(target));
}
