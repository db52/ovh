import { useMemo, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { isAxiosError } from "axios";
import { parseISO } from "date-fns";
import { ChevronRight, Info, Loader2, RefreshCw } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog";
import { trackingDisplayData, useOrderTracking, type OrderTracking, type OrderTrackingTarget } from "@/hooks/use-order-tracking";
import { cn } from "@/lib/utils";

/** 可把原状态徽标放进同一个按钮，整组可点。关闭的行不挂查询，也不预读其他单据。 */
export function OrderTrackingButton({ target, badge }: { target: OrderTrackingTarget; badge?: { content: ReactNode; label: string } }) {
  const { t } = useTranslation();
  // Pin the actual record at click time, including its account. Never follow a newly selected account.
  const [selected, setSelected] = useState<OrderTrackingTarget | null>(null);
  const label = `${t("orderTracking.open")} #${target.orderId}`;
  return (
    <Dialog open={!!selected} onOpenChange={(open) => setSelected(open ? target : null)}>
      <DialogTrigger asChild>
        <button
          type="button"
          data-order-tracking-trigger={target.kind}
          data-history-id={target.kind === "history" ? target.historyId : undefined}
          data-account-id={target.kind === "account" ? target.accountId : undefined}
          data-order-id={target.orderId}
          aria-label={badge ? `${badge.label} · ${label}` : label}
          title={label}
          className={cn(
            "inline-flex min-h-11 min-w-0 max-w-full flex-wrap items-center gap-1 rounded-md px-1.5 text-start text-xs font-medium leading-snug text-primary transition-colors hover:bg-primary/5 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring sm:min-h-8",
            badge && "shrink-0 flex-nowrap px-0.5",
          )}
        >
          {badge ? badge.content : t("orderTracking.open")}
          <ChevronRight className="h-3.5 w-3.5 shrink-0" aria-hidden />
        </button>
      </DialogTrigger>
      {selected && <OrderTrackingDialog target={selected} />}
    </Dialog>
  );
}

function OrderTrackingDialog({ target }: { target: OrderTrackingTarget }) {
  const { t } = useTranslation();
  const query = useOrderTracking(target);
  const status = isAxiosError(query.error) ? query.error.response?.status : undefined;
  // Only a transient failure may keep the previous snapshot visible (and clearly labelled).
  const data = status && status >= 400 && status < 500 && status !== 429 ? undefined : query.data;
  const errorMessage = status === 403 ? "orderTracking.forbidden" : status === 404 || status === 409
    ? `orderTracking.recordUnavailable.${target.kind}` : "orderTracking.loadError";
  const retry = query.isError || data?.detailState === "unavailable";
  return (
    <DialogContent
      data-order-tracking-dialog={target.kind}
      data-history-id={target.kind === "history" ? target.historyId : undefined}
      data-account-id={target.kind === "account" ? target.accountId : undefined}
      data-order-id={target.orderId}
      className="flex max-h-[88dvh] min-w-0 flex-col gap-3 overflow-hidden sm:max-h-[85dvh] sm:max-w-2xl"
    >
      <DialogHeader className="min-w-0 shrink-0 border-b border-border pb-3 pr-10">
        <DialogTitle className="leading-tight">{t("orderTracking.title")}</DialogTitle>
        <DialogDescription className="tabular-nums [overflow-wrap:anywhere]">
          {t("orderTracking.number", { id: target.orderId })}
        </DialogDescription>
      </DialogHeader>
      <div className="min-h-0 min-w-0 space-y-4 overflow-y-auto overscroll-contain [overflow-wrap:anywhere]" aria-busy={query.isFetching}>
        {data ? (
          <>
            {query.isError && <Notice>{t("orderTracking.staleNotice")}</Notice>}
            <OrderTrackingDetails data={trackingDisplayData(data, target)} stale={query.isError} />
          </>
        ) : query.isError ? (
          <Notice>{t(errorMessage)}</Notice>
        ) : (
          <p role="status" className="flex items-center gap-2 py-4 text-sm text-muted-foreground">
            <Loader2 className="h-4 w-4 animate-spin motion-reduce:animate-none" aria-hidden />{t("orderTracking.loading")}
          </p>
        )}
      </div>
      <div className="flex shrink-0 justify-end border-t border-border pt-3">
        <Button type="button" size="sm" variant="outline" disabled={query.isFetching} onClick={() => void query.refetch()}>
          <RefreshCw aria-hidden className={cn("h-3.5 w-3.5", query.isFetching && "animate-spin motion-reduce:animate-none")} />
          {t(retry ? "common.retry" : "common.refresh")}
        </Button>
      </div>
    </DialogContent>
  );
}

const known = {
  step: ["VALIDATING", "VALIDATED", "DELIVERING", "AVAILABLE"],
  stepStatus: ["TODO", "DOING", "DONE", "ERROR"],
  orderStatus: ["notPaid", "checking", "delivering", "delivered", "cancelling", "cancelled", "documentsRequested", "unknown"],
  event: ["DELIVERY", "FRAUD_CHECK", "FRAUD_DOCS_REQUESTED", "FRAUD_MANUAL_REVIEW", "FRAUD_REFUSED", "INVOICE_IN_PROGRESS", "INVOICE_SENT", "ORDER_ACCEPTED", "ORDER_STARTED", "PAYMENT_CONFIRMED", "PAYMENT_INITIATED", "PAYMENT_RECEIVED", "REGISTERED_PAYMENT_INITIATED"],
};

/** Unknown provider codes are data, not translation keys. */
function enumLabel(t: TFunction, kind: keyof typeof known, code?: string) {
  if (!code?.trim()) return t(kind === "orderStatus" ? "orderTracking.orderStatus.unknown" : "common.unknown");
  return known[kind].includes(code) ? t(`orderTracking.${kind}.${code}`) : code;
}

function timestamp(value?: string) {
  if (!value || !/^\d{4}-\d\d-\d\d[T ]\d\d:\d\d/.test(value)) return NaN;
  return parseISO(value).getTime();
}

function TrackingTime({ value, format }: { value?: string; format: Intl.DateTimeFormat }) {
  const { t } = useTranslation();
  if (!value?.trim()) return <>{t("orderTracking.noTime")}</>;
  const at = timestamp(value);
  if (!Number.isFinite(at)) {
    // Keep a date without inventing a time of day. Preserve unrecognised input as text.
    if (/^\d{4}-\d\d-\d\d$/.test(value) && Number.isFinite(parseISO(value).getTime())) {
      return <>{value} · {t("orderTracking.noTime")}</>;
    }
    return <>{t("orderTracking.invalidTime", { value })}</>;
  }
  return <time dateTime={value} title={value}>{format.format(at)}</time>;
}

function Notice({ children }: { children: ReactNode }) {
  return (
    <p role="status" className="flex min-w-0 items-start gap-2 rounded-lg border border-border bg-muted/30 p-3 text-sm leading-relaxed">
      <Info className="mt-0.5 h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />
      <span className="min-w-0">{children}</span>
    </p>
  );
}

export function OrderTrackingDetails({ data, stale = false }: { data: OrderTracking; stale?: boolean }) {
  const { t, i18n } = useTranslation();
  const format = useMemo(() => new Intl.DateTimeFormat(i18n.language, { dateStyle: "medium", timeStyle: "medium" }), [i18n.language]);
  const source = stale && data.statusSource === "live" ? "cached" : data.statusSource;
  const state = data.detailState === "available" && !data.steps.length ? "empty" : data.detailState;
  const events = useMemo(() => {
    if (data.detailState !== "available") return [];
    // Stages can overlap. Sort across all of them; keep undated events last in provider order.
    return data.steps.flatMap(({ step, history }) => (history || []).map((event) => {
      const at = timestamp(event.date);
      return { ...event, step, at: Number.isFinite(at) ? at : Infinity };
    })).sort((a, b) => a.at < b.at ? -1 : a.at > b.at ? 1 : 0);
  }, [data]);
  const orderNote = events.every(({ at }) => Number.isFinite(at)) ? "oldestFirst"
    : events.some(({ at }) => Number.isFinite(at)) ? "undatedLast" : "providerOrder";

  return (
    <div className="min-w-0 space-y-5 [overflow-wrap:anywhere]">
      <dl className="grid min-w-0 gap-3 rounded-xl border border-border bg-muted/30 p-3.5 sm:grid-cols-2">
        <div className="min-w-0" data-status-source={source}>
          <dt className="text-xs text-muted-foreground">{t(`orderTracking.source.${source}`)}</dt>
          <dd className="mt-1 font-semibold" title={data.orderStatus}>{enumLabel(t, "orderStatus", data.orderStatus)}</dd>
          {(data.orderStatusAt || source === "cached") && (
            <dd className="mt-1 text-xs text-muted-foreground">
              {t("orderTracking.observedAt")} · <TrackingTime value={data.orderStatusAt} format={format} />
            </dd>
          )}
        </div>
        <div className="min-w-0">
          <dt className="text-xs text-muted-foreground">{t("orderTracking.checkedAt")}</dt>
          <dd className="mt-1 text-sm tabular-nums"><TrackingTime value={data.checkedAt} format={format} /></dd>
        </div>
      </dl>
      {state !== "available" ? (
        <div data-detail-state={state}><Notice>{t(`orderTracking.${state}`)}</Notice></div>
      ) : (
        <>
          <section data-detail-state="available">
            <h3 className="mb-2 text-sm font-semibold">{t("orderTracking.stages")}</h3>
            <ol className="grid grid-cols-2 gap-2 sm:grid-cols-4">
              {data.steps.map(({ step, status }, i) => (
                <li key={i} data-step={step} data-step-status={status} className="min-w-0 rounded-lg border border-border px-2.5 py-2">
                  <p title={step} className="text-[13px] font-medium">{enumLabel(t, "step", step)}</p>
                  <p title={status} className={cn("mt-1 flex items-baseline gap-1.5 text-xs", status === "DONE" ? "text-success" : status === "DOING" ? "text-primary" : status === "ERROR" ? "text-destructive" : "text-muted-foreground")}>
                    <span aria-hidden className="h-1.5 w-1.5 shrink-0 rounded-full bg-current" />
                    <span className="min-w-0">{enumLabel(t, "stepStatus", status)}</span>
                  </p>
                </li>
              ))}
            </ol>
          </section>
          <section>
            <div className="mb-3 flex flex-wrap items-baseline justify-between gap-2">
              <h3 className="text-sm font-semibold">{t("orderTracking.events")}</h3>
              {events.length > 1 && <span className="text-xs text-muted-foreground">{t(`orderTracking.${orderNote}`)}</span>}
            </div>
            {events.length === 0 ? <Notice>{t("orderTracking.noEvents")}</Notice> : (
              <ol data-tracking-events className="ml-1.5 border-l border-border pl-4">
                {events.map(({ step, date, label, description }, i) => (
                  <li key={i} className="relative min-w-0 pb-4 last:pb-0">
                    <span aria-hidden className="absolute -left-[21px] top-1.5 h-2 w-2 rounded-full bg-primary ring-2 ring-background" />
                    <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5 text-xs text-muted-foreground">
                      <span className="tabular-nums"><TrackingTime value={date} format={format} /></span>
                      <span>{enumLabel(t, "step", step)}</span>
                    </div>
                    {label && <p title={label} className="mt-1 whitespace-pre-wrap text-sm font-medium">{enumLabel(t, "event", label)}</p>}
                    {description && <p className="mt-1 whitespace-pre-wrap text-[13px] leading-relaxed">{description}</p>}
                    {!label && !description && <p className="mt-1 text-[13px] text-muted-foreground">{t("orderTracking.noText")}</p>}
                  </li>
                ))}
              </ol>
            )}
          </section>
        </>
      )}
    </div>
  );
}
