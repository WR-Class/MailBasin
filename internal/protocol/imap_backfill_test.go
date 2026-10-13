package protocol

import (
	"strings"
	"testing"
	"time"
)

// 回填最容易被漏掉的一环：协议层即使拿到了全量意图，
// 「按邮箱上次同步时间算出的时间下限」仍会把每一封旧邮件判成「比上次同步还早」而丢掉，
// 而且失败是静默的——返回的是「成功 0」。只测「参数传到协议层了」抓不到它。
func TestIMAPMailboxScopeFullScanDropsPerMailboxTimeFloor(t *testing.T) {
	lastSync := time.Date(2026, 10, 13, 12, 57, 53, 0, time.UTC)
	mailbox := Mailbox{ID: "mbx-1", Email: "443836027@qq.com", LastSyncAt: lastSync, LastSyncUID: "573"}
	now := lastSync.Add(time.Hour)

	_, incremental := imapMailboxScope([]Mailbox{mailbox}, MailSyncOptions{UseCursor: true}, now)
	if incremental["mbx-1"].IsZero() {
		t.Fatalf("增量同步应当有按邮箱的时间下限")
	}
	if want := lastSync.Add(-mailboxSyncCursorOverlap); !incremental["mbx-1"].Equal(want) {
		t.Fatalf("增量下限不正确：得到 %v，期望 %v", incremental["mbx-1"], want)
	}

	_, full := imapMailboxScope([]Mailbox{mailbox}, MailSyncOptions{FullScan: true}, now)
	if !full["mbx-1"].IsZero() {
		t.Fatalf("全量回填不应套用按邮箱的时间下限，得到 %v；旧邮件正是被它丢掉的", full["mbx-1"])
	}
}

// 同一封旧邮件，在增量模式下被时间下限剪掉，在全量模式下必须留下。
// 这条走的是完整匹配流程，而不是只看单张表。
func TestICloudIMAPMessagesByMailboxFullScanKeepsOldMail(t *testing.T) {
	receivedAt := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	fetched := []iCloudIMAPFetchedMessage{{UID: "12", Raw: fullRawMessage("443836027@qq.com", "Login verification code", "Your verification code is 386812.", receivedAt)}}
	mailbox := Mailbox{ID: "mbx-1", Email: "443836027@qq.com", LastSyncAt: time.Date(2026, 10, 13, 12, 57, 53, 0, time.UTC)}

	incremental := iCloudIMAPMessagesByMailbox(fetched, []Mailbox{mailbox}, MailSyncOptions{Mode: MailSyncModeVerification, UseCursor: true}, "OpenAI")
	if len(incremental["mbx-1"]) != 0 {
		t.Fatalf("增量模式应当把旧邮件剪掉，实际留下 %d 封", len(incremental["mbx-1"]))
	}

	full := iCloudIMAPMessagesByMailbox(fetched, []Mailbox{mailbox}, MailSyncOptions{Mode: MailSyncModeVerification, FullScan: true}, "OpenAI")
	if len(full["mbx-1"]) != 1 {
		t.Fatalf("全量模式必须留下旧邮件，实际留下 %d 封", len(full["mbx-1"]))
	}
	if code := extractOTP(full["mbx-1"][0].Body); code != "386812" {
		t.Fatalf("回填回来的邮件取不到验证码：%q", code)
	}
}

// 第一轮拿到的报文里确实没有正文——这就是真实情况，头部预取回来的只有头部。
// 若在这一轮就套用「像不像验证码」的判定，主题不提验证码、码在正文里的邮件会被静默丢掉，
// 而那正是这个工具存在的理由。所以候选筛选只允许看头部能判定的东西。
func TestIMAPCandidateUIDsKeepsMessageWhoseCodeIsOnlyInBody(t *testing.T) {
	receivedAt := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	headers := []iCloudIMAPFetchedMessage{{UID: "12", Raw: headerOnlyRawMessage("443836027@qq.com", "Account notice", receivedAt)}}
	mailbox := Mailbox{ID: "mbx-1", Email: "443836027@qq.com"}

	uids := imapCandidateUIDs(headers, []Mailbox{mailbox}, MailSyncOptions{Mode: MailSyncModeVerification, FullScan: true}, "")
	if len(uids) != 1 || uids[0] != 12 {
		t.Fatalf("码只在正文里的邮件必须在第一轮就被选为候选，实际得到 %+v", uids)
	}
}

// 时间范围是头部就能判定的，第一轮该剪的还是要剪，否则预取等于没做。
func TestIMAPCandidateUIDsDropsOutOfRangeMail(t *testing.T) {
	receivedAt := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	headers := []iCloudIMAPFetchedMessage{{UID: "12", Raw: headerOnlyRawMessage("443836027@qq.com", "Login verification code", receivedAt)}}
	mailbox := Mailbox{ID: "mbx-1", Email: "443836027@qq.com"}

	uids := imapCandidateUIDs(headers, []Mailbox{mailbox}, MailSyncOptions{Mode: MailSyncModeAllRecent, After: receivedAt.Add(time.Hour)}, "")
	if len(uids) != 0 {
		t.Fatalf("早于时间范围的邮件不应成为候选，实际得到 %+v", uids)
	}
}

