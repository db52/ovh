package handlers

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ovh-buy/server/internal/app"
	"github.com/ovh-buy/server/internal/types"
)

// queueOrderView 保留原任务字段，只附加内存中可唯一匹配的订单，不增加数据库列。
type queueOrderView struct {
	types.QueueItem
	OrderID           string `json:"orderId,omitempty"`
	PurchaseHistoryID string `json:"purchaseHistoryId,omitempty"`
	// PlanName 商品名(如 KS-LE-6),取自该任务所属账户的目录缓存;缓存里没有就留空,不为它请求 OVH
	PlanName string `json:"planName,omitempty"`
}

// GetQueue GET /api/queue
func GetQueue(state *app.State) gin.HandlerFunc {
	return func(c *gin.Context) {
		state.QueueMu.Lock()
		// 返回副本，确保为空时序列化为 [] 而不是 null
		cp := make([]types.QueueItem, len(state.Queue))
		copy(cp, state.Queue)
		state.QueueMu.Unlock()
		type queueKey struct{ accountID, taskID string }
		type orderRef struct{ orderID, historyID string }
		orders := make(map[queueKey]orderRef)
		historyIDs := make(map[string]int)
		state.HistoryMu.Lock()
		for _, h := range state.History {
			historyIDs[h.ID]++
			if h.Status != "success" || h.TaskID == "" {
				continue
			}
			key := queueKey{h.AccountID, h.TaskID}
			if _, seen := orders[key]; seen || !validTrackingOrderID(h.OrderID) {
				orders[key] = orderRef{} // 同账户下有多条成功历史时，不猜一条。
				continue
			}
			ref := orderRef{orderID: h.OrderID}
			if strings.TrimSpace(h.AccountID) != "" {
				ref.historyID = h.ID
			}
			orders[key] = ref
		}
		state.HistoryMu.Unlock()
		// 每个账户视角的目录缓存只取一次(GetBucket 返回副本)
		names := map[string]map[string]string{}
		planName := func(accountID, planCode string) string {
			key := state.ServerCacheKey(accountID)
			m, ok := names[key]
			if !ok {
				m = map[string]string{}
				if state.ServerCache != nil {
					plans, _ := state.ServerCache.GetBucket(key)
					for _, p := range plans {
						// 目录里的名字形如 "ADVANCE-1 | AMD EPYC 4244P",只取商品名
						n := strings.TrimSpace(strings.SplitN(p.Name, "|", 2)[0])
						if n != "" && n != p.PlanCode {
							m[p.PlanCode] = n
						}
					}
				}
				names[key] = m
			}
			return m[planCode]
		}
		view := make([]queueOrderView, len(cp))
		for i, item := range cp {
			view[i].QueueItem = item
			view[i].PlanName = planName(item.AccountID, item.PlanCode)
			if item.Status == "completed" {
				ref := orders[queueKey{item.AccountID, item.ID}]
				view[i].OrderID = ref.orderID
				if ref.historyID != "" && historyIDs[ref.historyID] == 1 {
					if _, exists := state.FindAccount(item.AccountID); exists {
						view[i].PurchaseHistoryID = ref.historyID
					}
				}
			}
		}
		c.JSON(http.StatusOK, view)
	}
}

// GetPurchaseHistory GET /api/purchase-history
func GetPurchaseHistory(state *app.State) gin.HandlerFunc {
	return func(c *gin.Context) {
		state.HistoryMu.Lock()
		cp := make([]types.PurchaseHistoryEntry, len(state.History))
		copy(cp, state.History)
		state.HistoryMu.Unlock()
		c.JSON(http.StatusOK, cp)
	}
}
