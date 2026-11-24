package handlers

import (
	"encoding/json"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ovh-buy/server/internal/app"
	"github.com/ovh-buy/server/internal/types"
)

func TestQueueOrderTrackingBindsExactAccountTaskAndHistory(t *testing.T) {
	st := &app.State{Accounts: []types.OVHAccount{{ID: "a", IsDefault: true}, {ID: "b"}}}
	st.Queue = []types.QueueItem{
		{ID: "shared", AccountID: "a", Status: "completed"},
		{ID: "shared", AccountID: "b", Status: "completed"},
		{ID: "duplicate-local-id", AccountID: "a", Status: "completed"},
		{ID: "removed", AccountID: "deleted", Status: "completed"},
		{ID: "other-only", AccountID: "a", Status: "completed"},
		{ID: "ambiguous", AccountID: "a", Status: "completed"},
		{ID: "unknown-owner", Status: "completed"},
		{ID: "invalid", AccountID: "a", Status: "completed"},
		{ID: "still-running", AccountID: "b", Status: "running"},
	}
	st.History = []types.PurchaseHistoryEntry{
		{ID: "history-a", TaskID: "shared", AccountID: "a", Status: "success", OrderID: "11"},
		{ID: "history-b", TaskID: "shared", AccountID: "b", Status: "success", OrderID: "22"},
		{ID: "failed", TaskID: "shared", AccountID: "a", Status: "failed", OrderID: "99"},
		{ID: "duplicate", TaskID: "duplicate-local-id", AccountID: "a", Status: "success", OrderID: "80"},
		{ID: "duplicate", TaskID: "other", AccountID: "b", Status: "success", OrderID: "81"},
		{ID: "orphan", TaskID: "removed", AccountID: "deleted", Status: "success", OrderID: "82"},
		{ID: "wrong", TaskID: "other-only", AccountID: "b", Status: "success", OrderID: "33"},
		{ID: "candidate-1", TaskID: "ambiguous", AccountID: "a", Status: "success", OrderID: "44"},
		{ID: "candidate-2", TaskID: "ambiguous", AccountID: "a", Status: "success", OrderID: "45"},
		{ID: "cannot-use", TaskID: "unknown-owner", AccountID: "b", Status: "success", OrderID: "55"},
		{ID: "bad-id", TaskID: "invalid", AccountID: "a", Status: "success", OrderID: "11/pay"},
		{ID: "not-complete", TaskID: "still-running", AccountID: "b", Status: "success", OrderID: "55"},
	}
	router := gin.New()
	router.GET("/queue", GetQueue(st))
	w := payRequest(router, "GET", "/queue", "")
	var rows []map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rows) != nil || len(rows) != len(st.Queue) {
		t.Fatal(w.Code, w.Body)
	}
	if rows[0]["purchaseHistoryId"] != "history-a" || rows[0]["orderId"] != "11" || rows[1]["purchaseHistoryId"] != "history-b" || rows[1]["orderId"] != "22" {
		t.Fatal("lost the exact account/task binding", rows[:2])
	}
	if rows[2]["orderId"] != "80" || rows[3]["orderId"] != "82" || rows[2]["purchaseHistoryId"] != nil || rows[3]["purchaseHistoryId"] != nil {
		t.Fatal("ambiguous or unavailable history still offered a tracking link", rows[2:4])
	}
	for _, r := range rows[4:] {
		if r["purchaseHistoryId"] != nil || r["orderId"] != nil {
			t.Fatal("non-unique, invalid or foreign order leaked into the queue", r)
		}
	}
}
