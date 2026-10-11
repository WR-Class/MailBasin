package httpapi

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/auth"
	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/store"
)

func TestRealtimeStartsAtLatestAndReceivesNewChange(t *testing.T) {
	state, server, token := newRealtimeFixture(t)
	if err := state.RecordEvent("info", "test", "连接前的历史事件"); err != nil {
		t.Fatalf("写入历史事件失败：%v", err)
	}
	sequence, err := state.LatestChangeSequence()
	if err != nil {
		t.Fatalf("读取连接前变更序号失败：%v", err)
	}
	response := openRealtimeResponse(t, server.URL, token, 0, false)
	defer response.Body.Close()

	if err := state.RecordEvent("info", "test", "连接后的新事件"); err != nil {
		t.Fatalf("写入实时事件失败：%v", err)
	}
	change := readRealtimeChange(t, response.Body)
	if change.Sequence <= sequence {
		t.Fatalf("新连接回放了连接前的历史变更：起点=%d change=%+v", sequence, change)
	}
}

func TestRealtimeReplaysFromLastEventID(t *testing.T) {
	state, server, token := newRealtimeFixture(t)
	sequence, err := state.LatestChangeSequence()
	if err != nil {
		t.Fatalf("读取初始变更序号失败：%v", err)
	}
	if err := state.RecordEvent("info", "test", "断线期间的事件"); err != nil {
		t.Fatalf("写入断线事件失败：%v", err)
	}
	response := openRealtimeResponse(t, server.URL, token, sequence, true)
	defer response.Body.Close()

	change := readRealtimeChange(t, response.Body)
	if change.Sequence <= sequence || change.Resource != "event" {
		t.Fatalf("Last-Event-ID 回放结果不正确：起点=%d change=%+v", sequence, change)
	}
}

func newRealtimeFixture(t *testing.T) (*store.Store, *httptest.Server, string) {
	t.Helper()
	state, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建实时测试数据库失败：%v", err)
	}
	app := New(config.Default(), state, slog.New(slog.NewTextHandler(io.Discard, nil)))
	login, err := app.auth.Setup("realtime-tester", "fixture-password")
	if err != nil {
		state.Close()
		t.Fatalf("创建实时测试管理员失败：%v", err)
	}
	server := httptest.NewServer(app)
	t.Cleanup(func() {
		server.Close()
		state.Close()
	})
	return state, server, login.Token
}

func openRealtimeResponse(t *testing.T, baseURL, token string, sequence int64, replay bool) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, baseURL+"/api/realtime", nil)
	if err != nil {
		t.Fatalf("创建 SSE 请求失败：%v", err)
	}
	request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	if replay {
		request.Header.Set("Last-Event-ID", fmt.Sprintf("%d", sequence))
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("连接 SSE 失败：%v", err)
	}
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		response.Body.Close()
		t.Fatalf("SSE 响应不正确：status=%d content_type=%q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	return response
}

func readRealtimeChange(t *testing.T, body io.Reader) store.Change {
	t.Helper()
	type result struct {
		change store.Change
		err    error
	}
	resultChannel := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var change store.Change
			err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &change)
			resultChannel <- result{change: change, err: err}
			return
		}
		resultChannel <- result{err: scanner.Err()}
	}()
	select {
	case current := <-resultChannel:
		if current.err != nil {
			t.Fatalf("读取 SSE 事件失败：%v", current.err)
		}
		return current.change
	case <-time.After(2 * time.Second):
		t.Fatal("等待 SSE 事件超时")
		return store.Change{}
	}
}
