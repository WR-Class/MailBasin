package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"icloud-privacy-mail-v2/internal/config"
	mailboxservice "icloud-privacy-mail-v2/internal/mailbox"
	"icloud-privacy-mail-v2/internal/store"
)

// 这一组测试补的是一条从来没被测过的路径：「任务在跑就别更新」。
//
// 为什么值得单独立测试：更新会 Stop-Process -Force，没有优雅关闭。撞上正在写库的任务
// 只能指望 SQLite 自己从 WAL 里恢复，不该靠设计去赌。脚本看不到任务状态，服务看得到，
// 所以这道闸门是唯一的一道——而它此前零覆盖。

// fakeUpdateJobStatus 只回答守卫问的那两个问题。
//
// 真任务状态要启动一次真邮件任务才能造出来，那要连 IMAP、要动真数据，测试里不碰。
// 需要说清的是：这个假实现只证明判断与接线，不证明真实任务的生命周期——那个生命周期
// 由 mailbox 包自己的测试覆盖。
type fakeUpdateJobStatus struct {
	syncStatus    string
	cleanupStatus string
}

func (fake *fakeUpdateJobStatus) ExistingMailboxMessageSyncStatus() mailboxservice.ExistingMailboxMessageSyncJob {
	return mailboxservice.ExistingMailboxMessageSyncJob{Status: fake.syncStatus}
}

func (fake *fakeUpdateJobStatus) AppleMailCleanupStatus() mailboxservice.AppleMailCleanupJob {
	return mailboxservice.AppleMailCleanupJob{Status: fake.cleanupStatus}
}

// countingUpdateTrigger 替掉「真的启动一次更新」这个副作用。
//
// 成功路径不替掉的话，测一次「允许更新」就等于真的让计划任务去构建并替换正在跑的服务。
type countingUpdateTrigger struct {
	calls int
	err   error
}

func (trigger *countingUpdateTrigger) fire() error {
	trigger.calls++
	return trigger.err
}

