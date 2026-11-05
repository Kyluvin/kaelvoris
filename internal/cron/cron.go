// Package cron 定时任务：cron 表达式解析 + 持久化存储 + 应用内调度器。
//
// 表达式格式（5 段，空格分隔）：分 时 日 月 周，支持 * , - / 与数字；
// 另支持 @ 描述符（对齐 robfig/cron / vixie cron 语法糖）：
//   - "@hourly"            每小时整点
//   - "@daily"/@midnight   每天 00:00
//   - "@weekly"            每周日 00:00
//   - "@monthly"           每月 1 日 00:00
//   - "@yearly"/@annually  每年 1 月 1 日 00:00
//   - "@every 1h30m"       固定间隔（Go duration 语法，最小 1m）
//
// 5 段示例：
//   - "* * * * *"   每分钟
//   - "0 9 * * 1-5" 工作日每天 09:00
//   - "*/15 * * * *"每 15 分钟
//   - "0 12 1 * *"  每月 1 号 12:00
//
// 调度语义：桌面端运行期间分钟级 tick，到期即触发任务（经回调送入现有
// 会话执行链）；上次运行未结束则本次跳过（防重入），下次到期再试。
// 可靠性语义（对齐 GitHub Actions schedule/timeout + chroniker 熔断）：
// 单次运行超时自动解除占用；失败按退避自动重试；连续失败达阈值自动
// 熔断停用（防风暴），用户排查后手动重新启用。
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule 解析后的 cron 计划（各字段为"允许值集合"）。
// every>0 时为 @every 固定间隔模式（其余字段不参与匹配）。
type Schedule struct {
	minute  map[int]bool  // 0-59
	hour    map[int]bool  // 0-23
	dom     map[int]bool  // 1-31（日）
	month   map[int]bool  // 1-12
	dow     map[int]bool  // 0-6（周日=0）
	domStar bool          // 日域为 *（日/周约束互斥：其一为 * 时另一生效）
	dowStar bool          // 周域为 *
	every   time.Duration // @every 固定间隔（0 = 非 @every 模式）
}

// Every 返回 @every 固定间隔（非 @every 表达式返回 0）。
func (s *Schedule) Every() time.Duration { return s.every }

// 单次运行可靠性参数默认值（对齐 GitHub Actions timeout-minutes + retry、
// chroniker 连败熔断；均可被 Job 字段覆盖）。
const (
	// DefaultTimeoutMin 单次运行超时（分钟）：超时解除重入占用，允许下次再触发。
	DefaultTimeoutMin = 30
	// MaxRetryLimit 失败自动重试次数上限（防无限重试风暴）。
	MaxRetryLimit = 3
	// MaxFailStreak 连续失败自动熔断阈值（达到即停用任务，需手动重新启用）。
	MaxFailStreak = 3
	// RetryBackoff 失败重试等待间隔。
	RetryBackoff = 5 * time.Minute
)

// Parse 解析 cron 表达式：支持 5 段标准式与 @ 描述符（@hourly/@daily/
// @weekly/@monthly/@yearly/@every <时长>，对齐 vixie cron 语法糖）。
func Parse(expr string) (*Schedule, error) {
	expr = strings.TrimSpace(expr)
	if strings.HasPrefix(expr, "@") {
		switch strings.ToLower(expr) {
		case "@yearly", "@annually":
			return Parse("0 0 1 1 *")
		case "@monthly":
			return Parse("0 0 1 * *")
		case "@weekly":
			return Parse("0 0 * * 0")
		case "@daily", "@midnight":
			return Parse("0 0 * * *")
		case "@hourly":
			return Parse("0 * * * *")
		}
		if strings.HasPrefix(strings.ToLower(expr), "@every ") {
			d, err := time.ParseDuration(strings.TrimSpace(expr[len("@every "):]))
			if err != nil {
				return nil, fmt.Errorf("@every 时长非法（Go duration 语法，示例 @every 1h30m）: %w", err)
			}
			if d < time.Minute {
				return nil, fmt.Errorf("@every 最小间隔为 1m（收到 %s）", d)
			}
			return &Schedule{every: d}, nil
		}
		return nil, fmt.Errorf("未知描述符 %q（支持 @hourly @daily @weekly @monthly @yearly @every <时长>）", expr)
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron 表达式应为 5 段（分 时 日 月 周），收到 %d 段", len(fields))
	}
	minute, err := parseField(fields[0], 0, 59)
	if err != nil {
		return nil, fmt.Errorf("分钟域: %w", err)
	}
	hour, err := parseField(fields[1], 0, 23)
	if err != nil {
		return nil, fmt.Errorf("小时域: %w", err)
	}
	dom, domStar, err := parseDayField(fields[2], 1, 31)
	if err != nil {
		return nil, fmt.Errorf("日域: %w", err)
	}
	month, err := parseField(fields[3], 1, 12)
	if err != nil {
		return nil, fmt.Errorf("月域: %w", err)
	}
	dow, dowStar, err := parseDayField(fields[4], 0, 6)
	if err != nil {
		return nil, fmt.Errorf("周域: %w", err)
	}
	return &Schedule{
		minute: minute, hour: hour, dom: dom, month: month, dow: dow,
		domStar: domStar, dowStar: dowStar,
	}, nil
}

// parseField 解析单域（* , - / 组合）。
func parseField(f string, lo, hi int) (map[int]bool, error) {
	set := map[int]bool{}
	for _, part := range strings.Split(f, ",") {
		vals, err := parsePart(part, lo, hi)
		if err != nil {
			return nil, err
		}
		for _, v := range vals {
			set[v] = true
		}
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("空域 %q", f)
	}
	return set, nil
}

