package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// 更新器由这个计划任务承载，不是本进程的子进程。理由是要停掉、替换、再启动的正是本进程：
// 进程不能覆盖自己的镜像，也不该在自己被换掉的中途当裁判。任务名必须与
// scripts/update.ps1 里的 $TaskName 一致。
const updateTaskName = "MailBasin-Update"

// updateRunStatus 对应更新器写的 data/run/update-status.json。
//
// 为什么靠文件而不是内存：更新器跑在另一个进程里，本进程看不到它；而换二进制的那一段，
// 本进程正好是停着的。要让界面跨越这次重启看到进度，只能靠盘上的状态文件。
type updateRunStatus struct {
	Running    bool   `json:"running"`
	State      string `json:"state"`
	Step       string `json:"step"`
	Message    string `json:"message"`
	PID        int    `json:"pid"`
	FromCommit string `json:"from_commit"`
	ToCommit   string `json:"to_commit"`
	StartedAt  string `json:"started_at"`
	UpdatedAt  string `json:"updated_at"`
	FinishedAt string `json:"finished_at"`
	Error      string `json:"error"`
	RolledBack bool   `json:"rolled_back"`

	// Live 是核实过的 running。文件里写的 running 是脚本当时的状态，进程已经不在时
	// 不能再显示「更新中」。Stale 表示文件说在跑、但那个进程已经没了。
	Live  bool `json:"live"`
	Stale bool `json:"stale"`
}

// updateStatusFile 指向更新器写的状态文件。
//
// cfg.DataPath 这个名字看着像目录，实际指向数据库文件本身（默认 data\app.db），所以要取
// 它所在的那一层。我第一次就当目录用了，拼出 data\app.db\run\... 这种路径，结果接口永远
// 只看到 nil——是实测抓出来的。
//
// 这条路径和 scripts/update.ps1 里的 $RunDir（<仓库根>\data\run）在默认布局下一致。如果
// 有人把 data_path 改到别处，两边就会各写各的；这是已知的耦合。
func (s *Server) updateStatusFile() string {
	return filepath.Join(filepath.Dir(s.cfg.DataPath), "run", "update-status.json")
}

// updateRunIsLive 核实状态文件里的 running 是不是真的还在跑。
//
// 脚本可能写到一半被杀掉（机器直接重启也算），那样 running 会永远停在 true，界面就会一直
// 显示「更新中」这种假象。所以拿 PID 核实：在 Windows 上 os.FindProcess 打不开进程句柄
// 就会返回错误，正好可以当探活用。
func updateRunIsLive(status *updateRunStatus) bool {
	if status == nil || !status.Running || status.PID <= 0 {
		return false
	}
	process, err := os.FindProcess(status.PID)
	if err != nil {
		return false
	}
	_ = process.Release()
	return true
}

// readUpdateRunStatus 读更新器写的状态文件。文件缺失或解析失败都返回 nil：更新器可能正在
// 重写它，读到半截的 JSON 只是这一次轮询没有内容，不该让整个状态接口跟着报错。
func (s *Server) readUpdateRunStatus() *updateRunStatus {
	raw, err := os.ReadFile(s.updateStatusFile())
	if err != nil {
		return nil
	}
	var status updateRunStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return nil
	}
	status.Live = updateRunIsLive(&status)
	status.Stale = status.Running && !status.Live
	status.Running = status.Live
	return &status
}

func (s *Server) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	status := s.updates.Check(r.Context(), r.URL.Query().Get("force") == "1")

	// 解成 map 再挂 run 字段，而不是手抄一遍字段：抄字段名迟早会抄错一个，序列化一次就
	// 永远和 updatecheck 的 json tag 对齐。
	data := map[string]any{}
	if encoded, err := json.Marshal(status); err == nil {
		_ = json.Unmarshal(encoded, &data)
	}
	data["run"] = s.readUpdateRunStatus()

	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": data})
}

// jobActiveStatus 判断任务状态是不是「正在占着机器」。
// 取值来自 mailbox 服务的任务：idle/queued/running/interrupted/partial/completed/cancelled。
func jobActiveStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "running", "queued":
		return true
	default:
		return false
	}
}

// triggerUpdateTask 通过计划任务启动更新器，然后立刻返回。
//
// 不等它跑完：这个进程马上就会被更新器停掉，等下去只会把自己等死；而且结果本来也不该由
// 被替换的那个进程来宣布，界面读状态文件就行。
//
// 用 schtasks.exe 而不是 PowerShell 的 Start-ScheduledTask：少一层解释器，少一种失败方式。
func triggerUpdateTask() error {
	output, err := exec.Command("schtasks.exe", "/run", "/tn", updateTaskName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("启动计划任务 %s 失败：%s（%v）", updateTaskName, strings.TrimSpace(string(output)), err)
	}
	return nil
}

// handleUpdateApply 触发一次更新。
//
// 这里故意不接受任何目标参数：命令行上的 -Target 是给演练用的，一旦让调用方自选 ref，
// 任何能到达这个接口的人就能让我从任意 ref 构建并运行代码。目标固定是 origin/main，
// 由 scripts/update.ps1 决定。
func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.UpdateEnabled {
		writeError(w, http.StatusBadRequest, "update_disabled", "更新功能在配置里是关闭的（update_enabled）")
		return
	}

	// 有任务在跑就拒绝。更新会 Stop-Process -Force，没有优雅关闭；撞上正在写库的任务
	// 只能指望 SQLite 自己恢复，不该靠设计去赌。脚本看不到任务状态，这里看得到。
	if job := s.mailbox.ExistingMailboxMessageSyncStatus(); jobActiveStatus(job.Status) {
		writeError(w, http.StatusConflict, "update_busy_sync", fmt.Sprintf("批量同步正在进行（状态 %s），等它结束再更新", job.Status))
		return
	}
	if job := s.mailbox.AppleMailCleanupStatus(); jobActiveStatus(job.Status) {
		writeError(w, http.StatusConflict, "update_busy_cleanup", fmt.Sprintf("Apple 邮件清理正在进行（状态 %s），等它结束再更新", job.Status))
		return
	}

	// 已经有一个更新在跑。脚本自己会再查一次（那才是权威），这里早拒绝是为了让界面立刻
	// 拿到一句人话，而不是等一次注定失败的启动。
	if status := s.readUpdateRunStatus(); status != nil && status.Running {
		writeError(w, http.StatusConflict, "update_already_running", "已经有一个更新在跑，等它结束再说")
		return
	}

	if err := triggerUpdateTask(); err != nil {
		writeError(w, http.StatusBadGateway, "update_task_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"run": s.readUpdateRunStatus()}})
}
