package updatecheck

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"icloud-privacy-mail-v2/internal/buildinfo"
)

func TestCheckUsesRepositoryManifest(t *testing.T) {
	originalVersion := buildinfo.Version
	originalCommit := buildinfo.Commit
	buildinfo.Version = "2.0.0"
	buildinfo.Commit = "test-commit"
	defer func() {
		buildinfo.Version = originalVersion
		buildinfo.Commit = originalCommit
	}()

	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestCount++
		if request.URL.Path == "/owner/repository/info/refs" {
			writer.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			fmt.Fprint(writer, refsAdvertisement("refs/heads/main", branchHeadSHA))
			return
		}
		if request.URL.Path != "/owner/repository/HEAD/internal/updatecheck/announcements.json" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{
  "schema_version": 1,
  "latest": {
    "version": "2.1.0",
    "name": "2.1.0 源码版",
    "notes": "新增稳定的更新检查。",
    "published_at": "2026-08-28T12:00:00Z",
    "url": "https://github.com/owner/repository/archive/refs/heads/main.zip"
  },
  "announcements": [
    {"id":"notice-1","type":"project","title":"项目公告","summary":"摘要","content":"内容","published_at":"2026-08-28T11:00:00Z"}
  ]
}`)
	}))
	defer server.Close()

	service := New(true, "owner/repository")
	service.rawBaseURL = server.URL
	service.webBaseURL = server.URL
	status := service.Check(context.Background(), true)
	if status.Error != "" {
		t.Fatalf("按仓库公告配置检查失败：%s", status.Error)
	}
	// 一次公告配置 + 一次分支头。多问是无谓的网络往返，少问是有一问根本没发生。
	if requestCount != 2 {
		t.Fatalf("更新检查应各请求一次（公告配置 + 分支头），实际请求 %d 次", requestCount)
	}
	if status.Target == nil || status.Target.Commit != branchHeadSHA {
		t.Fatalf("目标提交未从远端广告里读出：%+v", status.Target)
	}
	if !status.UpdateAvailable || status.Latest == nil || status.Latest.Version != "2.1.0" || status.Latest.Source != "config" {
		t.Fatalf("最新版本判断不正确：%+v", status)
	}
	if !containsAnnouncement(status.Announcements, "notice-1") {
		t.Fatalf("未读取公告列表：%+v", status.Announcements)
	}
}

func TestConfiguredLatestRecognizesSameVersion(t *testing.T) {
	latest, available, announcement, err := configuredLatest(buildinfo.Info{Version: "2.0.0-dev"}, latestDocument{
		Version: "2.0.0-dev",
		Name:    "2.0.0 开发版",
		Notes:   "当前版本。",
		URL:     "https://github.com/owner/repository",
	})
	if err != nil {
		t.Fatalf("解析 latest 失败：%v", err)
	}
	if available {
		t.Fatal("相同版本不应提示更新")
	}
	if latest == nil || latest.Source != "config" || announcement != nil {
		t.Fatalf("更新配置结果不正确：latest=%+v announcement=%+v", latest, announcement)
	}
}

func TestCheckRejectsLegacyManifest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{"announcements":[]}`)
	}))
	defer server.Close()

	service := New(true, "owner/repository")
	service.rawBaseURL = server.URL
	service.webBaseURL = server.URL
	status := service.Check(context.Background(), true)
	if !strings.Contains(status.Error, "schema_version 应为 1") {
		t.Fatalf("旧格式应返回明确配置错误：%q", status.Error)
	}
}

