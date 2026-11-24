package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/ovh-buy/server/internal/app"
	"github.com/ovh-buy/server/internal/ovh"
	"github.com/ovh-buy/server/internal/types"
)

const historyPath = "/api/purchase-history/"
const accountOrdersPath = "/api/ovh/account/orders/"
const trackingSensitive = "private-upstream-fixture"

type trackingFixture struct {
	state  *app.State
	router *gin.Engine
	api    *OrderTracking
	mu     sync.Mutex
	calls  []string
}

// 两个虚构账户共用一个大区，不以 Host 代替凭据核验。任何请求都走内存 transport。
func newTrackingFixture(t *testing.T, reply func(account, path string, r *http.Request) (int, string)) *trackingFixture {
	t.Helper()
	state := &app.State{Accounts: []types.OVHAccount{
		{ID: "active", Endpoint: "ovh-eu", AppKey: "fixture-active", AppSecret: "fixture", ConsumerKey: "active", IsDefault: true},
		{ID: "owner", Endpoint: "ovh-eu", AppKey: "fixture-owner", AppSecret: "fixture", ConsumerKey: "owner"},
	}}
	// 不给 Factory 的按 ID 查找分支或旧配置任何机会兜底。
	state.OVH = ovh.NewFactory(nil, func(string) (types.OVHAccount, bool) {
		t.Error("tracking resolved credentials via factory lookup instead of the pinned account")
		return types.OVHAccount{}, false
	})
	f := &trackingFixture{state: state, router: gin.New(), api: NewOrderTracking(state)}
	f.router.GET(historyPath+":id/tracking", f.api.ByHistory)
	f.router.GET(accountOrdersPath+":order_id/payment", GetOrderPaymentOptions(state)) // 同层 Gin 通配符须兼容，不调用付款。
	f.router.GET(accountOrdersPath+":order_id/tracking", f.api.ByAccount)
	for _, a := range state.Accounts {
		base, err := state.OVH.ClientForAccount(a)
		if err != nil {
			t.Fatal(err)
		}
		base.Client.Transport = queuePreorderTransport(func(r *http.Request) (*http.Response, error) {
			path := strings.TrimPrefix(r.URL.Path, "/1.0")
			if r.Method != http.MethodGet || r.URL.RawQuery != "" || r.Header.Get("X-Ovh-Application") != a.AppKey {
				t.Error("unexpected method, query or account", r.Method, path)
			}
			code, body := 200, ""
			if path == "/auth/time" && r.Header.Get("X-Ovh-Consumer") == "" {
				// go-ovh 的无鉴权签名校时，不是第三条订单 API。
				body = strconv.FormatInt(time.Now().Unix(), 10)
			} else {
				if r.Header.Get("X-Ovh-Consumer") != a.ConsumerKey || r.Header.Get("X-Ovh-Signature") == "" {
					t.Error("order request was not signed by the owning account")
				}
				if _, ok := r.Context().Deadline(); !ok {
					t.Error("order request has no deadline")
				}
				f.mu.Lock()
				f.calls = append(f.calls, a.ID+" "+path)
				f.mu.Unlock()
				code, body = reply(a.ID, path, r)
			}
			return &http.Response{StatusCode: code, Header: http.Header{}, Request: r, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
	}
	return f
}

func (f *trackingFixture) wantCalls(t *testing.T, want ...string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Equal(f.calls, want) {
		t.Fatalf("signed order API calls = %v, want %v", f.calls, want)
	}
}

func trackingResponse(t *testing.T, w *httptest.ResponseRecorder) orderTracking {
	t.Helper()
	var out orderTracking
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Steps == nil || strings.Contains(w.Body.String(), trackingSensitive) {
		t.Fatalf("invalid tracking response (%d): %s", w.Code, w.Body)
	}
	if _, err := time.Parse(time.RFC3339Nano, out.CheckedAt); err != nil {
		t.Fatal("missing/invalid checkedAt", out.CheckedAt)
	}
	return out
}

func TestOrderTrackingHistoryPinsOwnerAndSharesOnlyThatOrdersCache(t *testing.T) {
	const order = "90123456789012345" // 数值超过 JS 精确整数范围，不能转为浮点或截短。
	f := newTrackingFixture(t, func(account, path string, _ *http.Request) (int, string) {
		switch {
		case strings.HasSuffix(path, "/status"):
			if account == "active" {
				return 200, `"notPaid"`
			}
			return 200, `"delivering"`
		case account == "owner" && strings.HasSuffix(path, "/followUp"):
			return 200, `[{"step":"VALIDATING","status":"DONE","history":[{"date":"2026-02-01T11:00:00Z","label":"ORDER_ACCEPTED","description":"Confirmed","extra":"private-upstream-fixture"}]},{"step":"NEW_STEP","status":"NEW_STATUS","serverOnly":"private-upstream-fixture"}]`
		}
		return 200, `[]`
	})
	h := types.PurchaseHistoryEntry{ID: "local-history", AccountID: "owner", TaskID: "task", OrderID: order, Status: "success", OrderStatus: "checking", OrderStatusAt: "2026-02-01T10:00:00Z"}
	f.state.History = []types.PurchaseHistoryEntry{h}
	get := func(path string) orderTracking { return trackingResponse(t, payRequest(f.router, "GET", path, "")) }
	out := get(historyPath + h.ID + "/tracking?account=active&orderId=123")
	if out.OrderID != order || out.OrderStatus != "delivering" || out.OrderStatusAt == "" || out.StatusSource != "live" || out.DetailState != "available" || len(out.Steps) != 2 {
		t.Fatalf("lost live tracking: %+v", out)
	}
	wantEvent := trackingEvent{Date: "2026-02-01T11:00:00Z", Label: "ORDER_ACCEPTED", Description: "Confirmed"}
	if out.Steps[0].Step != "VALIDATING" || out.Steps[0].Status != "DONE" || !slices.Equal(out.Steps[0].History, []trackingEvent{wantEvent}) || out.Steps[1].Step != "NEW_STEP" || out.Steps[1].Status != "NEW_STATUS" || out.Steps[1].History == nil {
		t.Fatalf("wrong/unknown upstream fields or null history: %+v", out.Steps)
	}
	// 账户订单和历史共享同一读取服务，但两个账户的同号订单绝不共享状态或详情。
	hit := get(accountOrdersPath + order + "/tracking?account=owner")
	if hit.StatusSource != "cached" || hit.OrderStatusAt != out.OrderStatusAt || hit.CheckedAt != out.CheckedAt || !reflect.DeepEqual(hit.Steps, out.Steps) {
		t.Fatal("cache response has incorrect provenance", hit)
	}
	other := get(accountOrdersPath + order + "/tracking?account=active")
	if other.OrderStatus != "notPaid" || other.StatusSource != "live" || other.DetailState != "empty" || len(other.Steps) != 0 {
		t.Fatal("same order number on another account reused the wrong response", other)
	}
	f.wantCalls(t, "owner /me/order/"+order+"/status", "owner /me/order/"+order+"/followUp", "active /me/order/"+order+"/status", "active /me/order/"+order+"/followUp")
	if !reflect.DeepEqual(f.state.History, []types.PurchaseHistoryEntry{h}) {
		t.Fatal("read operation mutated purchase history")
	}
	// 首次命中也必须重新验证本地记录/账户仍然存在。
	f.state.History = nil
	if w := payRequest(f.router, "GET", historyPath+h.ID+"/tracking", ""); w.Code != 404 {
		t.Fatal("removed history still trackable", w.Code)
	}
	f.state.Accounts = f.state.Accounts[:1]
	if w := payRequest(f.router, "GET", accountOrdersPath+order+"/tracking?account=owner", ""); w.Code != 404 {
		t.Fatal("removed account can still access cached order", w.Code)
	}
}

func TestOrderTrackingRemoteOrderNeedsNoLocalHistory(t *testing.T) {
	f := newTrackingFixture(t, func(account, path string, _ *http.Request) (int, string) {
		if account == "owner" && path == "/me/order/730/status" {
			return 200, `"checking"`
		}
		if account == "owner" && path == "/me/order/730/followUp" {
			return 200, `[]`
		}
		return 404, `{"message":"private-upstream-fixture"}`
	})
	if out := trackingResponse(t, payRequest(f.router, "GET", accountOrdersPath+"730/tracking?account=owner", "")); out.OrderID != "730" || out.StatusSource != "live" || out.DetailState != "empty" {
		t.Fatal(out)
	}
	w := payRequest(f.router, "GET", accountOrdersPath+"730/tracking?account=active", "")
	if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), trackingSensitive) {
		t.Fatal("unowned order must not reuse another account", w.Code, w.Body)
	}
	f.wantCalls(t, "owner /me/order/730/status", "owner /me/order/730/followUp", "active /me/order/730/status", "active /me/order/730/followUp")
}

func TestOrderTrackingRejectsMissingOwnersAndMalformedOrderIDs(t *testing.T) {
	f := newTrackingFixture(t, func(_, path string, _ *http.Request) (int, string) {
		t.Errorf("invalid request reached OVH: %s", path)
		return 500, ""
	})
	for _, query := range []string{"", "?account=", "?account=%20", "?account=gone", "?account=owner&account=active"} {
		w := payRequest(f.router, "GET", accountOrdersPath+"10/tracking"+query, "")
		if w.Code < 400 || w.Code >= 500 {
			t.Fatalf("unbound account accepted: %q: %d", query, w.Code)
		}
	}
	for _, order := range []string{"", "0", "010", "+10", "-1", "1.5", " 10", "10 ", "10/pay", "1e3", "9223372036854775808"} {
		f.state.History = []types.PurchaseHistoryEntry{{ID: "h", AccountID: "owner", Status: "success", OrderID: order}}
		for _, path := range []string{historyPath + "h/tracking", accountOrdersPath + url.PathEscape(order) + "/tracking?account=owner"} {
			w := payRequest(f.router, "GET", path, "")
			if w.Code < 400 || w.Code >= 500 {
				t.Fatalf("invalid order %q: %d on %q", order, w.Code, path)
			}
		}
	}
	for _, h := range []types.PurchaseHistoryEntry{
		{ID: "h", AccountID: "", Status: "success", OrderID: "10"},
		{ID: "h", AccountID: " ", Status: "success", OrderID: "10"},
		{ID: "h", AccountID: "deleted", Status: "success", OrderID: "10"},
		{ID: "h", AccountID: "owner", Status: "failed", OrderID: "10"},
		{ID: "h", AccountID: "owner", Status: "uncertain", OrderID: "10"},
	} {
		f.state.History = []types.PurchaseHistoryEntry{h}
		w := payRequest(f.router, "GET", historyPath+"h/tracking?account=active&orderId=10", "")
		if w.Code != http.StatusConflict {
			t.Fatal("history has no trackable order", h, w.Code)
		}
	}
	for _, h := range [][]types.PurchaseHistoryEntry{nil, {{ID: "h", AccountID: "owner", Status: "success", OrderID: "10"}, {ID: "h", AccountID: "active", Status: "success", OrderID: "20"}}} {
		f.state.History = h
		if w := payRequest(f.router, "GET", historyPath+"h/tracking", ""); w.Code != http.StatusNotFound {
			t.Fatal("missing/ambiguous history accepted", w.Code)
		}
	}
	f.wantCalls(t)
}

func TestOrderTrackingDegradesWithoutLeakingErrorsOrBorrowingStatus(t *testing.T) {
	const oldAt = "2026-01-01T00:00:00Z"
	for _, tc := range []struct {
		name                               string
		statusCode, detailCode             int
		old, detail, wantSource, wantState string
	}{
		{"empty", 200, 200, "checking", `[]`, "live", "empty"},
		{"forbidden", 200, 403, "", "", "live", "forbidden"},
		{"timeout", 200, 504, "", "", "live", "unavailable"},
		{"persisted", 500, 403, "checking", "", "cached", "forbidden"},
		{"unknown", 403, 500, "", "", "unknown", "unavailable"},
		{"details_only", 500, 200, "", `[{"step":"AVAILABLE","status":"DONE","history":[]}]`, "unknown", "available"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTrackingFixture(t, func(_ string, path string, _ *http.Request) (int, string) {
				code, body := tc.statusCode, `"delivering"`
				if strings.HasSuffix(path, "/followUp") {
					code, body = tc.detailCode, tc.detail
				}
				if code >= 400 {
					body = `{"message":"private-upstream-fixture"}`
				}
				return code, body
			})
			f.state.History = []types.PurchaseHistoryEntry{
				{ID: "another", AccountID: "active", OrderID: "11", Status: "success", OrderStatus: "delivered"},
				{ID: "h", AccountID: "owner", OrderID: "11", Status: "success", OrderStatus: tc.old, OrderStatusAt: oldAt},
			}
			out := trackingResponse(t, payRequest(f.router, "GET", historyPath+"h/tracking", ""))
			if out.DetailState != tc.wantState || out.StatusSource != tc.wantSource || (len(out.Steps) != 0) != (tc.wantState == "available") {
				t.Fatalf("incorrect partial result: %+v", out)
			}
			switch tc.wantSource {
			case "live":
				if out.OrderStatus != "delivering" || out.OrderStatusAt == oldAt || out.OrderStatusAt == "" {
					t.Fatal(out)
				}
			case "cached":
				if out.OrderStatus != tc.old || out.OrderStatusAt != oldAt {
					t.Fatal(out)
				}
			case "unknown":
				if out.OrderStatus != "" || out.OrderStatusAt != "" {
					t.Fatal(out)
				}
			}
			remote := trackingResponse(t, payRequest(f.router, "GET", accountOrdersPath+"11/tracking?account=owner", ""))
			if tc.statusCode != 200 && (remote.OrderStatus != "" || remote.StatusSource != "unknown") {
				t.Fatal("account endpoint borrowed a history-only fallback", remote)
			}
			f.wantCalls(t, "owner /me/order/11/status", "owner /me/order/11/followUp")
		})
	}
}