type updateErrorBody struct {
	Success bool   `json:"success"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// TestUpdateRefusalCoversEveryJobStatus 逐条钉住拒绝判断。
//
// 这些状态来自 mailbox 服务：idle/queued/running/interrupted/partial/completed/cancelled。
// 边界只有一处——running 和 queued 算「占着机器」，其余都算已经停下——但把每个取值都写进
// 表里，是因为两侧的错法都很贵：把已结束的任务当成在跑，按钮就永久锁死；把崩掉的任务当成
// 在跑，也一样永久锁死。两者都只差一个字符串。
func TestUpdateRefusalCoversEveryJobStatus(t *testing.T) {
	cases := []struct {
		name          string
		enabled       bool
		syncStatus    string
		cleanupStatus string
		updateRunning bool
		wantRefused   bool
		wantCode      string
		wantStatus    int
	}{
		{name: "配置里关掉更新时拒绝", enabled: false, wantRefused: true, wantCode: "update_disabled", wantStatus: http.StatusBadRequest},

		{name: "同步排队时拒绝", enabled: true, syncStatus: "queued", wantRefused: true, wantCode: "update_busy_sync", wantStatus: http.StatusConflict},
		{name: "同步进行中时拒绝", enabled: true, syncStatus: "running", wantRefused: true, wantCode: "update_busy_sync", wantStatus: http.StatusConflict},
		{name: "同步已完成时放行", enabled: true, syncStatus: "completed"},
		{name: "同步中断时放行", enabled: true, syncStatus: "interrupted"},
		{name: "同步部分失败时放行", enabled: true, syncStatus: "partial"},
		{name: "同步已取消时放行", enabled: true, syncStatus: "cancelled"},
		{name: "同步空闲时放行", enabled: true, syncStatus: "idle"},

		{name: "清理排队时拒绝", enabled: true, cleanupStatus: "queued", wantRefused: true, wantCode: "update_busy_cleanup", wantStatus: http.StatusConflict},
		{name: "清理进行中时拒绝", enabled: true, cleanupStatus: "running", wantRefused: true, wantCode: "update_busy_cleanup", wantStatus: http.StatusConflict},
		{name: "清理已完成时放行", enabled: true, cleanupStatus: "completed"},
		{name: "清理中断时放行", enabled: true, cleanupStatus: "interrupted"},
		{name: "清理部分失败时放行", enabled: true, cleanupStatus: "partial"},
		{name: "清理空闲时放行", enabled: true, cleanupStatus: "idle"},

		{name: "已有更新在跑时拒绝", enabled: true, updateRunning: true, wantRefused: true, wantCode: "update_already_running", wantStatus: http.StatusConflict},
		{name: "什么都没有时放行", enabled: true},

		// 优先级：几个条件同时命中时报哪一个，此前没人验过。
		{name: "关闭优先于任务在跑", enabled: false, syncStatus: "running", cleanupStatus: "running", updateRunning: true, wantRefused: true, wantCode: "update_disabled", wantStatus: http.StatusBadRequest},
		{name: "同步优先于清理", enabled: true, syncStatus: "running", cleanupStatus: "running", wantRefused: true, wantCode: "update_busy_sync", wantStatus: http.StatusConflict},
		{name: "任务优先于更新在跑", enabled: true, syncStatus: "running", updateRunning: true, wantRefused: true, wantCode: "update_busy_sync", wantStatus: http.StatusConflict},
		{name: "清理优先于更新在跑", enabled: true, cleanupStatus: "running", updateRunning: true, wantRefused: true, wantCode: "update_busy_cleanup", wantStatus: http.StatusConflict},

		// 拼错的状态不能把按钮永久锁死：宁可放行（脚本自己还会再查一次），也不要卡死。
		{name: "未知状态不阻塞", enabled: true, syncStatus: "runnning", cleanupStatus: "nonsense"},
		{name: "空状态不阻塞", enabled: true, syncStatus: "", cleanupStatus: ""},
		{name: "大小写与空格不影响判断", enabled: true, syncStatus: "  RUNNING  ", wantRefused: true, wantCode: "update_busy_sync", wantStatus: http.StatusConflict},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var run *updateRunStatus
			if testCase.updateRunning {
				run = &updateRunStatus{Running: true}
			}
			status, code, _, refused := updateRefusal(
				testCase.enabled,
				mailboxservice.ExistingMailboxMessageSyncJob{Status: testCase.syncStatus},
				mailboxservice.AppleMailCleanupJob{Status: testCase.cleanupStatus},
				run,
			)
			if refused != testCase.wantRefused {
				t.Fatalf("拒绝与否不正确：期望 %v，实际 %v（状态 %q / 清理 %q）",
					testCase.wantRefused, refused, testCase.syncStatus, testCase.cleanupStatus)
			}
			if !refused {
				return
			}
			if code != testCase.wantCode {
				t.Fatalf("拒绝原因不正确：期望 %q，实际 %q", testCase.wantCode, code)
			}
			if status != testCase.wantStatus {
				t.Fatalf("HTTP 状态不正确：期望 %d，实际 %d", testCase.wantStatus, status)
			}
		})
	}
}

// 守卫默认必须接到真实服务上。接成 nil 的话，生产里一按更新就 panic——而那是唯一一条
// 不能让用户看到崩溃的路径。
func TestUpdateGuardDefaultsToRealMailboxService(t *testing.T) {
	server, _ := newUpdateGuardTestServer(t)
	if server.updateJobs == nil {
		t.Fatal("守卫的任务状态来源不能为空，否则一按更新就 panic")
	}
	if job := server.updateJobs.ExistingMailboxMessageSyncStatus(); job.Status != "idle" {
		t.Fatalf("刚建好的服务，同步任务应当是 idle，实际 %q", job.Status)
	}
	if job := server.updateJobs.AppleMailCleanupStatus(); job.Status != "idle" {
		t.Fatalf("刚建好的服务，清理任务应当是 idle，实际 %q", job.Status)
	}
}

// 接线：handler 真的去问了守卫，并且拒绝时绝不启动更新器。
//
// 反向断言（trigger 一次都没被调用）才是这条测试的重点：一道「拒绝了请求但照样把活干了」
// 的闸门，能通过所有只测正向的测试。
func TestUpdateApplyRefusesWhileBackgroundJobRuns(t *testing.T) {
	cases := []struct {
		name       string
		syncStatus string
		cleanup    string
		wantCode   string
	}{
		{name: "批量同步在跑", syncStatus: "running", cleanup: "idle", wantCode: "update_busy_sync"},
		{name: "批量同步在排队", syncStatus: "queued", cleanup: "idle", wantCode: "update_busy_sync"},
		{name: "Apple 邮件清理在跑", syncStatus: "idle", cleanup: "running", wantCode: "update_busy_cleanup"},
		{name: "Apple 邮件清理在排队", syncStatus: "idle", cleanup: "queued", wantCode: "update_busy_cleanup"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server, _ := newUpdateGuardTestServer(t)
			server.updateJobs = &fakeUpdateJobStatus{syncStatus: testCase.syncStatus, cleanupStatus: testCase.cleanup}
			trigger := &countingUpdateTrigger{}
			server.updateTrigger = trigger.fire

			recorder, body := callUpdateApply(server)

			if recorder.Code != http.StatusConflict {
				t.Fatalf("任务在跑时应以 409 拒绝，实际 %d %s", recorder.Code, recorder.Body.String())
			}
			if body.Code != testCase.wantCode {
				t.Fatalf("拒绝原因不正确：期望 %q，实际 %q", testCase.wantCode, body.Code)
			}
			if trigger.calls != 0 {
				t.Fatalf("拒绝了请求却仍然启动了更新器（调用了 %d 次）——这道闸门等于没有", trigger.calls)
			}
		})
	}
}

// 配置里关掉更新时拒绝。这一条走的是真配置，不是假的。
func TestUpdateApplyRefusesWhenDisabledInConfig(t *testing.T) {
	server, _ := newUpdateGuardTestServer(t)
	server.cfg.UpdateEnabled = false
	trigger := &countingUpdateTrigger{}
	server.updateTrigger = trigger.fire

	recorder, body := callUpdateApply(server)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("关掉更新后应以 400 拒绝，实际 %d", recorder.Code)
	}
	if body.Code != "update_disabled" {
		t.Fatalf("拒绝原因不正确：%q", body.Code)
	}
	if trigger.calls != 0 {
		t.Fatalf("关掉更新却仍然启动了更新器（%d 次）", trigger.calls)
	}
}

// 「已经有一个更新在跑」这条走真实文件与真实探活：真写一份状态文件，让 handler 自己读、
// 自己核实 PID。这里不用假实现，因为读文件加探活正是要验的那段代码。
func TestUpdateApplyRefusesWhenAnotherUpdateIsRunning(t *testing.T) {
	server, cfg := newUpdateGuardTestServer(t)
	// PID 用自己的：它一定活着，所以探活必然判定「真的在跑」。
	writeUpdateRunStatusFile(t, cfg, updateRunStatus{Running: true, PID: os.Getpid(), State: "running", Step: "构建"})
	server.updateJobs = &fakeUpdateJobStatus{syncStatus: "idle", cleanupStatus: "idle"}
	trigger := &countingUpdateTrigger{}
	server.updateTrigger = trigger.fire

	recorder, body := callUpdateApply(server)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("已有更新在跑时应以 409 拒绝，实际 %d %s", recorder.Code, recorder.Body.String())
	}
	if body.Code != "update_already_running" {
		t.Fatalf("拒绝原因不正确：%q", body.Code)
	}
	if trigger.calls != 0 {
		t.Fatalf("已有更新在跑却仍然又启动了一个（%d 次）", trigger.calls)
	}
}

// 反面：状态文件说在跑，但那个进程已经没了（脚本被杀、机器直接重启），必须放行。
//
// 少了这条，一次半路死掉的更新会把更新按钮永久锁死，而用户完全不知道为什么——
// 这正是 readUpdateRunStatus 里那段探活存在的理由，也该由测试钉住。
func TestUpdateApplyAllowsWhenPreviousUpdateProcessIsGone(t *testing.T) {
	server, cfg := newUpdateGuardTestServer(t)
	// 999999 远超 Windows 的 PID 表，OpenProcess 会失败，探活因此判定进程已不在。
	writeUpdateRunStatusFile(t, cfg, updateRunStatus{Running: true, PID: 999999, State: "running", Step: "构建"})
	server.updateJobs = &fakeUpdateJobStatus{syncStatus: "idle", cleanupStatus: "idle"}
	trigger := &countingUpdateTrigger{}
	server.updateTrigger = trigger.fire

	recorder, body := callUpdateApply(server)

	if recorder.Code != http.StatusOK {
		t.Fatalf("上次更新已死，应放行而不是继续拒绝，实际 %d %s（原因 %q）", recorder.Code, recorder.Body.String(), body.Code)
	}
	if trigger.calls != 1 {
		t.Fatalf("放行时应当恰好启动一次更新器，实际 %d 次", trigger.calls)
	}
}

// 放行时必须恰好启动一次更新器。
func TestUpdateApplyStartsUpdateOnceWhenNothingIsRunning(t *testing.T) {
	server, _ := newUpdateGuardTestServer(t)
	server.updateJobs = &fakeUpdateJobStatus{syncStatus: "idle", cleanupStatus: "idle"}
	trigger := &countingUpdateTrigger{}
	server.updateTrigger = trigger.fire

	recorder, _ := callUpdateApply(server)

	if recorder.Code != http.StatusOK {
		t.Fatalf("没有任何任务在跑时应放行，实际 %d %s", recorder.Code, recorder.Body.String())
	}
	if trigger.calls != 1 {
		t.Fatalf("放行时应恰好启动一次更新器，实际 %d 次", trigger.calls)
	}
}

// 启动失败要如实报出来，不能报成功——否则界面会显示「正在更新」而实际什么都没发生。
func TestUpdateApplyReportsTriggerFailure(t *testing.T) {
	server, _ := newUpdateGuardTestServer(t)
	server.updateJobs = &fakeUpdateJobStatus{syncStatus: "idle", cleanupStatus: "idle"}
	trigger := &countingUpdateTrigger{err: errors.New("schtasks 拒绝了这次启动")}
	server.updateTrigger = trigger.fire

	recorder, body := callUpdateApply(server)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("启动失败应报 502，实际 %d", recorder.Code)
	}
	if body.Code != "update_task_failed" {
		t.Fatalf("失败原因不正确：%q", body.Code)
	}
	if body.Success {
		t.Fatal("启动失败时 success 不能是 true")
	}
	if !strings.Contains(body.Message, "schtasks") {
		t.Fatalf("错误原文应当透出来，实际 %q", body.Message)
	}
}

func callUpdateApply(server *Server) (*httptest.ResponseRecorder, updateErrorBody) {
	recorder := httptest.NewRecorder()
	server.handleUpdateApply(recorder, httptest.NewRequest(http.MethodPost, "/api/update/apply", nil))
	var body updateErrorBody
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	return recorder, body
}

// writeUpdateRunStatusFile 写出更新器平时写的那个状态文件，让 handler 走真实的读文件路径。
func writeUpdateRunStatusFile(t *testing.T, cfg config.Config, status updateRunStatus) {
	t.Helper()
	dir := filepath.Join(filepath.Dir(cfg.DataPath), "run")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("创建 run 目录失败：%v", err)
	}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("序列化状态失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "update-status.json"), raw, 0o600); err != nil {
		t.Fatalf("写状态文件失败：%v", err)
	}
}

func newUpdateGuardTestServer(t *testing.T) (*Server, config.Config) {
	t.Helper()
	dir := t.TempDir()
	state, err := store.Open(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatalf("创建测试数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = state.Close() })
	cfg := config.Default()
	cfg.DataPath = filepath.Join(dir, "app.db")
	cfg.UpdateEnabled = true
	return New(cfg, state, slog.New(slog.NewTextHandler(io.Discard, nil))), cfg
}
