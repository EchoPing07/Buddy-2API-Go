package admin

import (
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"golang.org/x/crypto/bcrypt"
)

// hashPassword bcrypt 哈希。
func hashPassword(pw string) string {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return ""
	}
	return string(b)
}

// secondsParser 6 段含秒 cron 解析器（DEVELOPMENT.md §10）。
var secondsParser = cron.NewParser(
	cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// validCron 校验 6 段含秒 cron 表达式。
func validCron(expr string) bool {
	if strings.TrimSpace(expr) == "" {
		return false
	}
	_, err := secondsParser.Parse(expr)
	return err == nil
}

var _ = time.Now
