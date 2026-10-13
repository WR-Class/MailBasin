package store

import (
	"strings"

	"icloud-privacy-mail-v2/internal/domain"
)

// providerExpr 是 domain.NormalizeProvider 的 SQL 版本。
//
// 两边必须一字不差：SQL 说「这一行不是这个服务商」而 Go 认为它是，那一行就会从
// 列表里凭空消失——而且只在翻页时消失，第一页看起来永远是对的。
const providerExpr = `CASE
	WHEN lower(trim(COALESCE(json_extract(data_json, '$.provider'), ''))) IN ('microsoft','google','external','imap')
	THEN lower(trim(json_extract(data_json, '$.provider')))
	ELSE 'icloud' END`

// normalizePage 把页码和每页条数收进合法范围。
func normalizePage(page, pageSize int) (int, int) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 200 {
		pageSize = 200
	}
	return page, pageSize
}

// accountFilterClause 把「哪个服务商 / 哪个 preset / 搜索什么」编成 WHERE 子句。
// 分页查询、取全部 id、批量操作三处共用它——三处各写一遍，迟早会有一处漏掉
// 搜索条件，于是批量操作会作用到使用者在屏幕上根本看不到的行上。
func accountFilterClause(provider, preset, query string) (string, []any) {
	where := []string{"1 = 1"}
	args := []any{}
	if wanted := strings.TrimSpace(provider); wanted != "" {
		where = append(where, providerExpr+" = ?")
		args = append(args, wanted)
	}
	if preset = strings.ToLower(strings.TrimSpace(preset)); preset != "" {
		where = append(where, `lower(trim(COALESCE(json_extract(data_json, '$.preset'), ''))) = ?`)
		args = append(args, preset)
	}
	if query = strings.ToLower(strings.TrimSpace(query)); query != "" {
		where = append(where, `lower(COALESCE(json_extract(data_json, '$.apple_id'), '') || ' ' || COALESCE(json_extract(data_json, '$.label'), '')) LIKE ?`)
		args = append(args, "%"+query+"%")
	}
	return strings.Join(where, " AND "), args
}

// AccountIDs 返回符合筛选条件的账号标识，不分页。
//
// 批量同步与批量入池要处理「当前筛选下的全部」，不能只处理当前这一页。
// 这里只 SELECT id：id 是普通列，整条路一行都不用解密，所以拿几千个 id 也很便宜。
func (s *Store) AccountIDs(provider, preset, query string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	clause, args := accountFilterClause(provider, preset, query)
	rows, err := s.db.Query(`SELECT id FROM apple_accounts WHERE `+clause+` ORDER BY json_extract(data_json, '$.created_at'), id`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]string, 0, 64)
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil && strings.TrimSpace(id) != "" {
			out = append(out, id)
		}
	}
	return out
}

// AccountsPage 返回某个服务商账号的一页。
//
// 在 SQL 里做 WHERE / COUNT / LIMIT / OFFSET，而不是读全表再在 Go 里切片：后者只是
// 把网络体积换成了内存和 CPU，账号数再涨一次还是同一个问题，这一次的修法就白费了。
//
// preset 只对 IMAP 母号有意义（qq / 163 / gmail / custom），传空表示不按它过滤。
// query 同时匹配邮箱地址和标签——使用者记不清自己当初把哪个填进了哪个字段。
//
// oldestFirst 保留各页面原本的排序方向：Apple 页历来是升序（最早的在上），
// 另外三个是降序。分页时改排序方向会让使用者以为数据被重排了。
func (s *Store) AccountsPage(provider, preset, query string, page, pageSize int, oldestFirst bool) domain.Page[domain.AppleAccount] {
	s.mu.RLock()
	defer s.mu.RUnlock()

	wanted := strings.TrimSpace(provider)
	page, pageSize = normalizePage(page, pageSize)
	clause, args := accountFilterClause(provider, preset, query)

	// 排序键里的 id 是次序稳定所必需的：只按 created_at 排时，同一秒创建的两行在
	// 不同页之间可能换位置，于是有的行翻两遍、有的行一次都不出现。
	direction := "DESC"
	if oldestFirst {
		direction = "ASC"
	}
	orderBy := ` ORDER BY json_extract(data_json, '$.created_at') ` + direction + `, id ` + direction

	var total int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM apple_accounts WHERE `+clause, args...).Scan(&total)
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages == 0 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}

	pageArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	rows, err := s.db.Query(`SELECT data_json FROM apple_accounts WHERE `+clause+orderBy+` LIMIT ? OFFSET ?`, pageArgs...)
	items := make([]domain.AppleAccount, 0, pageSize)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var data []byte
			var account domain.AppleAccount
			if rows.Scan(&data) == nil && s.decodeEntity("apple_accounts", data, &account) == nil {
				account.Password = ""
				items = append(items, account)
			}
		}
	}
	// 状态槽位要和整表那条路用同一段代码补，否则同一个账号在两条路上状态不一样。
	s.enrichAccountStatusLocked(wanted, items)
	return domain.Page[domain.AppleAccount]{
		Items: items, Page: page, PageSize: pageSize, Total: total, TotalPages: totalPages,
	}
}
