// Package store SQLite 数据层。
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动
)

// APIKey OpenAI 端点密钥（明文存储）。
type APIKey struct {
	ID            int64  `json:"id"`
	KeyPrefix     string `json:"key_prefix"`
	KeyPlain      string `json:"key_plain"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	TotalRequests int64  `json:"total_requests"`
	TotalTokens   int64  `json:"total_tokens"`
	CreatedAt     int64  `json:"created_at"`
	LastUsedAt    int64  `json:"last_used_at"`
}

// LogEntry 请求日志（仅元信息，不存对话内容）。
type LogEntry struct {
	ID               int64   `json:"id"`
	APIKeyID         int64   `json:"api_key_id"`
	APIKeyName       string  `json:"api_key_name"`
	Model            string  `json:"model"`
	Stream           bool    `json:"stream"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	Credit           float64 `json:"credit"`
	FinishReason     string  `json:"finish_reason"`
	DurationMs       int64   `json:"duration_ms"`
	StatusCode       int     `json:"status_code"`
	ErrorMsg         string  `json:"error_msg"`
	CreatedAt        int64   `json:"created_at"`
	// AccountUID 本次请求最终生效的账号 uid（号池开启时才有值，关闭为空）。
	// 没有它就无法验证「粘性有没有生效」，也就无法调 TTL 与分配算法。
	AccountUID string `json:"account_uid"`
}

// Store 数据库封装。
type Store struct{ db *sql.DB }

// Open 打开（或创建）数据库并建表。
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite 单写者，避免锁冲突
	if err := db.Ping(); err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS api_keys (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  key_prefix      TEXT,
  key_plain       TEXT,
  name            TEXT,
  status          TEXT DEFAULT 'active',
  total_requests  INTEGER DEFAULT 0,
  total_tokens    INTEGER DEFAULT 0,
  created_at      INTEGER,
  last_used_at    INTEGER
);
CREATE TABLE IF NOT EXISTS logs (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  api_key_id        INTEGER,
  api_key_name      TEXT,
  model             TEXT,
  stream            INTEGER,
  prompt_tokens     INTEGER DEFAULT 0,
  completion_tokens INTEGER DEFAULT 0,
  total_tokens      INTEGER DEFAULT 0,
  credit            REAL DEFAULT 0,
  finish_reason     TEXT,
  duration_ms       INTEGER,
  status_code       INTEGER,
  error_msg         TEXT,
  created_at        INTEGER
);
DROP TABLE IF EXISTS api_key_daily_usage; -- 已移除每日限额功能，清理遗留计数表
CREATE TABLE IF NOT EXISTS resource_cache (
  account_key TEXT PRIMARY KEY,
  payload     TEXT,
  updated_at  INTEGER
);
CREATE TABLE IF NOT EXISTS checkin_cache (
  account_key TEXT PRIMARY KEY,
  payload     TEXT,
  updated_at  INTEGER
);
CREATE TABLE IF NOT EXISTS growth_cache (
  account_key TEXT PRIMARY KEY,
  payload     TEXT,
  updated_at  INTEGER
);
CREATE INDEX IF NOT EXISTS idx_logs_created ON logs(created_at);
CREATE INDEX IF NOT EXISTS idx_logs_api_key ON logs(api_key_id);
CREATE INDEX IF NOT EXISTS idx_logs_model   ON logs(model);
CREATE TABLE IF NOT EXISTS pool_account_state (
  uid                TEXT PRIMARY KEY,
  manual_disabled    INTEGER NOT NULL DEFAULT 0,
  manual_disabled_at INTEGER NOT NULL DEFAULT 0,
  auto_disabled      TEXT    NOT NULL DEFAULT '',
  auto_disabled_at   INTEGER NOT NULL DEFAULT 0,
  auto_disabled_msg  TEXT    NOT NULL DEFAULT '',
  quota_fail_streak  INTEGER NOT NULL DEFAULT 0,
  last_probe_at      INTEGER NOT NULL DEFAULT 0,
  last_probe_ok      INTEGER NOT NULL DEFAULT 0,
  last_probe_msg     TEXT    NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS pool_model_cool (
  uid      TEXT    NOT NULL,
  model    TEXT    NOT NULL,
  until    INTEGER NOT NULL,
  reset_at INTEGER NOT NULL DEFAULT 0,
  code     TEXT    NOT NULL DEFAULT '',
  reason   TEXT    NOT NULL DEFAULT '',
  hits     INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (uid, model)
);
CREATE INDEX IF NOT EXISTS idx_pool_cool_until ON pool_model_cool(until);
`)
	if err != nil {
		return err
	}
	// logs.account_uid 是后续增量加列（现存库已有历史数据），
	// migrate 其余部分全是 CREATE TABLE IF NOT EXISTS，故单独处理幂等性。
	if err := s.ensureColumn("logs", "account_uid", "TEXT DEFAULT ''"); err != nil {
		return err
	}
	_, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_logs_account ON logs(account_uid)`)
	return err
}

