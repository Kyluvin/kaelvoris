// datasource.go：外部 MySQL 数据源连接（工作流 SQL 自定义节点的 JDBC 式数据源）。
//
// 设计要点（Kaelvoris 补丁 2026-10）：
//   - 连接配置存 ~/.kaelvoris/coze/datasources.json（含密码，桌面单机本地文件）
//   - 每个连接分配一个负数 ID（-900000000000000001 起递减），与内核雪花正数 ID
//     永不冲突；SQL 节点的 databaseInfoID < 0 即视为外部连接
//   - externalDB 装饰器包装内部 crossdomain.Database 默认实例：Execute（SQL 自定义
//     节点唯一入口）时按 ID 分流——负 ID 直连外部 MySQL 执行，正 ID 透传内部实现
//     （内部 SQLite 表/draft-online 转换逻辑原样保留）
//   - 内核 CustomSQL.Invoke 已把 {{var}} 模板渲染为 ? 占位符 + Params 参数化值，
//     外部 MySQL 原生支持 ? 占位符，直接透传执行
package cozeflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	crossdatabase "github.com/coze-dev/coze-studio/backend/crossdomain/database"
	database "github.com/coze-dev/coze-studio/backend/crossdomain/database/model"

	_ "github.com/go-sql-driver/mysql" // MySQL 驱动（go.mod 已有，仅注册）
)

// externalIDBase 外部连接 ID 基准：负数命名空间（内部雪花 ID 恒为正，永不冲突）。
const externalIDBase = int64(-900000000000000001)

// Datasource 外部 MySQL 连接配置（设置面板管理）。
type Datasource struct {
	ID        int64  `json:"id"`        // 负数 ID（即 SQL 节点 databaseInfoID，字符串化传输）
	Name      string `json:"name"`      // 连接名称（用户自定义，展示用）
	Host      string `json:"host"`      // 主机
	Port      int    `json:"port"`      // 端口（默认 3306）
	User      string `json:"user"`      // 用户名
	Password  string `json:"password"`  // 密码
	DBName    string `json:"dbName"`    // 库名
	TableName string `json:"tableName"` // 绑定表名（可选；填了才可用于查询/新增/更新/删除节点，SQL 自定义节点不需要）
}

// DSN 生成 go-sql-driver 连接串（对应 JDBC 的 jdbc:mysql://host:port/db）。
func (d *Datasource) DSN() string {
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&timeout=10s&readTimeout=60s",
		d.User, d.Password, d.Host, d.Port, d.DBName)
}

// JDBCURL 展示用 JDBC 连接串（前端回显对照）。
func (d *Datasource) JDBCURL() string {
	return fmt.Sprintf("jdbc:mysql://%s:%d/%s", d.Host, d.Port, d.DBName)
}

// ── 连接配置持久化（~/.kaelvoris/coze/datasources.json）──

var (
	dsMu    sync.Mutex      // 连接文件读写锁（panel 多绑定方法并发访问）
	dsNextID int64          // 下一个外部 ID（进程内递减；重启后由文件最大值续推）
)

func datasourcesFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kaelvoris", "coze", "datasources.json")
}

// LoadDatasources 读取连接列表；文件不存在/损坏视为空（跳过不致命）。
func LoadDatasources() []*Datasource {
	dsMu.Lock()
	defer dsMu.Unlock()
	return loadDatasourcesLocked()
}

func loadDatasourcesLocked() []*Datasource {
	bs, err := os.ReadFile(datasourcesFile())
	if err != nil {
		return nil
	}
	var out []*Datasource
	if err := json.Unmarshal(bs, &out); err != nil {
		return nil
	}
	// 续推下一个 ID：取现存最小 ID 再减一（负数递减，越减越小）
	for _, d := range out {
		if d.ID < dsNextID {
			dsNextID = d.ID
		}
	}
	return out
}

