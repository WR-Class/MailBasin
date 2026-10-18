//go:build measure

package store

import (
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
)

// 这个文件只在 go test -tags measure 时编译。它用活动库的副本做真实规模的前后对照，
// 所以不进常规测试：常规测试不该依赖 data/app.db 存在。
//
// 关键是插入**新**邮件而不是重放已存在的：回填的代价全在「未命中」——每封新邮件
// 都要扫完这个邮箱已有的全部邮件才能断定它不存在，命中反而一扫就停。
// 所以测重放会低估一两个数量级，只有测插入才对应线上那 21 分钟。
func TestMeasureBackfillDedupeOnLiveCopy(t *testing.T) {
	dbPath := os.Getenv("IPM_MEASURE_DB")
	mailboxID := os.Getenv("IPM_MEASURE_MAILBOX")
	limit := 200
	if raw := os.Getenv("IPM_MEASURE_LIMIT"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if dbPath == "" || mailboxID == "" {
		t.Skip("没有提供 IPM_MEASURE_DB / IPM_MEASURE_MAILBOX")
	}
	state, err := Open(dbPath)
	if err != nil {
		t.Fatalf("打开测量库失败：%v", err)
	}
	defer state.Close()

	var stored int
	if err := state.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE json_extract(data_json, '$.mailbox_id') = ?`, mailboxID).Scan(&stored); err != nil {
		t.Fatalf("统计已有邮件失败：%v", err)
	}
	base := time.Now().Add(-30 * 24 * time.Hour)
	batch := make([]MailboxSyncMessage, 0, limit)
	for index := 0; index < limit; index++ {
		batch = append(batch, MailboxSyncMessage{
			RemoteID:    fmt.Sprintf("measure:%d:%d", base.UnixNano(), index),
			RemoteIDs:   []string{fmt.Sprintf("measure:%d:%d", base.UnixNano(), index)},
			Source:      "imap",
			Subject:     fmt.Sprintf("测量邮件 %d", index),
			From:        "measure@imap.test",
			Body:        "验证码 123456",
			HTMLBody:    "<p>验证码 123456</p>",
			ContentType: "text/html",
			ReceivedAt:  base.Add(time.Duration(index) * time.Minute),
		})
	}
	started := time.Now()
	created, err := state.ApplyMailboxSyncBatch([]MailboxSyncUpdate{{MailboxID: mailboxID, Messages: batch}})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("回填失败：%v", err)
	}
	fmt.Printf("MEASURE mailbox=%s stored_before=%d inserted=%d created=%d elapsed=%s per_message=%s\n",
		mailboxID, stored, len(batch), created, elapsed, elapsed/time.Duration(len(batch)))
	if created != len(batch) {
		t.Fatalf("期望插入 %d 封，实际 %d 封", len(batch), created)
	}
}