// 收件人不匹配的邮件不该进入第二轮：这才是预筛省下网络量的地方。
func TestIMAPCandidateUIDsDropsOtherRecipients(t *testing.T) {
	receivedAt := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	headers := []iCloudIMAPFetchedMessage{{UID: "12", Raw: headerOnlyRawMessage("someone-else@qq.com", "Login verification code", receivedAt)}}
	mailbox := Mailbox{ID: "mbx-1", Email: "443836027@qq.com"}

	uids := imapCandidateUIDs(headers, []Mailbox{mailbox}, MailSyncOptions{Mode: MailSyncModeAllRecent, FullScan: true}, "")
	if len(uids) != 0 {
		t.Fatalf("收件人不匹配的邮件不应成为候选，实际得到 %+v", uids)
	}
}

// 账号地址就是邮箱地址时，不能把这个邮箱自己从匹配表里排除掉。
//
// 这条是让 QQ 邮箱一封都收不到的真正原因：IMAP 取码邮箱的账号地址和邮箱地址是同一个，
// 而忽略名单里恰好放着它，于是 aliases 成了空表，matchingMailboxIDs 对空表直接返回 nil，
// 任何邮件都匹配不上——表现是每次同步都「成功 0 封」，游标还照常往前推进。
func TestIMAPMailboxScopeKeepsMailboxWhoseAddressIsAlsoTheAccount(t *testing.T) {
	const address = "443836027@qq.com"
	mailboxes := []Mailbox{{ID: "mbx-1", Email: address}}
	options := MailSyncOptions{Mode: MailSyncModeAllRecent, FullScan: true}

	aliases, _ := imapMailboxScope(mailboxes, options, time.Now(), address, address)
	if aliases["mbx-1"] != address {
		t.Fatalf("账号地址与邮箱地址相同时该邮箱必须留在匹配表里，实际 %v", aliases)
	}

	receivedAt := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	headers := []iCloudIMAPFetchedMessage{{UID: "1", Raw: headerOnlyRawMessage(address, "Login verification code", receivedAt)}}
	if uids := imapCandidateUIDs(headers, mailboxes, options, address, address); len(uids) != 1 {
		t.Fatalf("收件人是账号地址的邮件必须入选，实际得到 %+v", uids)
	}
}

// 忽略名单的本意不能因为上面那条改动而丢掉：只发给主号、没发给别名的邮件，
// 仍然不能算到别名头上。
func TestIMAPMailboxScopeStillIgnoresAddressesNotBeingSynced(t *testing.T) {
	const mother = "mother@example.invalid"
	mailboxes := []Mailbox{{ID: "alias-1", Email: "random-user@example.invalid"}}
	options := MailSyncOptions{Mode: MailSyncModeAllRecent, FullScan: true}

	aliases, _ := imapMailboxScope(mailboxes, options, time.Now(), mother)
	if aliases["alias-1"] != "random-user@example.invalid" {
		t.Fatalf("别名应当照常保留，实际 %v", aliases)
	}

	receivedAt := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	headers := []iCloudIMAPFetchedMessage{{UID: "2", Raw: headerOnlyRawMessage(mother, "Statement", receivedAt)}}
	if uids := imapCandidateUIDs(headers, mailboxes, options, mother); len(uids) != 0 {
		t.Fatalf("只发给主号的邮件不该算到别名头上，实际得到 %+v", uids)
	}
}

func headerOnlyRawMessage(to, subject string, receivedAt time.Time) []byte {
	var builder strings.Builder
	builder.WriteString("From: no-reply@example.invalid\r\n")
	builder.WriteString("To: " + to + "\r\n")
	builder.WriteString("Subject: " + subject + "\r\n")
	builder.WriteString("Date: " + receivedAt.Format(time.RFC1123Z) + "\r\n")
	builder.WriteString("Message-ID: <header-only-" + subject + "@example.invalid>\r\n")
	builder.WriteString("\r\n")
	return []byte(builder.String())
}

func fullRawMessage(to, subject, body string, receivedAt time.Time) []byte {
	var builder strings.Builder
	builder.WriteString("From: no-reply@example.invalid\r\n")
	builder.WriteString("To: " + to + "\r\n")
	builder.WriteString("Subject: " + subject + "\r\n")
	builder.WriteString("Date: " + receivedAt.Format(time.RFC1123Z) + "\r\n")
	builder.WriteString("Message-ID: <full-" + subject + "@example.invalid>\r\n")
	builder.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	builder.WriteString("\r\n")
	builder.WriteString(body)
	return []byte(builder.String())
}
