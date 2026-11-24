import assert from "node:assert/strict";
import { after, test } from "node:test";
import { fileURLToPath } from "node:url";
import { createElement as h } from "react";
import { renderToStaticMarkup as render } from "react-dom/server";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createServer } from "vite";

// Middleware mode with no development proxy. Every HTTP invocation below uses an in-memory adapter.
const vite = await createServer({
  configFile: false, root: fileURLToPath(new URL("..", import.meta.url)),
  resolve: { alias: { "@": fileURLToPath(new URL("../src", import.meta.url)) } },
  server: { middlewareMode: true, hmr: false, ws: false }, appType: "custom",
});
after(() => vite.close());
const { accountTrackingTarget, historyTrackingTarget, orderTrackingQuery, trackingDisplayData } = await vite.ssrLoadModule("/src/hooks/use-order-tracking.ts");
const { OrderTrackingButton, OrderTrackingDetails } = await vite.ssrLoadModule("/src/components/common/OrderTrackingDialog.tsx");
const { api } = await vite.ssrLoadModule("/src/lib/api.ts");
const { default: i18n } = await vite.ssrLoadModule("/src/i18n/index.ts");
const base = { orderId: "123", orderStatus: "checking", statusSource: "live", detailState: "available", checkedAt: "2026-01-01T10:30:01Z", steps: [] };

test("only exact record identities and positive decimal order IDs make a tracking target", () => {
  for (const value of [undefined, "", " 123", "123 ", "123\n", "00123", "0", "-1", "+1", "1.0", "1e2", "123/other", "9223372036854775808"]) {
    assert.equal(historyTrackingTarget("local-id", value), null);
    assert.equal(accountTrackingTarget("scope", value), null);
  }
  for (const id of [undefined, "", " ", " id", ".", ".."]) assert.equal(historyTrackingTarget(id, "123"), null);
  assert.deepEqual(historyTrackingTarget("local-id", "9223372036854775807"), { kind: "history", historyId: "local-id", orderId: "9223372036854775807" });
  assert.equal(accountTrackingTarget("", 123), null);
  assert.equal(accountTrackingTarget("scope", Number.MAX_SAFE_INTEGER + 1), null);
  assert.deepEqual(accountTrackingTarget("scope", 123), { kind: "account", accountId: "scope", orderId: "123" });
});

test("closed buttons create no tracking query; history badges are a single accessible hit target", () => {
  const client = new QueryClient();
  try {
    const target = historyTrackingTarget("local-id", "123");
    const html = render(h(QueryClientProvider, { client }, h(OrderTrackingButton, {
      target, badge: { label: "Existing status", content: h("span", null, "Existing status") },
    })));
    assert.match(html, /aria-haspopup="dialog"/);
    assert.match(html, /aria-label="Existing status · (?:Track order|追踪订单) #123"/);
    assert.doesNotMatch(html, />(?:Track order|追踪订单)</);
    assert.equal((html.match(/<button/g) || []).length, 1);
    assert.equal(client.getQueryCache().getAll().length, 0);
  } finally { client.clear(); }
});

test("the opened account row can retain its status but not invent a timestamp or borrow from another order", () => {
  const owned = accountTrackingTarget("owner", 123, "checking");
  const missing = { ...base, orderStatus: undefined, orderStatusAt: undefined, statusSource: "unknown", detailState: "forbidden" };
  assert.deepEqual(trackingDisplayData(missing, owned), { ...missing, orderStatus: "checking", statusSource: "cached" });
  assert.equal(missing.statusSource, "unknown");
  assert.equal(trackingDisplayData(base, owned), base);
  assert.equal(trackingDisplayData(missing, accountTrackingTarget("owner", 124, "delivered")), missing);
  assert.equal(trackingDisplayData(missing, historyTrackingTarget("record", "123")), missing);
});

test("GET uses only the local history ID or the pinned account; cache never crosses scopes", async () => {
  const client = new QueryClient();
  const adapter = api.defaults.adapter;
  const seen = [];
  let mismatch = false;
  api.defaults.adapter = async (config) => {
    seen.push({ path: api.getUri(config), method: config.method, aborted: config.signal?.aborted });
    return { config, status: 200, statusText: "OK", headers: {}, data: { ...base, orderId: mismatch ? "456" : "123", orderStatus: `from-check-${seen.length}` } };
  };
  try {
    // Even a history ID and an account ID that are identical literals are different read scopes.
    const history = orderTrackingQuery(historyTrackingTarget("same #", "123"));
    const otherHistory = orderTrackingQuery(historyTrackingTarget("different-record", "123"));
    const account = orderTrackingQuery(accountTrackingTarget("same #", 123));
    const otherAccount = orderTrackingQuery(accountTrackingTarget("other-owner", 123));
    assert.equal((await client.fetchQuery(history)).orderStatus, "from-check-1");
    assert.equal((await client.fetchQuery(history)).orderStatus, "from-check-1");
    assert.equal((await client.fetchQuery(otherHistory)).orderStatus, "from-check-2");
    assert.equal((await client.fetchQuery(account)).orderStatus, "from-check-3");
    assert.equal((await client.fetchQuery(otherAccount)).orderStatus, "from-check-4");
    assert.deepEqual(seen.map((call) => call.path), [
      "/api/purchase-history/same%20%23/tracking", "/api/purchase-history/different-record/tracking",
      "/api/ovh/account/orders/123/tracking?account=same+%23", "/api/ovh/account/orders/123/tracking?account=other-owner",
    ]);
    assert.ok(seen.every((call) => call.method === "get" && call.aborted === false));
    assert.equal(history.refetchInterval, false);
    mismatch = true;
    const bad = orderTrackingQuery(historyTrackingTarget("mismatch", "123"));
    await assert.rejects(client.fetchQuery(bad));
    assert.equal(client.getQueryData(bad.queryKey), undefined);
    assert.equal(seen.length, 5); // no retry or fallback to the active account
  } finally { api.defaults.adapter = adapter; client.clear(); }
});