func TestCheckRequiresLatest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{"schema_version":1,"announcements":[]}`)
	}))
	defer server.Close()

	service := New(true, "owner/repository")
	service.rawBaseURL = server.URL
	service.webBaseURL = server.URL
	status := service.Check(context.Background(), true)
	if status.Error != "仓库公告配置缺少 latest" {
		t.Fatalf("缺少 latest 时的错误不正确：%q", status.Error)
	}
}

func TestVersionIsNewer(t *testing.T) {
	testCases := []struct {
		name     string
		current  string
		latest   string
		expected bool
	}{
		{name: "次版本升级", current: "2.0.0", latest: "2.1.0", expected: true},
		{name: "当前版本更高", current: "2.1.0", latest: "2.0.0", expected: false},
		{name: "开发版升级为正式版", current: "2.0.0-dev", latest: "2.0.0", expected: true},
		{name: "正式版高于开发版", current: "2.0.0", latest: "2.0.0-dev", expected: false},
		{name: "高版本开发版不会降级", current: "2.1.0-dev", latest: "2.0.0", expected: false},
		{name: "相同开发版", current: "2.0.0-dev", latest: "2.0.0-dev", expected: false},
		{name: "支持 v 前缀", current: "v2.0.0", latest: "v2.0.1", expected: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if actual := versionIsNewer(testCase.current, testCase.latest); actual != testCase.expected {
				t.Fatalf("versionIsNewer(%q, %q) = %v，期望 %v", testCase.current, testCase.latest, actual, testCase.expected)
			}
		})
	}
}

// branchHeadSHA 是测试用的假提交号。用一个只有在真的读了远端广告时才会出现的
// 值，才能把「读了远端」和「自己编了一个」区分开。
// 拼成 8+10+10+10+2 位：git 的提交号必须正好 40 位，而写成一整串时眼睛数不准长度，
// 长度错了会让解析整体错位、安静地返回空——这个坑已经踩过一次。
const branchHeadSHA = "5bd7a270" + "0000000000" + "0000000000" + "0000000000" + "ff"

// refsAdvertisement 造一段结构真实的 git smart HTTP 广告（含服务宣告、flush 包、
// HEAD 行和能力列表），用来测解析——长度前缀是按真实格式算出来的。
func refsAdvertisement(ref, sha string) string {
	header := "# service=git-upload-pack\n"
	headLine := sha + " HEAD\x00multi_ack thin-pack side-band\n"
	refLine := sha + " " + ref + "\n"
	return fmt.Sprintf("%04x%s0000%04x%s%04x%s0000",
		4+len(header), header,
		4+len(headLine), headLine,
		4+len(refLine), refLine)
}

// 目标提交必须来自远端广告本身，而且是我们要的那个引用，不能顺手抓第一行
// （HEAD 行的提交号和分支头通常相同，所以必须再确认分支名不存在的分支会落空）。
func TestFindRefCommitPicksTheNamedBranch(t *testing.T) {
	if len(branchHeadSHA) != 40 {
		t.Fatalf("测试用的提交号必须正好 40 位，实际 %d 位", len(branchHeadSHA))
	}
	payload := refsAdvertisement("refs/heads/main", branchHeadSHA)
	if got := findRefCommit([]byte(payload), "refs/heads/main"); got != branchHeadSHA {
		t.Fatalf("应读出 %s，实际 %q", branchHeadSHA, got)
	}
	if got := findRefCommit([]byte(payload), "refs/heads/release"); got != "" {
		t.Fatalf("分支不存在时应返回空，实际 %q", got)
	}
}

// 取不到目标时不能编：留空并说明原因，让界面退回原来的措辞。
func TestCheckLeavesTargetEmptyWhenBranchMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "/info/refs") {
			writer.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			fmt.Fprint(writer, refsAdvertisement("refs/heads/other", branchHeadSHA))
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{"schema_version":1,"latest":{"version":"2.1.0","name":"n","notes":"x","url":"https://github.com/owner/repository"},"announcements":[]}`)
	}))
	defer server.Close()

	service := New(true, "owner/repository")
	service.rawBaseURL = server.URL
	service.webBaseURL = server.URL
	status := service.Check(context.Background(), true)
	if status.Error != "" {
		t.Fatalf("公告本身正常，不该整体报错：%s", status.Error)
	}
	if status.Target == nil || status.Target.Commit != "" || status.Target.Error == "" {
		t.Fatalf("目标取不到时应留空并说明，实际 %+v", status.Target)
	}
}

func containsAnnouncement(items []Announcement, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}
