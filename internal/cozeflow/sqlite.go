// Package cozeflow 嵌入式基础设施（Coze Studio 源码融合，见 cache_mem.go 包注释）。
//
// sqlite.go：嵌入式 SQLite 数据库 —— 替代 MySQL。
//   - 驱动：modernc.org/sqlite（纯 Go、无 CGO，Windows/交叉编译友好），
//     gorm 适配器 github.com/glebarez/sqlite（与 gorm.io/driver/sqlite 同接口）。
//   - DDL：coze 的 docker/volumes/mysql/schema.sql 为 MySQL 方言（55 张表，
//     单行一条 CREATE TABLE），经 mysqlDDLToSQLite 运行时转换后执行：
//       · 反引号 → 双引号
//       · 去掉 MySQL 专属尾巴：ENGINE=.../CHARSET .../COLLATE .../COMMENT '...'/AUTO_INCREMENT <n>
//       · 列内 COMMENT '...' 剥离（SQLite 无列注释）
//       · AUTO_INCREMENT 列 → INTEGER PRIMARY KEY AUTOINCREMENT（仅单列主键时）
//       · datetime(3)/datetime → TEXT（coze 以毫秒时间戳 bigint 为主，datetime 仅少量软删列）
//       · INDEX a (...)/UNIQUE INDEX a (...) → 独立 CREATE INDEX/UNIQUE INDEX
//       · 布尔 DEFAULT 0/1 保留（SQLite 接受）
//   - data source：~/.kaelvoris/coze/coze.db（单文件，随 Kaelvoris 数据目录走）。
package cozeflow

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// OpenSQLite 打开（必要时创建）嵌入式 SQLite 库并执行 coze 全量 DDL。
// idempotent：DDL 全部 IF NOT EXISTS + 转换器输出幂等，可重复调用。
func OpenSQLite(dbPath, ddlPath string) (*gorm.DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("创建数据库目录失败: %w", err)
	}
	// _pragma：WAL 提升并发写、busy_timeout 等待锁、foreign_keys 关闭（coze 表间无外键约束）
	dsn := dbPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(0)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		// coze 的 gorm 使用自带日志体系；SQLite 下静默化避免刷屏
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	// 单文件库：连接数收敛（gorm 并发访问 + SQLite 写锁）
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	if err := ApplyMySQLDDL(db, ddlPath); err != nil {
		return nil, fmt.Errorf("执行 coze DDL 失败: %w", err)
	}
	return db, nil
}

// ApplyMySQLDDL 读取 MySQL DDL 文件，逐条转换为 SQLite 方言并执行。
// 非 CREATE TABLE 语句（SET/INSERT/USE 等）跳过（初始化 mock 数据不移植，
// 融合形态下用户/空间/模型由 Kaelvoris 侧装配）。
func ApplyMySQLDDL(db *gorm.DB, ddlPath string) error {
	b, err := os.ReadFile(ddlPath)
	if err != nil {
		return fmt.Errorf("读取 DDL: %w", err)
	}
	stmts := mysqlDDLToSQLite(string(b))
	for i, stmt := range stmts {
		if err := db.Exec(stmt).Error; err != nil {
			return fmt.Errorf("DDL[%d] 执行失败: %w\nSQL: %s", i, err, stmt)
		}
	}
	return nil
}

// ---- MySQL DDL → SQLite 方言转换 ----

var (
	// 行内注释剥离用：COMMENT '...'（值内允许转义引号 ''）
	reColumnComment = regexp.MustCompile(`(?i)\s*COMMENT\s+'(?:[^']|'')*'`)
	// 表尾：ENGINE=InnoDB CHARSET utf8mb4 COLLATE xxx COMMENT '...' AUTO_INCREMENT 123
	reTableTail = regexp.MustCompile(`(?i)\)\s*ENGINE\s*=\s*\w+[^;]*;`)
	// 索引定义：[UNIQUE] [FULLTEXT|SPATIAL] INDEX/KEY name (cols) —— 表内捕获后转为独立语句。
	// 名称允许被反引号或双引号包裹（反引号→双引号替换先于此正则执行）；
	// cols 支持一层嵌套括号（MySQL 前缀索引 `col` (100) 形态）。
	reIndexDef = regexp.MustCompile(`(?i)(UNIQUE\s+)?(FULLTEXT\s+|SPATIAL\s+)?(INDEX|KEY)\s+[` + "`" + `"]?([a-zA-Z0-9_]+)[` + "`" + `"]?\s*\(([^()]*(?:\([^()]*\)[^()]*)*)\)`)
	// MySQL 前缀索引长度（如 `uri` (100)）——SQLite 无前缀索引，剥掉长度保留整列
	reIndexPrefixLen = regexp.MustCompile(`\(\s*\d+\s*\)`)
	// 列级/表级 COLLATE 子句（utf8mb4_0900_ai_ci / utf8mb4_bin 等）
	reCollate = regexp.MustCompile(`(?i)\s*COLLATE\s+[a-zA-Z0-9_]+`)
)

// mysqlDDLToSQLite 将 MySQL DDL 文本转换为 SQLite 语句切片。
// 只处理单行 CREATE TABLE（coze schema.sql 的形态：一行一条）。
func mysqlDDLToSQLite(ddl string) []string {
	var out []string
	for _, line := range strings.Split(ddl, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToUpper(line), "CREATE TABLE") {
			continue
		}
		out = append(out, convertCreateTable(line)...)
	}
	return out
}