// parseDayField 解析日/周域（额外回报是否为 *）。
func parseDayField(f string, lo, hi int) (map[int]bool, bool, error) {
	set, err := parseField(f, lo, hi)
	if err != nil {
		return nil, false, err
	}
	star := strings.TrimSpace(f) == "*"
	return set, star, nil
}

// parsePart 解析单个逗号片段（支持 N / N-M / N-M/S / */S）。
func parsePart(part string, lo, hi int) ([]int, error) {
	var rng string
	var step int = 1
	if i := strings.Index(part, "/"); i >= 0 {
		rng = part[:i]
		s, err := strconv.Atoi(part[i+1:])
		if err != nil || s <= 0 {
			return nil, fmt.Errorf("非法步长 %q", part)
		}
		step = s
	} else {
		rng = part
	}
	loB, hiB := lo, hi
	switch {
	case rng == "*" || rng == "":
		// 全域（rng=="" 仅当形如 "/5"，等价 */5）
	case strings.Contains(rng, "-"):
		b := strings.SplitN(rng, "-", 2)
		a, err1 := strconv.Atoi(b[0])
		c, err2 := strconv.Atoi(b[1])
		if err1 != nil || err2 != nil || a > c {
			return nil, fmt.Errorf("非法范围 %q", part)
		}
		loB, hiB = a, c
	default:
		a, err := strconv.Atoi(rng)
		if err != nil {
			return nil, fmt.Errorf("非法值 %q", part)
		}
		loB, hiB = a, a
	}
	if loB < lo || hiB > hi || loB > hiB {
		return nil, fmt.Errorf("值越界 %q（允许 %d-%d）", part, lo, hi)
	}
	var out []int
	for v := loB; v <= hiB; v += step {
		out = append(out, v)
	}
	return out, nil
}

// Match 判断 t 是否命中计划（秒级精度按分钟匹配）。
func (s *Schedule) Match(t time.Time) bool {
	if !s.minute[t.Minute()] || !s.hour[t.Hour()] || !s.month[int(t.Month())] {
		return false
	}
	// 标准 cron 语义：日与周均非 * 时取并集；其一为 * 时另一生效。
	domOK := s.dom[t.Day()]
	dowOK := s.dow[int(t.Weekday())]
	if s.domStar && s.dowStar {
		return true
	}
	if s.domStar {
		return dowOK
	}
	if s.dowStar {
		return domOK
	}
	return domOK || dowOK
}

// Next 计算晚于 from 的下一次触发时间（最多回溯 366*24*60 分钟）。
// @every 模式：from + 间隔向上取整到分钟（调度器传入上次触发时间或 now）。
func (s *Schedule) Next(from time.Time) time.Time {
	if s.every > 0 {
		next := from.Add(s.every)
		next = time.Date(next.Year(), next.Month(), next.Day(), next.Hour(), next.Minute(), 0, 0, next.Location())
		if !next.After(from) {
			next = next.Add(time.Minute)
		}
		return next
	}
	t := from.Truncate(time.Minute).Add(time.Minute)
	limit := from.AddDate(0, 0, 366)
	for t.Before(limit) {
		if s.Match(t) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}
}

// Describe 返回表达式的中文描述（前端列表展示；支持 @ 描述符）。
func Describe(expr string) string {
	expr = strings.TrimSpace(expr)
	switch strings.ToLower(expr) {
	case "@hourly":
		return "每小时整点"
	case "@daily", "@midnight":
		return "每天 00:00"
	case "@weekly":
		return "每周日 00:00"
	case "@monthly":
		return "每月 1 日 00:00"
	case "@yearly", "@annually":
		return "每年 1 月 1 日 00:00"
	}
	if strings.HasPrefix(strings.ToLower(expr), "@every ") {
		if d, err := time.ParseDuration(strings.TrimSpace(expr[len("@every "):])); err == nil {
			return "每 " + humanDuration(d)
		}
	}
	f := strings.Fields(expr)
	if len(f) != 5 {
		return expr
	}
	if expr == "* * * * *" {
		return "每分钟"
	}
	if f[2] == "*" && f[3] == "*" && f[4] == "*" {
		// 日/月/周全为 *：小时+分钟模式。
		if f[1] == "*" {
			if strings.HasPrefix(f[0], "*/") {
				return "每 " + strings.TrimPrefix(f[0], "*/") + " 分钟"
			}
			return "每小时第 " + f[0] + " 分"
		}
		return "每天 " + f[1] + ":" + f[0]
	}
	if f[3] == "*" && f[4] == "*" && f[2] != "*" {
		return "每月 " + f[2] + " 号 " + f[1] + ":" + f[0]
	}
	if f[2] == "*" && f[3] == "*" && f[4] != "*" {
		if w, ok := weekdayName(f[4]); ok {
			return "每周" + w + " " + f[1] + ":" + f[0]
		}
	}
	return expr
}

// weekdayName 周域数字转中文名（支持单值与逗号列表）。
func weekdayName(field string) (string, bool) {
	names := []string{"日", "一", "二", "三", "四", "五", "六"}
	parts := strings.Split(field, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 || n > 6 {
			return "", false
		}
		out = append(out, names[n])
	}
	return strings.Join(out, "、"), true
}

// humanDuration 时长转人类可读中文（@every 描述用）。
func humanDuration(d time.Duration) string {
	total := int(d.Minutes())
	if total%1440 == 0 && total >= 1440 {
		return strconv.Itoa(total/1440) + " 天"
	}
	if total%60 == 0 && total >= 60 {
		return strconv.Itoa(total/60) + " 小时"
	}
	if total > 60 {
		return strconv.Itoa(total/60) + " 小时 " + strconv.Itoa(total%60) + " 分"
	}
	return strconv.Itoa(total) + " 分钟"
}