// SaveDatasources 落盘连接列表。
func SaveDatasources(list []*Datasource) error {
	dsMu.Lock()
	defer dsMu.Unlock()
	bs, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(datasourcesFile(), bs, 0o600) // 含密码，仅本用户可读
}

// UpsertDatasource 新增或更新连接（ID==0 时分配新负数 ID），返回最新列表与该连接。
func UpsertDatasource(ds *Datasource) ([]*Datasource, *Datasource, error) {
	dsMu.Lock()
	defer dsMu.Unlock()
	list := loadDatasourcesLocked()
	if ds.ID == 0 {
		ds.ID = dsNextID - 1 // 递减分配
		dsNextID = ds.ID
		list = append(list, ds)
	} else {
		found := false
		for i, d := range list {
			if d.ID == ds.ID {
				list[i] = ds
				found = true
				break
			}
		}
		if !found {
			return nil, nil, fmt.Errorf("连接不存在（ID=%d）", ds.ID)
		}
	}
	if err := SaveDatasourcesLocked(list); err != nil {
		return nil, nil, err
	}
	return list, ds, nil
}

// SaveDatasourcesLocked 落盘（调用方已持锁）。
func SaveDatasourcesLocked(list []*Datasource) error {
	bs, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(datasourcesFile(), bs, 0o600)
}

// RemoveDatasource 按负数 ID 删除连接，返回最新列表。
func RemoveDatasource(id int64) ([]*Datasource, error) {
	dsMu.Lock()
	defer dsMu.Unlock()
	list := loadDatasourcesLocked()
	out := list[:0]
	for _, d := range list {
		if d.ID != id {
			out = append(out, d)
		}
	}
	return out, SaveDatasourcesLocked(out)
}

// TestDatasource 测试连接（sql.Open + Ping，超时 10s）。
func TestDatasource(ds *Datasource) error {
	if ds.Host == "" || ds.DBName == "" {
		return fmt.Errorf("主机与库名不能为空")
	}
	db, err := sql.Open("mysql", ds.DSN())
	if err != nil {
		return err
	}
	defer db.Close()
	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return db.PingContext(pingCtx)
}

// GetDatasource 按负数 ID 查找连接（Decorator 执行时用）。
func GetDatasource(id int64) *Datasource {
	for _, d := range LoadDatasources() {
		if d.ID == id {
			return d
		}
	}
	return nil
}

// ── externalDB 装饰器：SQL 自定义节点按 ID 分流（外部 MySQL / 内部实现）──

// externalDB 包装内部 crossdomain.Database；仅 override Execute（SQL 自定义节点入口），
// 其余方法（Query/Update/Insert/Delete/MGet…）透传内部实现。
type externalDB struct {
	crossdatabase.Database // 嵌入内部默认实例（未覆盖方法透传）
}

// Execute SQL 自定义节点执行入口：
//   - DatabaseInfoID < 0 → 外部 MySQL 连接：参数化执行（? 占位符 + Params），
//     SELECT/SHOW 类走 Query 返回行集，其余走 Exec 返回影响行数
//   - DatabaseInfoID > 0 → 透传内部实现（内部 SQLite 表，含 DebugRun draft 转换）
func (e *externalDB) Execute(ctx context.Context, request *database.CustomSQLRequest) (*database.Response, error) {
	if request.DatabaseInfoID >= 0 {
		return e.Database.Execute(ctx, request) // 内部表：原链路
	}
	return e.executeExternal(ctx, request)
}

