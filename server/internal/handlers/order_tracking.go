package handlers

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	ovhsdk "github.com/ovh/go-ovh/ovh"

	"github.com/ovh-buy/server/internal/app"
	"github.com/ovh-buy/server/internal/ovh"
	"github.com/ovh-buy/server/internal/types"
)

type trackingEvent struct {
	Date        string `json:"date"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

type trackingStep struct {
	Step    string          `json:"step"`
	Status  string          `json:"status"`
	History []trackingEvent `json:"history"`
}

type orderTracking struct {
	OrderID       string         `json:"orderId"`
	OrderStatus   string         `json:"orderStatus,omitempty"`
	OrderStatusAt string         `json:"orderStatusAt,omitempty"`
	StatusSource  string         `json:"statusSource"`
	DetailState   string         `json:"detailState"`
	Steps         []trackingStep `json:"steps"`
	CheckedAt     string         `json:"checkedAt"`
	notFound      bool           // 只有两个端点均明确返回 404 时，账户入口才报订单不存在。
}

// OVH 的 orderId 是正的 long。只接受原始十进制表示，不修复或拼接不合法的历史值。
func validTrackingOrderID(id string) bool {
	if id == "" || id[0] < '1' || id[0] > '9' {
		return false
	}
	_, err := strconv.ParseUint(id, 10, 63)
	return err == nil
}

func trackingHistory(state *app.State, id string) (types.PurchaseHistoryEntry, bool) {
	state.HistoryMu.Lock()
	defer state.HistoryMu.Unlock()
	var match types.PurchaseHistoryEntry
	for _, h := range state.History {
		if h.ID == id {
			if match.ID != "" { // 两条记录不能共用一个追踪入口。
				return types.PurchaseHistoryEntry{}, false
			}
			match = h
		}
	}
	return match, match.ID != ""
}

func unavailableTracking(orderID string) orderTracking {
	return orderTracking{OrderID: orderID, StatusSource: "unknown", DetailState: "unavailable",
		Steps: []trackingStep{}, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano)}
}

// OrderTracking 为两个只读入口共享缓存；没有后台任务或持久化写入。
type OrderTracking struct {
	state *app.State
	cache trackingCache
}

func NewOrderTracking(state *app.State) *OrderTracking {
	return &OrderTracking{state: state, cache: trackingCache{entries: make(map[trackingKey]*trackingEntry)}}
}

// ByHistory 只从本地记录取账户和订单号，不理会客户端的 active account 或订单号。
func (t *OrderTracking) ByHistory(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	h, ok := trackingHistory(t.state, c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "找不到唯一的抢购历史记录"})
		return
	}
	if h.Status != "success" || !validTrackingOrderID(h.OrderID) {
		c.JSON(http.StatusConflict, gin.H{"error": "这条历史没有可追踪的成功订单"})
		return
	}
	// FindAccount("") 和 ClientFor 都有默认账户回退。非空 ID 精确查找后只使用这个账户快照。
	if strings.TrimSpace(h.AccountID) == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "历史记录没有下单账户，无法查询订单"})
		return
	}
	account, ok := t.state.FindAccount(h.AccountID)
	if !ok {
		c.JSON(http.StatusConflict, gin.H{"error": "原下单账户不存在，无法查询订单"})
		return
	}
	result := t.read(c.Request.Context(), account, h.OrderID)
	// 缓存只存 OVH 观察值；本地状态只用于该历史响应，不反写，也不推算进度。
	if result.OrderStatus == "" && h.OrderStatus != "" {
		result.OrderStatus, result.OrderStatusAt, result.StatusSource = h.OrderStatus, h.OrderStatusAt, "cached"
	}
	c.JSON(http.StatusOK, result)
}

// ByAccount 查询账户订单列表中的原始 OVH 订单，不要求它来自本地抢购。归属由该账户签名的
// /me/order 读取验证；既不查其它账户，也不从同号的本地历史借状态。
func (t *OrderTracking) ByAccount(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	orderID := c.Param("order_id")
	ids := c.QueryArray("account")
	if !validTrackingOrderID(orderID) || len(ids) != 1 || strings.TrimSpace(ids[0]) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请指定明确的账户和有效订单号"})
		return
	}
	account, ok := t.state.FindAccount(ids[0])
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "指定的下单账户不存在"})
		return
	}
	result := t.read(c.Request.Context(), account, orderID)
	if result.notFound {
		c.JSON(http.StatusNotFound, gin.H{"error": "该账户下找不到这笔订单"})
		return
	}
	c.JSON(http.StatusOK, result)
}

func (t *OrderTracking) read(parent context.Context, account types.OVHAccount, orderID string) orderTracking {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	base, err := t.state.OVH.ClientForAccount(account)
	if err != nil || ctx.Err() != nil {
		return unavailableTracking(orderID)
	}
	result, hit := t.cache.read(ctx, trackingKey{base, orderID})
	if hit && result.OrderStatus != "" {
		result.StatusSource = "cached"
	}
	return result
}

// Factory 为每个账户、每版连接配置返回独立的 base。把它作为缓存命名空间，
// 即使同一账户 ID 替换了凭据或站点，也不会复用旧权限下的详情；不在 base 上发请求。
type trackingKey struct {
	owner   *ovhsdk.Client
	orderID string
}

func fetchOrderTracking(ctx context.Context, key trackingKey) orderTracking {
	out := unavailableTracking(key.orderID)
	client, err := ovh.IsolateClient(key.owner)
	if err != nil || ctx.Err() != nil {
		return out
	}
	// SDK 首次为签名校时的 GET /auth/time 使用自己的 context，也必须有超时。
	deadline, ok := ctx.Deadline()
	remaining := time.Until(deadline)
	if !ok || remaining <= 0 {
		return out
	}
	client.Timeout = remaining
	var status string
	statusErr := client.GetWithContext(ctx, "/me/order/"+key.orderID+"/status", &status)
	if statusErr == nil && status != "" {
		out.OrderStatus, out.OrderStatusAt, out.StatusSource = status, time.Now().UTC().Format(time.RFC3339Nano), "live"
	}
	if remaining := time.Until(deadline); ctx.Err() == nil && remaining > 0 {
		client.Timeout = remaining
		var steps []trackingStep
		err := client.GetWithContext(ctx, "/me/order/"+key.orderID+"/followUp", &steps)
		out.notFound = ovhIsNotFound(statusErr) && ovhIsNotFound(err)
		switch {
		case err == nil && len(steps) == 0:
			out.DetailState = "empty"
		case err == nil:
			for i := range steps {
				if steps[i].History == nil {
					steps[i].History = []trackingEvent{}
				}
			}
			out.Steps, out.DetailState = steps, "available"
		case ovhAPICode(err) == http.StatusForbidden || ovhAPICode(err) == http.StatusUnauthorized:
			out.DetailState = "forbidden"
		}
	}
	out.CheckedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return out
}

const (
	trackingTTL   = 30 * time.Second
	trackingLimit = 128
)

type trackingEntry struct {
	done    chan struct{}
	expires time.Time // 为零时请求在途；result 在 close(done) 后只读。
	result  orderTracking
}

type trackingCache struct {
	mu      sync.Mutex
	entries map[trackingKey]*trackingEntry
}

// 同一单合并请求，不同订单独立。锁仅保护索引，绝不跨网络调用或响应编码。
func (cache *trackingCache) read(ctx context.Context, key trackingKey) (orderTracking, bool) {
	cache.mu.Lock()
	if e := cache.entries[key]; e != nil && (e.expires.IsZero() || time.Now().Before(e.expires)) {
		cache.mu.Unlock()
		select {
		case <-e.done:
			return e.result, true
		case <-ctx.Done():
			return unavailableTracking(key.orderID), false
		}
	}
	delete(cache.entries, key)
	if len(cache.entries) >= trackingLimit {
		var oldest trackingKey
		var expires time.Time
		for k, e := range cache.entries {
			if !e.expires.IsZero() && (expires.IsZero() || e.expires.Before(expires)) {
				oldest, expires = k, e.expires
			}
		}
		if expires.IsZero() { // 全部在途，不阻塞别的订单，也不无限扩充缓存。
			cache.mu.Unlock()
			return unavailableTracking(key.orderID), false
		}
		delete(cache.entries, oldest)
	}
	e := &trackingEntry{done: make(chan struct{})}
	cache.entries[key] = e
	cache.mu.Unlock()

	out := fetchOrderTracking(ctx, key)
	cache.mu.Lock()
	e.result, e.expires = out, time.Now().Add(trackingTTL)
	close(e.done)
	if ctx.Err() != nil { // 不用一个请求中断的结果污染后续请求。
		delete(cache.entries, key)
	}
	cache.mu.Unlock()
	return out, false
}