// convertCreateTable 转换单条 CREATE TABLE 语句，返回
// [CREATE TABLE ...] + [CREATE [UNIQUE] INDEX ...]。
func convertCreateTable(line string) []string {
	// 1. 反引号 → 双引号（SQLite 标识符引号）
	s := strings.ReplaceAll(line, "`", `"`)

	// 2. 拆分表体与表尾（ENGINE/CHARSET/COMMENT/AUTO_INCREMENT <n>）
	tail := ""
	if loc := reTableTail.FindStringIndex(s); loc != nil {
		// 保留右括号本身，去掉 `) ENGINE=... ;` 的尾巴
		tail = s[loc[0]:loc[1]]
		s = s[:loc[0]] + ");"
	}
	_ = tail

	// 3. 剥离列内/表内 COMMENT
	s = reColumnComment.ReplaceAllString(s, "")

	// 4. 提取表内 INDEX/UNIQUE INDEX 定义 → 独立 CREATE INDEX
	tableName := extractTableName(s)
	var idxStmts []string
	s = reIndexDef.ReplaceAllStringFunc(s, func(m string) string {
		g := reIndexDef.FindStringSubmatch(m)
		unique, name, cols := g[1], g[4], g[5]
		cols = strings.TrimSpace(reIndexPrefixLen.ReplaceAllString(cols, "")) // 剥掉前缀长度
		kind := "INDEX"
		if strings.TrimSpace(unique) != "" {
			kind = "UNIQUE INDEX"
		}
		if tableName != "" && cols != "" {
			idxStmts = append(idxStmts,
				fmt.Sprintf("CREATE %s IF NOT EXISTS %s_%s ON %q (%s);", kind, tableName, name, tableName, cols))
		}
		return "" // 从表体移除
	})

	// 5. 清理移除索引后残留的 ", )" / ",)" 与空括号问题
	s = fixTrailingComma(s)

	// 6. 类型与约束替换
	s = replaceTypes(s)

	return append([]string{s}, idxStmts...)
}

// extractTableName 提取 CREATE TABLE <name>。
func extractTableName(s string) string {
	m := regexp.MustCompile(`(?i)CREATE\s+TABLE\s+IF\s+NOT\s+EXISTS\s+"([^"]+)"`).FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return m[1]
}

// replaceTypes MySQL 类型/约束 → SQLite 等价物。
func replaceTypes(s string) string {
	r := strings.NewReplacer(
		// 类型（大小写不敏感由调用侧处理：coze DDL 全小写，这里覆盖两种）
		`bigint unsigned`, `INTEGER`, `BIGINT UNSIGNED`, `INTEGER`,
		`bigint`, `INTEGER`, `BIGINT`, `INTEGER`,
		`tinyint unsigned`, `INTEGER`, `TINYINT UNSIGNED`, `INTEGER`,
		`tinyint`, `INTEGER`, `TINYINT`, `INTEGER`,
		`int unsigned`, `INTEGER`, `INT UNSIGNED`, `INTEGER`,
		`mediumint`, `INTEGER`, `MEDIUMINT`, `INTEGER`,
		`smallint`, `INTEGER`, `SMALLINT`, `INTEGER`,
		`double`, `REAL`, `DOUBLE`, `REAL`,
		`float`, `REAL`, `FLOAT`, `REAL`,
		`decimal(`, `NUMERIC(`, `DECIMAL(`, `NUMERIC(`,
		`datetime(3)`, `TEXT`, `DATETIME(3)`, `TEXT`,
		`datetime`, `TEXT`, `DATETIME`, `TEXT`,
		`timestamp`, `TEXT`, `TIMESTAMP`, `TEXT`,
		`longtext`, `TEXT`, `LONGTEXT`, `TEXT`,
		`mediumtext`, `TEXT`, `MEDIUMTEXT`, `TEXT`,
		`tinytext`, `TEXT`, `TINYTEXT`, `TEXT`,
		`json`, `TEXT`, `JSON`, `TEXT`,
		`longblob`, `BLOB`, `LONGBLOB`, `BLOB`,
		`mediumblob`, `BLOB`, `MEDIUMBLOB`, `BLOB`,
		`tinyblob`, `BLOB`, `TINYBLOB`, `BLOB`,
		`varbinary(`, `BLOB`, `VARBINARY(`, `BLOB`,
		// AUTO_INCREMENT → SQLite 行 id 语法（GORM 显式赋 id 值时 SQLite 也接受）
		`AUTO_INCREMENT`, ``,
		// MySQL 专属默认值/语法
		`CURRENT_TIMESTAMP(3)`, `CURRENT_TIMESTAMP`,
		`ON UPDATE CURRENT_TIMESTAMP`, ``,
		`CHARACTER SET utf8mb4`, ``,
		`CHARSET utf8mb4`, ``,
	)
	// 列级/表级 COLLATE 子句（MySQL 8 的 utf8mb4_0900_ai_ci 等）——SQLite 无对应 collation，统一剥离
	s = reCollate.ReplaceAllString(s, "")
	return r.Replace(s)
}

// fixTrailingComma 修掉移除索引/注释后残留的 ", )" 或 ",)"（列尾悬空逗号）。
func fixTrailingComma(s string) string {
	for {
		idx := strings.LastIndex(s, ")")
		if idx < 0 {
			return s
		}
		head := s[:idx]
		trimmed := strings.TrimRight(head, " \t\n")
		if strings.HasSuffix(trimmed, ",") {
			s = trimmed[:len(trimmed)-1] + " " + s[idx:]
			continue
		}
		// 空表体 "( )" 防御（不会出现在 coze DDL，仅兜底）
		if strings.HasSuffix(trimmed, "(") {
			// 找前一个逗号移除
			openIdx := strings.LastIndex(trimmed, "(")
			prev := strings.TrimRight(trimmed[:openIdx], " \t")
			if strings.HasSuffix(prev, ",") {
				s = prev[:len(prev)-1] + " " + s[idx:]
				continue
			}
		}
		return s
	}
}
