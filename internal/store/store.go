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
CREATE INDEX IF NOT EXISTS idx_logs_created ON logs(created_at);
CREATE INDEX IF NOT EXISTS idx_logs_api_key ON logs(api_key_id);
CREATE INDEX IF NOT EXISTS idx_logs_model   ON logs(model);
`)
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
		completion_tokens, total_tokens, credit, finish_reason, duration_ms, status_code, error_msg, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		l.APIKeyID, l.APIKeyName, l.Model, boolToInt(l.Stream), l.PromptTokens, l.CompletionTokens,
		l.TotalTokens, l.Credit, l.FinishReason, l.DurationMs, l.StatusCode, l.ErrorMsg,
		orDefaultInt64(l.CreatedAt, time.Now().Unix()))
	return err
}

// LogFilter 日志查询过滤条件。
type LogFilter struct {
	Model    string // 模糊
	KeyID    int64  // =0 不过滤
	Status   int    // >0 精确；<0 表示仅错误（>=400）
	Page     int
	PageSize int
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

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM logs`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	q := `SELECT id, api_key_id, COALESCE(api_key_name,''), COALESCE(model,''), stream, COALESCE(prompt_tokens,0), COALESCE(completion_tokens,0),
		COALESCE(total_tokens,0), COALESCE(credit,0), COALESCE(finish_reason,''), COALESCE(duration_ms,0), COALESCE(status_code,0), COALESCE(error_msg,''), created_at
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
			&l.StatusCode, &l.ErrorMsg, &l.CreatedAt)
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
	ByKey         []KeyStat   `json:"by_key"`
	RecentErrors  []LogEntry  `json:"recent_errors"`
}

// DailyStat 按日请求/token 统计。
type DailyStat struct {
	Date     string `json:"date"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}

// ModelStat 按模型聚合。
type ModelStat struct {
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}

// KeyStat 按 key 聚合。
type KeyStat struct {
	APIKeyID   int64  `json:"api_key_id"`
	APIKeyName string `json:"api_key_name"`
	Requests   int64  `json:"requests"`
	Tokens     int64  `json:"tokens"`
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
		rows, err := s.db.Query(`SELECT date(created_at,'unixepoch','localtime') d, COUNT(*), COALESCE(SUM(total_tokens),0)
			FROM logs WHERE created_at >= strftime('%s','now','-13 days','start of day')
			GROUP BY d ORDER BY d`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d DailyStat
			if err := rows.Scan(&d.Date, &d.Requests, &d.Tokens); err != nil {
				return err
			}
			st.Daily = append(st.Daily, d)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		// 按模型 top10
		rows2, err := s.db.Query(`SELECT COALESCE(model,''), COUNT(*), COALESCE(SUM(total_tokens),0)
			FROM logs GROUP BY model ORDER BY COUNT(*) DESC LIMIT 10`)
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
		// 按 key
		rows3, err := s.db.Query(`SELECT COALESCE(api_key_id,0), COALESCE(api_key_name,''), COUNT(*), COALESCE(SUM(total_tokens),0)
			FROM logs GROUP BY api_key_id, api_key_name ORDER BY COUNT(*) DESC LIMIT 10`)
		if err != nil {
			return err
		}
		defer rows3.Close()
		for rows3.Next() {
			var k KeyStat
			if err := rows3.Scan(&k.APIKeyID, &k.APIKeyName, &k.Requests, &k.Tokens); err != nil {
				return err
			}
			st.ByKey = append(st.ByKey, k)
		}
		// 近期错误
		rows4, err := s.db.Query(`SELECT id, api_key_id, COALESCE(api_key_name,''), COALESCE(model,''), stream, COALESCE(prompt_tokens,0),
			COALESCE(completion_tokens,0), COALESCE(total_tokens,0), COALESCE(credit,0), COALESCE(finish_reason,''), COALESCE(duration_ms,0), COALESCE(status_code,0), COALESCE(error_msg,''), created_at
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
				&l.StatusCode, &l.ErrorMsg, &l.CreatedAt); err != nil {
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

// ── 缓存表 ──

// GetCache 读缓存（resource_cache / checkin_cache 共用），TTL 秒内有效。
func (s *Store) GetCache(table, key string, ttlSeconds int) (string, int64, bool) {
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
	_, err := s.db.Exec(`INSERT INTO `+table+` (account_key, payload, updated_at) VALUES (?,?,?)
		ON CONFLICT(account_key) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at`,
		key, payload, time.Now().Unix())
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