// ensureColumn 列不存在时 ALTER TABLE 补加（SQLite 无 ADD COLUMN IF NOT EXISTS）。
//
// 现存库的经验值是 8000+ 条日志：ALTER TABLE 对无默认值的 TEXT 列是元数据级操作，
// 不重写表；带 DEFAULT 时历史行会读到默认值，COUNT/SUM 不受影响。
func (s *Store) ensureColumn(table, column, ddl string) error {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var (
			cid     int
			name    string
			typ     string
			notNull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if found {
		return nil
	}
	_, err = s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + ddl)
	return err
}

// ── api_keys ──

func scanKey(row interface{ Scan(...any) error }) (*APIKey, error) {
	var k APIKey
	var stream sql.NullBool
	_ = stream
	err := row.Scan(&k.ID, &k.KeyPrefix, &k.KeyPlain, &k.Name, &k.Status,
		&k.TotalRequests, &k.TotalTokens, &k.CreatedAt, &k.LastUsedAt)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

const keyCols = `id, COALESCE(key_prefix,''), COALESCE(key_plain,''), COALESCE(name,''), COALESCE(status,'active'), COALESCE(total_requests,0), COALESCE(total_tokens,0), COALESCE(created_at,0), COALESCE(last_used_at,0)`

// ListKeys 全量 key 列表。
func (s *Store) ListKeys() ([]APIKey, error) {
	rows, err := s.db.Query(`SELECT ` + keyCols + ` FROM api_keys ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// CreateKey 新增 key。
func (s *Store) CreateKey(k *APIKey) error {
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO api_keys (key_prefix, key_plain, name, status, created_at)
		VALUES (?,?,?,?,?)`,
		k.KeyPrefix, k.KeyPlain, k.Name, orDefaultStr(k.Status, "active"), now)
	if err != nil {
		return err
	}
	k.ID, _ = res.LastInsertId()
	k.CreatedAt = now
	k.Status = orDefaultStr(k.Status, "active")
	return nil
}

// UpdateKey 更新备注/状态。
func (s *Store) UpdateKey(id int64, name, status *string) error {
	k, err := s.GetKey(id)
	if err != nil {
		return err
	}
	if k == nil {
		return fmt.Errorf("key 不存在")
	}
	if name != nil {
		k.Name = *name
	}
	if status != nil {
		if *status != "active" && *status != "disabled" {
			return fmt.Errorf("status 取值非法")
		}
		k.Status = *status
	}
	_, err = s.db.Exec(`UPDATE api_keys SET name=?, status=? WHERE id=?`,
		k.Name, k.Status, id)
	return err
}

// DeleteKey 删除 key。
func (s *Store) DeleteKey(id int64) error {
	_, err := s.db.Exec(`DELETE FROM api_keys WHERE id=?`, id)
	return err
}

// GetKey 按 ID 查询。
func (s *Store) GetKey(id int64) (*APIKey, error) {
	row := s.db.QueryRow(`SELECT `+keyCols+` FROM api_keys WHERE id=?`, id)
	k, err := scanKey(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return k, nil
}

// IncrementKeyUsage 累计请求数与 token 数。
func (s *Store) IncrementKeyUsage(id int64, tokens int64) error {
	_, err := s.db.Exec(`UPDATE api_keys SET total_requests=total_requests+1, total_tokens=total_tokens+?, last_used_at=? WHERE id=?`,
		tokens, time.Now().Unix(), id)
	return err
}

// ── logs ──

// InsertLog 写入请求日志。
func (s *Store) InsertLog(l *LogEntry) error {
	_, err := s.db.Exec(`INSERT INTO logs (api_key_id, api_key_name, model, stream, prompt_tokens,
		completion_tokens, total_tokens, credit, finish_reason, duration_ms, status_code, error_msg, created_at, account_uid)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		l.APIKeyID, l.APIKeyName, l.Model, boolToInt(l.Stream), l.PromptTokens, l.CompletionTokens,
		l.TotalTokens, l.Credit, l.FinishReason, l.DurationMs, l.StatusCode, l.ErrorMsg,
		orDefaultInt64(l.CreatedAt, time.Now().Unix()), l.AccountUID)
	return err
}

// LogFilter 日志查询过滤条件。
type LogFilter struct {
	Model      string // 模糊
	KeyID      int64  // =0 不过滤
	Status     int    // >0 精确；<0 表示仅错误（>=400）
	AccountUID string // 精确（号池开启时才有值）
	Page       int
	PageSize   int
}

// QueryLogs 分页查询日志。
func (s *Store) QueryLogs(f LogFilter) ([]LogEntry, int, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 || f.PageSize > 100 {
		f.PageSize = 20
	}
	where, args := " WHERE 1=1", []any{}
	if f.Model != "" {
		where += " AND model LIKE ?"
		args = append(args, "%"+f.Model+"%")
	}
	if f.KeyID > 0 {
		where += " AND api_key_id=?"
		args = append(args, f.KeyID)
	}
	if f.Status > 0 {
		where += " AND status_code=?"
		args = append(args, f.Status)
	} else if f.Status < 0 {
		where += " AND (status_code>=400 OR error_msg!='')"
	}
	if f.AccountUID != "" {
		where += " AND account_uid=?"
		args = append(args, f.AccountUID)
	}

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM logs`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	q := `SELECT id, api_key_id, COALESCE(api_key_name,''), COALESCE(model,''), stream, COALESCE(prompt_tokens,0), COALESCE(completion_tokens,0),
		COALESCE(total_tokens,0), COALESCE(credit,0), COALESCE(finish_reason,''), COALESCE(duration_ms,0), COALESCE(status_code,0), COALESCE(error_msg,''), created_at, COALESCE(account_uid,'')
		FROM logs` + where + ` ORDER BY id DESC LIMIT ? OFFSET ?`
	args = append(args, f.PageSize, (f.Page-1)*f.PageSize)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []LogEntry
	for rows.Next() {
		var l LogEntry
		var stream int
		err := rows.Scan(&l.ID, &l.APIKeyID, &l.APIKeyName, &l.Model, &stream, &l.PromptTokens,
			&l.CompletionTokens, &l.TotalTokens, &l.Credit, &l.FinishReason, &l.DurationMs,
			&l.StatusCode, &l.ErrorMsg, &l.CreatedAt, &l.AccountUID)
		if err != nil {
			return nil, 0, err
		}
		l.Stream = stream != 0
		out = append(out, l)
	}
	return out, total, rows.Err()
}

// ── stats ──

// Stats 仪表盘聚合。
type Stats struct {
	TotalRequests int64       `json:"total_requests"`
	TotalTokens   int64       `json:"total_tokens"`
	TotalCredit   float64     `json:"total_credit"`
	ErrorCount    int64       `json:"error_count"`
	TodayRequests int64       `json:"today_requests"`
	TodayTokens   int64       `json:"today_tokens"`
	Daily         []DailyStat `json:"daily"`
	ByModel       []ModelStat `json:"by_model"`
	RecentErrors  []LogEntry  `json:"recent_errors"`
}

// DailyStat 按日请求/tokens/credits 统计。
type DailyStat struct {
	Date     string  `json:"date"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Credit   float64 `json:"credit"`
}

// ModelStat 按模型聚合。
type ModelStat struct {
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}

// GetStats 仪表盘聚合查询。
func (s *Store) GetStats() (*Stats, error) {
	st := &Stats{}
	err := func() error {
		// 总量
		if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(total_tokens),0), COALESCE(SUM(credit),0),
			COALESCE(SUM(CASE WHEN status_code>=400 OR error_msg!='' THEN 1 ELSE 0 END),0) FROM logs`).
			Scan(&st.TotalRequests, &st.TotalTokens, &st.TotalCredit, &st.ErrorCount); err != nil {
			return err
		}
		// 今日
		if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(total_tokens),0) FROM logs
			WHERE date(created_at,'unixepoch','localtime')=date('now','localtime')`).
			Scan(&st.TodayRequests, &st.TodayTokens); err != nil {
			return err
		}
		// 近 14 天
		rows, err := s.db.Query(`SELECT date(created_at,'unixepoch','localtime') d, COUNT(*), COALESCE(SUM(total_tokens),0), COALESCE(SUM(credit),0)
			FROM logs WHERE created_at >= strftime('%s','now','-13 days','start of day')
			GROUP BY d ORDER BY d`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d DailyStat
			if err := rows.Scan(&d.Date, &d.Requests, &d.Tokens, &d.Credit); err != nil {
				return err
			}
			st.Daily = append(st.Daily, d)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		// 按模型聚合，不截断：模型集合由 /v3/config 下发、数量有限，截断会使返回条数少于实际模型数
		rows2, err := s.db.Query(`SELECT COALESCE(model,''), COUNT(*), COALESCE(SUM(total_tokens),0)
			FROM logs GROUP BY model ORDER BY COUNT(*) DESC`)
		if err != nil {
			return err
		}
		defer rows2.Close()
		for rows2.Next() {
			var m ModelStat
			if err := rows2.Scan(&m.Model, &m.Requests, &m.Tokens); err != nil {
				return err
			}
			st.ByModel = append(st.ByModel, m)
		}
		// 近期错误
		rows4, err := s.db.Query(`SELECT id, api_key_id, COALESCE(api_key_name,''), COALESCE(model,''), stream, COALESCE(prompt_tokens,0),
			COALESCE(completion_tokens,0), COALESCE(total_tokens,0), COALESCE(credit,0), COALESCE(finish_reason,''), COALESCE(duration_ms,0), COALESCE(status_code,0), COALESCE(error_msg,''), created_at, COALESCE(account_uid,'')
			FROM logs WHERE status_code>=400 OR error_msg!='' ORDER BY id DESC LIMIT 10`)
		if err != nil {
			return err
		}
		defer rows4.Close()
		for rows4.Next() {
			var l LogEntry
			var stream int
			if err := rows4.Scan(&l.ID, &l.APIKeyID, &l.APIKeyName, &l.Model, &stream, &l.PromptTokens,
				&l.CompletionTokens, &l.TotalTokens, &l.Credit, &l.FinishReason, &l.DurationMs,
				&l.StatusCode, &l.ErrorMsg, &l.CreatedAt, &l.AccountUID); err != nil {
				return err
			}
			l.Stream = stream != 0
			st.RecentErrors = append(st.RecentErrors, l)
		}
		return rows4.Err()
	}()
	if err != nil {
		return nil, err
	}
	return st, nil
}

// CleanupLogs 删除保留期外的日志，返回删除条数。
func (s *Store) CleanupLogs(retentionDays int) (int64, error) {
	if retentionDays <= 0 {
		return 0, nil
	}
	cutoff := time.Now().AddDate(0, 0, -retentionDays).Unix()
	res, err := s.db.Exec(`DELETE FROM logs WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CleanupLogsBySize 按累计大小估算删除最旧日志，使总占用降至 maxBytes 以下。
func (s *Store) CleanupLogsBySize(maxBytes int64) (int64, error) {
	if maxBytes <= 0 {
		return 0, nil
	}
	var total, count int64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(
		length(CAST(COALESCE(model,'') AS BLOB)) +
		length(CAST(COALESCE(api_key_name,'') AS BLOB)) +
		length(CAST(COALESCE(finish_reason,'') AS BLOB)) +
		length(CAST(COALESCE(error_msg,'') AS BLOB)) + 128), 0), COUNT(*)
		FROM logs`).Scan(&total, &count)
	if err != nil {
		return 0, err
	}
	if count == 0 || total <= maxBytes {
		return 0, nil
	}
	avg := total / count
	if avg < 1 {
		avg = 1
	}
	toDelete := (total - maxBytes) / avg
	if toDelete < 1 {
		toDelete = 1
	}
	res, err := s.db.Exec(`DELETE FROM logs WHERE id IN (SELECT id FROM logs ORDER BY id ASC LIMIT ?)`, toDelete)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ── 号池状态 ──

// PoolAccountRow 账号级池状态（一行一个账号）。
// 手动域与自动域分开存：手动禁用是用户的决定，探测成功也不能自动解开。
type PoolAccountRow struct {
	UID              string `json:"uid"`
	ManualDisabled   bool   `json:"manual_disabled"`
	ManualDisabledAt int64  `json:"manual_disabled_at"`
	AutoDisabled     string `json:"auto_disabled"` // "" | "quota" | "credential"
	AutoDisabledAt   int64  `json:"auto_disabled_at"`
	AutoDisabledMsg  string `json:"auto_disabled_msg"`
	QuotaFailStreak  int    `json:"quota_fail_streak"`
	LastProbeAt      int64  `json:"last_probe_at"`
	LastProbeOK      bool   `json:"last_probe_ok"`
	LastProbeMsg     string `json:"last_probe_msg"`
}

// PoolCoolRow 模型级冷却（一行一个 (uid, model)）。
type PoolCoolRow struct {
	UID     string `json:"uid"`
	Model   string `json:"model"`
	Until   int64  `json:"until"`    // 实际生效截止（已截断）
	ResetAt int64  `json:"reset_at"` // 上游原始墙钟（0 = 上游没给时间）
	Code    string `json:"code"`
	Reason  string `json:"reason"`
	Hits    int    `json:"hits"`
}

// LoadPoolAccounts 读出全部账号级状态。
func (s *Store) LoadPoolAccounts() ([]PoolAccountRow, error) {
	rows, err := s.db.Query(`SELECT uid, COALESCE(manual_disabled,0), COALESCE(manual_disabled_at,0),
		COALESCE(auto_disabled,''), COALESCE(auto_disabled_at,0), COALESCE(auto_disabled_msg,''),
		COALESCE(quota_fail_streak,0), COALESCE(last_probe_at,0), COALESCE(last_probe_ok,0), COALESCE(last_probe_msg,'')
		FROM pool_account_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PoolAccountRow
	for rows.Next() {
		var r PoolAccountRow
		var manual, probeOK int
		if err := rows.Scan(&r.UID, &manual, &r.ManualDisabledAt, &r.AutoDisabled,
			&r.AutoDisabledAt, &r.AutoDisabledMsg, &r.QuotaFailStreak,
			&r.LastProbeAt, &probeOK, &r.LastProbeMsg); err != nil {
			return nil, err
		}
		r.ManualDisabled, r.LastProbeOK = manual != 0, probeOK != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// SavePoolAccount 写入/覆盖一个账号的池状态。
func (s *Store) SavePoolAccount(r *PoolAccountRow) error {
	_, err := s.db.Exec(`INSERT INTO pool_account_state
		(uid, manual_disabled, manual_disabled_at, auto_disabled, auto_disabled_at,
		 auto_disabled_msg, quota_fail_streak, last_probe_at, last_probe_ok, last_probe_msg)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(uid) DO UPDATE SET
		 manual_disabled=excluded.manual_disabled, manual_disabled_at=excluded.manual_disabled_at,
		 auto_disabled=excluded.auto_disabled, auto_disabled_at=excluded.auto_disabled_at,
		 auto_disabled_msg=excluded.auto_disabled_msg, quota_fail_streak=excluded.quota_fail_streak,
		 last_probe_at=excluded.last_probe_at, last_probe_ok=excluded.last_probe_ok,
		 last_probe_msg=excluded.last_probe_msg`,
		r.UID, boolToInt(r.ManualDisabled), r.ManualDisabledAt, r.AutoDisabled, r.AutoDisabledAt,
		r.AutoDisabledMsg, r.QuotaFailStreak, r.LastProbeAt, boolToInt(r.LastProbeOK), r.LastProbeMsg)
	return err
}

// DeletePoolAccount 删除一个账号的全部池状态（含模型冷却）。
// 删账号档案时必须一并调：否则重新导入同一个号会继承旧的「额度耗尽」标记而立即被跳过。
func (s *Store) DeletePoolAccount(uid string) error {
	if _, err := s.db.Exec(`DELETE FROM pool_account_state WHERE uid=?`, uid); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM pool_model_cool WHERE uid=?`, uid)
	return err
}

// LoadPoolCools 读出全部尚未过期的模型冷却（now 之前到期的直接跳过，由 CleanupPoolCools 清）。
func (s *Store) LoadPoolCools(now int64) ([]PoolCoolRow, error) {
	rows, err := s.db.Query(`SELECT uid, model, COALESCE(until,0), COALESCE(reset_at,0),
		COALESCE(code,''), COALESCE(reason,''), COALESCE(hits,0)
		FROM pool_model_cool WHERE until > ?`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PoolCoolRow
	for rows.Next() {
		var r PoolCoolRow
		if err := rows.Scan(&r.UID, &r.Model, &r.Until, &r.ResetAt, &r.Code, &r.Reason, &r.Hits); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SavePoolCool 写入/覆盖一条模型冷却。
func (s *Store) SavePoolCool(r *PoolCoolRow) error {
	_, err := s.db.Exec(`INSERT INTO pool_model_cool (uid, model, until, reset_at, code, reason, hits)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(uid, model) DO UPDATE SET
		 until=excluded.until, reset_at=excluded.reset_at, code=excluded.code,
		 reason=excluded.reason, hits=excluded.hits`,
		r.UID, r.Model, r.Until, r.ResetAt, r.Code, r.Reason, r.Hits)
	return err
}

// DeletePoolCool 删除一条模型冷却（幂等）。
func (s *Store) DeletePoolCool(uid, model string) error {
	_, err := s.db.Exec(`DELETE FROM pool_model_cool WHERE uid=? AND model=?`, uid, model)
	return err
}

// CleanupPoolCools 清理已过期的模型冷却，返回删除条数。
func (s *Store) CleanupPoolCools(now int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM pool_model_cool WHERE until <= ?`, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ── 缓存表 ──

// cacheTables 允许作为缓存表的白名单，防止 table 参数拼接 SQL 注入。
var cacheTables = map[string]bool{
	"resource_cache": true,
	"checkin_cache":  true,
	"growth_cache":   true,
}

// GetCache 读缓存（resource_cache / checkin_cache / growth_cache 共用），TTL 秒内有效。
func (s *Store) GetCache(table, key string, ttlSeconds int) (string, int64, bool) {
	if !cacheTables[table] {
		return "", 0, false
	}
	var payload string
	var updatedAt int64
	err := s.db.QueryRow(`SELECT payload, updated_at FROM `+table+` WHERE account_key=?`, key).
		Scan(&payload, &updatedAt)
	if err != nil {
		return "", 0, false
	}
	if ttlSeconds > 0 && time.Now().Unix()-updatedAt > int64(ttlSeconds) {
		return payload, updatedAt, false // 过期（仍返回内容供 force=0 参考）
	}
	return payload, updatedAt, true
}

// SetCache 写缓存。
func (s *Store) SetCache(table, key, payload string) error {
	if !cacheTables[table] {
		return fmt.Errorf("非法缓存表: %s", table)
	}
	_, err := s.db.Exec(`INSERT INTO `+table+` (account_key, payload, updated_at) VALUES (?,?,?)
		ON CONFLICT(account_key) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at`,
		key, payload, time.Now().Unix())
	return err
}

// DeleteCacheKey 删除指定缓存行（幂等）。缓存表无 TTL 清理任务，换 key 后
// 旧行会永久残留，由调用方在写入新 key 时顺手清理遗留 key。
func (s *Store) DeleteCacheKey(table, key string) error {
	if !cacheTables[table] {
		return fmt.Errorf("非法缓存表: %s", table)
	}
	_, err := s.db.Exec(`DELETE FROM `+table+` WHERE account_key=?`, key)
	return err
}

// ── util ──

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func orDefaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func orDefaultInt64(v, def int64) int64 {
	if v == 0 {
		return def
	}
	return v
}