func TestOrderTrackingSlowOrderIsCancellableAndDoesNotBlockOtherOrders(t *testing.T) {
	started := make(chan struct{})
	f := newTrackingFixture(t, func(_ string, path string, r *http.Request) (int, string) {
		if path == "/me/order/21/followUp" {
			close(started)
			<-r.Context().Done()
			return 504, `{"message":"private-upstream-fixture"}`
		}
		if strings.HasSuffix(path, "/status") {
			return 200, `"notPaid"`
		}
		return 200, `[]`
	})
	f.state.History = []types.PurchaseHistoryEntry{{ID: "slow", AccountID: "owner", OrderID: "21", Status: "success"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slow := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		f.router.ServeHTTP(w, httptest.NewRequestWithContext(ctx, "GET", historyPath+"slow/tracking", nil))
		slow <- w
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("slow order did not reach followUp")
	}
	if !f.state.HistoryMu.TryLock() {
		t.Fatal("tracking held HistoryMu during network request")
	}
	f.state.HistoryMu.Unlock()
	fast := make(chan *httptest.ResponseRecorder, 1)
	go func() { fast <- payRequest(f.router, "GET", accountOrdersPath+"22/tracking?account=owner", "") }()
	select {
	case w := <-fast:
		if trackingResponse(t, w).DetailState != "empty" {
			t.Fatal(w.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unrelated order blocked on the slow one")
	}
	cancel()
	select {
	case w := <-slow:
		out := trackingResponse(t, w)
		if out.OrderStatus != "notPaid" || out.DetailState != "unavailable" {
			t.Fatal(out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled followUp did not return")
	}
	f.wantCalls(t, "owner /me/order/21/status", "owner /me/order/21/followUp", "owner /me/order/22/status", "owner /me/order/22/followUp")
}

func TestOrderTrackingPinsCredentialsThroughoutBothReads(t *testing.T) {
	var f *trackingFixture
	f = newTrackingFixture(t, func(_ string, path string, _ *http.Request) (int, string) {
		if strings.HasSuffix(path, "/status") {
			// 模拟两个 GET 中间更新账户；不能换用新凭据、当前默认或其它缓存。
			f.state.AccountsMu.Lock()
			f.state.Accounts[1].ConsumerKey = ""
			f.state.AccountsMu.Unlock()
			return 200, `"delivering"`
		}
		return 200, `[{"step":"DELIVERING","status":"DOING"}]`
	})
	path := accountOrdersPath + "710/tracking?account=owner"
	if got := trackingResponse(t, payRequest(f.router, "GET", path, "")); got.DetailState != "available" || got.OrderStatus != "delivering" {
		t.Fatal("the reads did not keep the original owner's credentials", got)
	}
	if got := trackingResponse(t, payRequest(f.router, "GET", path, "")); got.DetailState != "unavailable" || got.StatusSource != "unknown" {
		t.Fatal("invalidated credentials exposed prior owner data", got)
	}
	f.wantCalls(t, "owner /me/order/710/status", "owner /me/order/710/followUp")
}

func TestOrderTrackingCacheExpiresAndIsBounded(t *testing.T) {
	f := newTrackingFixture(t, func(_ string, path string, _ *http.Request) (int, string) {
		if strings.HasSuffix(path, "/status") {
			return 200, `"notPaid"`
		}
		return 200, `[]`
	})
	get := func(id int) orderTracking {
		return trackingResponse(t, payRequest(f.router, "GET", fmt.Sprintf("%s%d/tracking?account=owner", accountOrdersPath, id), ""))
	}
	get(1)
	for _, entry := range f.api.cache.entries {
		entry.expires = time.Now().Add(-time.Second)
	}
	if get(1).StatusSource != "live" {
		t.Fatal("expired observation served as current")
	}
	for i := 2; i <= trackingLimit+1; i++ {
		get(i)
	}
	if len(f.api.cache.entries) != trackingLimit || get(1).StatusSource != "live" {
		t.Fatal("cache grew unbounded or failed to evict oldest entry")
	}
}