for (const [lang, text] of Object.entries({
  zh: { live: "最新订单状态", cached: "上次已知订单状态", checked: "最后查询时间", noTime: "未提供时间", invalid: "无法识别的时间", empty: "暂未提供", unavailable: "暂时无法", forbidden: "没有读取追踪明细的权限", noEvents: "没有提供具体的事件记录" },
  en: { live: "Latest order status", cached: "Last known order status", checked: "Last checked", noTime: "Time not provided", invalid: "Unrecognized time", empty: "has not provided", unavailable: "temporarily unavailable", forbidden: "do not have permission", noEvents: "did not provide any individual events" },
})) {
  test(`${lang}: show only reported stages/events in chronological order, retaining unknown codes and text`, async () => {
    await i18n.changeLanguage(lang);
    const data = { ...base, orderStatus: "NEW_ORDER_STATE", steps: [
      { step: "VALIDATING", status: "DONE", history: [
        { date: "", label: "Undated event", description: "no estimate" },
        { date: "2026-01-01T08:00:00Z", label: "Documents requested", description: "Upload documents" },
      ] },
      { step: "VALIDATED", status: "ERROR", history: [{ date: "2026-01-01T09:00:00+02:00", label: "Payment confirmed", description: "<em>Provider text</em>" }] },
      { step: "DELIVERING", status: "DOING", history: [{ date: "2026-01-01T08:30:00Z", label: "DELIVERY", description: "Long".repeat(600) }] },
      { step: "AVAILABLE", status: "TODO", history: [] },
      { step: "NEW_STEP", status: "NEW_STATUS", history: [{ date: "not-a-time", label: "Invalid date event", description: "" }] },
    ] };
    const html = render(h(OrderTrackingDetails, { data }));
    for (const expected of [text.live, text.checked, text.noTime, text.invalid, "not-a-time", "NEW_STEP", "NEW_STATUS", "NEW_ORDER_STATE", "Long".repeat(600), "&lt;em&gt;Provider text&lt;/em&gt;"]) assert.ok(html.includes(expected), expected);
    for (const step of data.steps) assert.ok(html.includes(`data-step="${step.step}" data-step-status="${step.status}"`));
    const events = html.slice(html.indexOf("data-tracking-events"));
    const positions = ["Payment confirmed", "Documents requested", `title="DELIVERY"`, "Undated event", "Invalid date event"].map((label) => events.indexOf(label));
    assert.ok(positions.every((index, i) => index !== -1 && (!i || index > positions[i - 1])));
    assert.ok(events.includes(lang === "zh" ? ">服务交付</p>" : ">Service delivery</p>"));
    assert.ok(!html.includes("<em>Provider text</em>"));
    assert.match(render(h(OrderTrackingDetails, { data, stale: true })), new RegExp(text.cached));
  });

  test(`${lang}: unavailable, forbidden and empty details remain distinct from the overall status`, async () => {
    await i18n.changeLanguage(lang);
    for (const detailState of ["empty", "unavailable", "forbidden"]) {
      const html = render(h(OrderTrackingDetails, { data: { ...base, statusSource: "cached", detailState, orderStatusAt: "", steps: [
        { step: "Hidden stage", status: "DONE", history: [{ label: "Hidden event", description: "", date: "" }] },
      ] } }));
      for (const expected of [text.cached, text[detailState], text.checked, text.noTime]) assert.ok(html.includes(expected), expected);
      assert.ok(!html.includes("Hidden stage") && !html.includes("Hidden event") && !html.includes("data-tracking-events"));
    }
    const noEvents = render(h(OrderTrackingDetails, { data: { ...base, steps: [{ step: "VALIDATING", status: "TODO", history: [] }] } }));
    assert.ok(noEvents.includes(text.noEvents));
    assert.ok(!noEvents.includes("data-tracking-events"));
  });
}