// Query 查询数据节点（结构化条件 → SELECT；外部 MySQL）
func (e *externalDB) Query(ctx context.Context, request *database.QueryRequest) (*database.Response, error) {
	if request.DatabaseInfoID >= 0 {
		return e.Database.Query(ctx, request)
	}
	ds, table, err := e.externalTable(request.DatabaseInfoID)
	if err != nil {
		return nil, err
	}
	sqlStr := "SELECT * FROM " + table
	args := []any{}
	if len(request.SelectFields) > 0 { // 指定列（列名白名单过滤）
		cols := make([]string, 0, len(request.SelectFields))
		for _, f := range request.SelectFields {
			if c := safeIdent(f); c != "" {
				cols = append(cols, c)
			}
		}
		if len(cols) > 0 {
			sqlStr = "SELECT " + strings.Join(cols, ", ") + " FROM " + table
		}
	}
	if len(request.ConditionGroup.Conditions) > 0 {
		where, wArgs := buildWhere(request.ConditionGroup)
		sqlStr += " WHERE " + where
		args = append(args, wArgs...)
	}
	for _, oc := range request.OrderClauses { // 排序（列名白名单过滤，跳过非法项）
		if c := safeIdent(oc.FieldID); c != "" {
			dir := "ASC"
			if !oc.IsAsc {
				dir = "DESC"
			}
			if !strings.Contains(sqlStr, " ORDER BY ") {
				sqlStr += " ORDER BY "
			} else {
				sqlStr += ", "
			}
			sqlStr += c + " " + dir
		}
	}
	if request.Limit > 0 {
		sqlStr += fmt.Sprintf(" LIMIT %d", request.Limit)
	}
	return e.runQuery(ds, sqlStr, args)
}

// Insert 新增数据节点（字段映射 → INSERT；外部 MySQL）
func (e *externalDB) Insert(ctx context.Context, request *database.InsertRequest) (*database.Response, error) {
	if request.DatabaseInfoID >= 0 {
		return e.Database.Insert(ctx, request)
	}
	ds, table, err := e.externalTable(request.DatabaseInfoID)
	if err != nil {
		return nil, err
	}
	if len(request.Fields) == 0 {
		return nil, fmt.Errorf("插入字段为空")
	}
	cols, ph, args := make([]string, 0, len(request.Fields)), make([]string, 0, len(request.Fields)), make([]any, 0, len(request.Fields))
	for _, k := range sortedKeys(request.Fields) {
		c := safeIdent(k)
		if c == "" {
			continue
		}
		cols, ph = append(cols, c), append(ph, "?")
		args = append(args, request.Fields[k])
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("插入字段名全部非法")
	}
	sqlStr := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, strings.Join(cols, ", "), strings.Join(ph, ", "))
	return e.runExec(ds, sqlStr, args)
}

// Update 更新数据节点（SET 字段 + WHERE 条件 → UPDATE；外部 MySQL）
func (e *externalDB) Update(ctx context.Context, request *database.UpdateRequest) (*database.Response, error) {
	if request.DatabaseInfoID >= 0 {
		return e.Database.Update(ctx, request)
	}
	ds, table, err := e.externalTable(request.DatabaseInfoID)
	if err != nil {
		return nil, err
	}
	if len(request.Fields) == 0 {
		return nil, fmt.Errorf("更新字段为空")
	}
	sets, args := make([]string, 0, len(request.Fields)), make([]any, 0, len(request.Fields))
	for _, k := range sortedKeys(request.Fields) {
		c := safeIdent(k)
		if c == "" {
			continue
		}
		sets = append(sets, c+" = ?")
		args = append(args, request.Fields[k])
	}
	if len(sets) == 0 {
		return nil, fmt.Errorf("更新字段名全部非法")
	}
	sqlStr := "UPDATE " + table + " SET " + strings.Join(sets, ", ")
	if len(request.ConditionGroup.Conditions) > 0 {
		where, wArgs := buildWhere(request.ConditionGroup)
		sqlStr += " WHERE " + where
		args = append(args, wArgs...)
	}
	return e.runExec(ds, sqlStr, args)
}

