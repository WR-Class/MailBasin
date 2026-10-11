package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/store"
)

type failingMailboxCreator struct {
	calls atomic.Int32
}

func (c *failingMailboxCreator) Create(context.Context, string, string, string, string) (domain.Mailbox, error) {
	c.calls.Add(1)
	return domain.Mailbox{}, errors.New("模拟创建失败")
}

func TestRecordManualSuccessIncludesMailboxLabel(t *testing.T) {
	service := NewService(nil, nil)
	state := service.RecordManualSuccess("account_1", domain.Mailbox{ID: "mailbox_1", Email: "sample@icloud.com", Label: "project_12"})

	if len(state.Events) != 1 {
		t.Fatalf("事件数量 = %d，期望为 1", len(state.Events))
	}
	if state.Events[0].Label != "project_12" {
		t.Fatalf("事件标签 = %q，期望为 %q", state.Events[0].Label, "project_12")
	}
}

func TestSchedulerStatePersistsInSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	database, err := store.Open(path)
	if err != nil {
		t.Fatalf("创建 SQLite 数据库失败：%v", err)
	}
	service := NewService(database, nil)
	service.RecordManualSuccess("account_1", domain.Mailbox{ID: "mailbox_1", Email: "persist@icloud.com", Label: "persist_1"})
	if err := database.Close(); err != nil {
		t.Fatalf("关闭数据库失败：%v", err)
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatalf("重新打开 SQLite 数据库失败：%v", err)
	}
	defer reopened.Close()
	restored := NewService(reopened, nil).Snapshot()
	if restored.Success != 1 || len(restored.Events) != 1 || restored.Events[0].Email != "persist@icloud.com" {
		t.Fatalf("调度状态恢复不正确：%+v", restored)
	}
}

func TestRecordManualFailureIncludesTrimmedLabel(t *testing.T) {
	service := NewService(nil, nil)
	state := service.RecordManualFailure("account_1", "  project  ", errors.New("创建失败"))

	if len(state.Events) != 1 {
		t.Fatalf("事件数量 = %d，期望为 1", len(state.Events))
	}
	if state.Events[0].Label != "project" {
		t.Fatalf("事件标签 = %q，期望为 %q", state.Events[0].Label, "project")
	}
}

func TestSchedulerStopsImmediatelyAfterCreateFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	database, err := store.Open(path)
	if err != nil {
		t.Fatalf("创建 SQLite 数据库失败：%v", err)
	}
	defer database.Close()

	firstSession, err := database.SaveICloudSession(domain.ICloudSession{AppleID: "first@example.com"})
	if err != nil {
		t.Fatalf("保存第一个测试登录态失败：%v", err)
	}
	secondSession, err := database.SaveICloudSession(domain.ICloudSession{AppleID: "second@example.com"})
	if err != nil {
		t.Fatalf("保存第二个测试登录态失败：%v", err)
	}
	creator := &failingMailboxCreator{}
	service := NewService(database, creator)
	_, err = service.Start(context.Background(), Config{
		AccountIDs:         []string{firstSession.AccountID, secondSession.AccountID},
		IntervalMinSeconds: 60,
		IntervalMaxSeconds: 60,
	})
	if err != nil {
		t.Fatalf("启动自动创建失败：%v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	var state State
	for time.Now().Before(deadline) {
		state = service.Snapshot()
		if !state.Running && state.Failed == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if state.Running || state.Status != "stopped" {
		t.Fatalf("创建失败后调度器仍未停止：%+v", state)
	}
	if creator.calls.Load() != 1 {
		t.Fatalf("创建调用次数 = %d，期望首次失败后立即停止", creator.calls.Load())
	}
	if state.Failed != 1 || state.LastError != "模拟创建失败" {
		t.Fatalf("失败状态记录不正确：%+v", state)
	}
	if !state.NextRunAt.IsZero() {
		t.Fatalf("创建失败后仍保留下一轮时间：%s", state.NextRunAt)
	}
	if len(state.Events) < 3 {
		t.Fatalf("调度事件数量不足：%+v", state.Events)
	}
	last := state.Events[len(state.Events)-1]
	if last.Type != "stopped" || last.Message != "创建失败，自动创建已停止" {
		t.Fatalf("停止事件不正确：%+v", last)
	}
}