// Delete 删除数据节点（WHERE 条件 → DELETE；外部 MySQL）
func (e *externalDB) Delete(ctx context.Context, request *database.DeleteRequest) (*database.Response, error) {
	if request.DatabaseInfoID >= 0 {
		return e.Database.Delete(ctx, request)
	}
	ds, table, err := e.externalTable(request.DatabaseInfoID)
	if err != nil {
		return nil, err
	}
	sqlStr := "DELETE FROM " + table
	args := []any{}
	if len(request.ConditionGroup.Conditions) > 0 {
		where, wArgs := buildWhere(request.ConditionGroup)
		sqlStr += " WHERE " + where
		args = append(args, wArgs...)
	}
	return e.runExec(ds, sqlStr, args)
}

// externalTable 负 ID → 连接 + 表名（TableName 未配置则报错引导）。
func (e *externalDB) externalTable(id int64) (*Datasource, string, error) {
	ds := GetDatasource(id)
	if ds == nil {
		return nil, "", fmt.Errorf("外部数据源不存在（ID=%d），请到「设置 → 数据源」检查", id)
	}
	table := safeIdent(ds.TableName)
	if table == "" {
		return nil, "", fmt.Errorf("数据源「%s」未配置表名：请到「设置 → 数据源」编辑该连接并填写表名", ds.Name)
	}
	return ds, table, nil
}

// buildWhere 条件组 → WHERE 片段（列名白名单 + 值参数化；支持 and/or 组合）。
func buildWhere(g *database.ConditionGroup) (string, []any) {
	args := make([]any, 0, len(g.Conditions))
	items := make([]string, 0, len(g.Conditions))
	for _, c := range g.Conditions {
		col := safeIdent(c.Left)
		if col == "" {
			continue
		}
		switch c.Operator {
		case database.OperatorIsNull:
			items = append(items, col+" IS NULL")
		case database.OperatorIsNotNull:
			items = append(items, col+" IS NOT NULL")
		case database.OperatorLike:
			items = append(items, col+" LIKE ?")
			args = append(args, c.Right)
		case database.OperatorNotLike:
			items = append(items, col+" NOT LIKE ?")
			args = append(args, c.Right)
		case database.OperatorIn, database.OperatorNotIn:
			// 右值支持数组或逗号分隔字符串 → IN (?, ?, ...)
			vals := toSlice(c.Right)
			if len(vals) == 0 {
				continue
			}
			ph := make([]string, 0, len(vals))
			for _, v := range vals {
				ph = append(ph, "?")
				args = append(args, v)
			}
			op := "IN"
			if c.Operator == database.OperatorNotIn {
				op = "NOT IN"
			}
			items = append(items, fmt.Sprintf("%s %s (%s)", col, op, strings.Join(ph, ", ")))
		default: // = / != / > / < / >= / <=
			items = append(items, fmt.Sprintf("%s %s ?", col, string(c.Operator)))
			args = append(args, c.Right)
		}
	}
	relation := " AND "
	if g.Relation == database.ClauseRelationOR {
		relation = " OR "
	}
	return strings.Join(items, relation), args
}

// safeIdent 标识符白名单（字母/数字/下划线，防 SQL 注入；表名/列名共用）。
func safeIdent(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 64 {
		return ""
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') {
			return ""
		}
	}
	return s
}

// sortedKeys map 键排序（保证 SQL 字段顺序稳定，便于测试与比对）。
func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// toSlice 右值 → 值切片（IN/NOT IN 用；数组或逗号分隔字符串）。
func toSlice(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case []string:
		out := make([]any, 0, len(t))
		for _, s := range t {
			out = append(out, s)
		}
		return out
	case string:
		parts := strings.Split(t, ",")
		out := make([]any, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	default:
		if v == nil {
			return nil
		}
		return []any{v}
	}
}

// runQuery 执行 SELECT 类语句 → 行集 Response（objects 键 = 列名）。
func (e *externalDB) runQuery(ds *Datasource, sqlStr string, args []any) (*database.Response, error) {
	db, err := sql.Open("mysql", ds.DSN())
	if err != nil {
		return nil, fmt.Errorf("打开 MySQL 连接失败: %w", err)
	}
	defer db.Close()
	execCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rows, err := db.QueryContext(execCtx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("MySQL 查询失败: %w", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("读取列信息失败: %w", err)
	}
	objects := make([]database.Object, 0, 64)
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("读取行数据失败: %w", err)
		}
		obj := make(database.Object, len(cols))
		for i, c := range cols {
			obj[c] = normalizeSQLValue(vals[i])
		}
		objects = append(objects, obj)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历结果失败: %w", err)
	}
	rowNum := int64(len(objects))
	return &database.Response{RowNumber: &rowNum, Objects: objects}, nil
}

// runExec 执行写类语句 → 影响行数 Response。
func (e *externalDB) runExec(ds *Datasource, sqlStr string, args []any) (*database.Response, error) {
	db, err := sql.Open("mysql", ds.DSN())
	if err != nil {
		return nil, fmt.Errorf("打开 MySQL 连接失败: %w", err)
	}
	defer db.Close()
	execCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := db.ExecContext(execCtx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("MySQL 执行失败: %w", err)
	}
	affected, _ := res.RowsAffected()
	rowNum := affected
	return &database.Response{RowNumber: &rowNum, Objects: []database.Object{}}, nil
}

// executeExternal 外部 MySQL 执行：查询类返回行集（Objects），写类返回影响行数。
func (e *externalDB) executeExternal(_ context.Context, request *database.CustomSQLRequest) (*database.Response, error) {
	ds := GetDatasource(request.DatabaseInfoID)
	if ds == nil {
		return nil, fmt.Errorf("外部数据源不存在（ID=%d），请到「设置 → 数据源」检查", request.DatabaseInfoID)
	}
	db, err := sql.Open("mysql", ds.DSN())
	if err != nil {
		return nil, fmt.Errorf("打开 MySQL 连接失败: %w", err)
	}
	defer db.Close()
	execCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 参数化值：内核传来的 Params 与 SQL 中 ? 占位符一一对应
	args := make([]any, 0, len(request.Params))
	for _, p := range request.Params {
		if p.IsNull {
			args = append(args, nil)
			continue
		}
		args = append(args, p.Value)
	}

	upper := strings.ToUpper(strings.TrimSpace(request.SQL))
	isQuery := strings.HasPrefix(upper, "SELECT") || strings.HasPrefix(upper, "SHOW") ||
		strings.HasPrefix(upper, "DESC") || strings.HasPrefix(upper, "EXPLAIN") ||
		strings.HasPrefix(upper, "WITH")

	if !isQuery {
		res, err := db.ExecContext(execCtx, request.SQL, args...)
		if err != nil {
			return nil, fmt.Errorf("MySQL 执行失败: %w", err)
		}
		affected, _ := res.RowsAffected()
		rowNum := affected
		return &database.Response{RowNumber: &rowNum, Objects: []database.Object{}}, nil
	}

	rows, err := db.QueryContext(execCtx, request.SQL, args...)
	if err != nil {
		return nil, fmt.Errorf("MySQL 查询失败: %w", err)
	}
	defer rows.Close()

	// 行集 → Objects（列名 → 值；[]byte 统一转 string，保证 JSON 可序列化）
	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("读取列信息失败: %w", err)
	}
	objects := make([]database.Object, 0, 64)
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("读取行数据失败: %w", err)
		}
		obj := make(database.Object, len(cols))
		for i, c := range cols {
			obj[c] = normalizeSQLValue(vals[i])
		}
		objects = append(objects, obj)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历结果失败: %w", err)
	}
	rowNum := int64(len(objects))
	return &database.Response{RowNumber: &rowNum, Objects: objects}, nil
}

// normalizeSQLValue 驱动返回值归一化（[]byte → string；其余原样）。
func normalizeSQLValue(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case []byte:
		return string(t)
	default:
		return v
	}
}
