/*
 * Copyright 2025 coze-dev Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package rdb

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"gorm.io/gorm"

	"github.com/coze-dev/coze-studio/backend/infra/idgen"
	"github.com/coze-dev/coze-studio/backend/infra/rdb"
	"github.com/coze-dev/coze-studio/backend/infra/rdb/entity"
	"github.com/coze-dev/coze-studio/backend/infra/sqlparser"
	"github.com/coze-dev/coze-studio/backend/pkg/logs"
)

type mysqlService struct {
	db        *gorm.DB
	generator idgen.IDGenerator
}

func NewService(db *gorm.DB, generator idgen.IDGenerator) rdb.RDB {
	return &mysqlService{db: db, generator: generator}
}

// CreateTable create table
func (m *mysqlService) CreateTable(ctx context.Context, req *rdb.CreateTableRequest) (*rdb.CreateTableResponse, error) {
	// Kaelvoris 融合改造（2026-10）：嵌入式形态底层为 SQLite（glebarez/modernc），
	// 建表 DDL 需切 SQLite 方言（AUTO_INCREMENT/内联 KEY/COMMENT/表选项均不兼容）。
	if m.isSQLite() {
		return m.createTableSQLite(ctx, req)
	}
	if req == nil || req.Table == nil {
		return nil, fmt.Errorf("invalid request")
	}

	// build column definitions
	columnDefs := make([]string, 0, len(req.Table.Columns))
	for _, col := range req.Table.Columns {
		if !isValidIdentifier(col.Name) {
			return nil, fmt.Errorf("invalid column name format: %s", col.Name)
		}
		colDef := fmt.Sprintf("`%s` %s", col.Name, col.DataType)

		if col.Length != nil {
			colDef += fmt.Sprintf("(%d)", *col.Length)
		} else if col.Length == nil && col.DataType == entity.TypeVarchar {
			colDef += fmt.Sprintf("(%d)", 255)
		}

		if col.NotNull {
			colDef += " NOT NULL"
		}
		if col.DefaultValue != nil {
			if col.DataType == entity.TypeTimestamp {
				colDef += fmt.Sprintf(" DEFAULT %s", *col.DefaultValue)
			} else if col.DataType == entity.TypeText {
				// do nothing
			} else {
				colDef += fmt.Sprintf(" DEFAULT '%s'", escapeString(*col.DefaultValue))
			}
		}
		if col.AutoIncrement {
			colDef += " AUTO_INCREMENT"
		}
		if col.Comment != nil {
			colDef += fmt.Sprintf(" COMMENT '%s'", escapeString(*col.Comment))
		}

		columnDefs = append(columnDefs, colDef)
	}

	// build index definitions
	for _, idx := range req.Table.Indexes {
		if idx.Name != "" && !isValidIdentifier(idx.Name) {
			return nil, fmt.Errorf("invalid index name format: %s", idx.Name)
		}
		for _, col := range idx.Columns {
			if !isValidIdentifier(col) {
				return nil, fmt.Errorf("invalid column name in index: %s", col)
			}
		}

		var idxDef string
		switch idx.Type {
		case entity.PrimaryKey:
			idxDef = fmt.Sprintf("PRIMARY KEY (`%s`)", strings.Join(idx.Columns, "`,`"))
		case entity.UniqueKey:
			idxDef = fmt.Sprintf("UNIQUE KEY `%s` (`%s`)", idx.Name, strings.Join(idx.Columns, "`,`"))
		default:
			idxDef = fmt.Sprintf("KEY `%s` (`%s`)", idx.Name, strings.Join(idx.Columns, "`,`"))
		}
		columnDefs = append(columnDefs, idxDef)
	}

	tableOptions := make([]string, 0)
	if req.Table.Options != nil {
		if req.Table.Options.Collate != nil {
			tableOptions = append(tableOptions, fmt.Sprintf("COLLATE=%s", *req.Table.Options.Collate))
		}
		if req.Table.Options.AutoIncrement != nil {
			tableOptions = append(tableOptions, fmt.Sprintf("AUTO_INCREMENT=%d", *req.Table.Options.AutoIncrement))
		}
		if req.Table.Options.Comment != nil {
			tableOptions = append(tableOptions, fmt.Sprintf("COMMENT='%s'", escapeString(*req.Table.Options.Comment)))
		}
	}

	tableName := req.Table.Name
	if req.Table.Name == "" {
		genName, err := m.genTableName(ctx)
		if err != nil {
			return nil, err
		}

		tableName = genName
	}

	if !isValidIdentifier(tableName) {
		return nil, fmt.Errorf("invalid table name format: %s", tableName)
	}

	createSQL := fmt.Sprintf("CREATE TABLE IF NOT EXISTS `%s` (\n  %s\n) %s",
		tableName,
		strings.Join(columnDefs, ",\n  "),
		strings.Join(tableOptions, " "),
	)

	logs.CtxInfof(ctx, "[CreateTable] execute sql is %s, req is %v", createSQL, req)

	err := m.db.WithContext(ctx).Exec(createSQL).Error
	if err != nil {
		return nil, fmt.Errorf("failed to create table: %v", err)
	}

	resTable := req.Table
	resTable.Name = tableName
	return &rdb.CreateTableResponse{Table: resTable}, nil
}

func isValidIdentifier(name string) bool {
	match, _ := regexp.MatchString(`^[a-zA-Z_][a-zA-Z0-9_]*$`, name)
	return match
}

func escapeString(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}

// AlterTable alter table
func (m *mysqlService) AlterTable(ctx context.Context, req *rdb.AlterTableRequest) (*rdb.AlterTableResponse, error) {
	// Kaelvoris 融合改造（2026-10）：SQLite ALTER 语法受限（无 MODIFY/ADD PRIMARY KEY，
	// ADD INDEX 需独立语句），走专用分支。
	if m.isSQLite() {
		return m.alterTableSQLite(ctx, req)
	}
	if req == nil || len(req.Operations) == 0 {
		return nil, fmt.Errorf("invalid request")
	}

	alterSQL := fmt.Sprintf("ALTER TABLE `%s`", req.TableName)
	operations := make([]string, 0, len(req.Operations))

	for _, op := range req.Operations {
		switch op.Action {
		case entity.AddColumn:
			if op.Column == nil {
				return nil, fmt.Errorf("column is required for ADD COLUMN operation")
			}
			colDef := fmt.Sprintf("ADD COLUMN `%s` %s", op.Column.Name, op.Column.DataType)
			if op.Column.Length != nil {
				colDef += fmt.Sprintf("(%d)", *op.Column.Length)
			} else if op.Column.Length == nil && op.Column.DataType == entity.TypeVarchar {
				colDef += fmt.Sprintf("(%d)", 255)
			}

			if op.Column.NotNull {
				colDef += " NOT NULL"
			}

			if op.Column.DefaultValue != nil {
				if op.Column.DataType == entity.TypeTimestamp {
					colDef += fmt.Sprintf(" DEFAULT %s", *op.Column.DefaultValue)
				} else {
					colDef += fmt.Sprintf(" DEFAULT '%s'", *op.Column.DefaultValue)
				}
			}

			operations = append(operations, colDef)

		case entity.DropColumn:
			if op.Column == nil {
				return nil, fmt.Errorf("column is required for DROP COLUMN operation")
			}
			operations = append(operations, fmt.Sprintf("DROP COLUMN `%s`", op.Column.Name))

		case entity.ModifyColumn:
			if op.Column == nil {
				return nil, fmt.Errorf("column is required for MODIFY COLUMN operation")
			}
			colDef := fmt.Sprintf("MODIFY COLUMN `%s` %s", op.Column.Name, op.Column.DataType)
			if op.Column.Length != nil {
				colDef += fmt.Sprintf("(%d)", *op.Column.Length)
			} else if op.Column.Length == nil && op.Column.DataType == entity.TypeVarchar {
				colDef += fmt.Sprintf("(%d)", 255)
			}
			operations = append(operations, colDef)

		case entity.RenameColumn:
			if op.Column == nil || op.OldName == nil {
				return nil, fmt.Errorf("column and old name are required for RENAME COLUMN operation")
			}
			operations = append(operations, fmt.Sprintf("RENAME COLUMN `%s` TO `%s`", *op.OldName, op.Column.Name))

		case entity.AddIndex:
			if op.Index == nil {
				return nil, fmt.Errorf("index is required for ADD INDEX operation")
			}
			var idxDef string
			switch op.Index.Type {
			case entity.PrimaryKey:
				idxDef = fmt.Sprintf("ADD PRIMARY KEY (`%s`)", strings.Join(op.Index.Columns, "`,`"))
			case entity.UniqueKey:
				idxDef = fmt.Sprintf("ADD UNIQUE INDEX `%s` (`%s`)", op.Index.Name, strings.Join(op.Index.Columns, "`,`"))
			default:
				idxDef = fmt.Sprintf("ADD INDEX `%s` (`%s`)", op.Index.Name, strings.Join(op.Index.Columns, "`,`"))
			}
			operations = append(operations, idxDef)
		}
	}

	alterSQL += " " + strings.Join(operations, ", ")

	logs.CtxInfof(ctx, "[AlterTable] execute sql is %s, req is %v", alterSQL, req)

	err := m.db.WithContext(ctx).Exec(alterSQL).Error
	if err != nil {
		return nil, fmt.Errorf("failed to alter table: %v", err)
	}

	table, err := m.getTableInfo(ctx, req.TableName)
	if err != nil {
		return nil, fmt.Errorf("failed to get table info: %v", err)
	}

	return &rdb.AlterTableResponse{Table: table}, nil
}

// DropTable drop table
func (m *mysqlService) DropTable(ctx context.Context, req *rdb.DropTableRequest) (*rdb.DropTableResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("invalid request")
	}

	dropSQL := "DROP TABLE"
	if req.IfExists {
		dropSQL += " IF EXISTS"
	}
	dropSQL += fmt.Sprintf(" `%s`", req.TableName)

	logs.CtxInfof(ctx, "[DropTable] execute sql is %s, req is %v", dropSQL, req)

	err := m.db.WithContext(ctx).Exec(dropSQL).Error
	if err != nil {
		return nil, fmt.Errorf("failed to drop table: %v", err)
	}

	return &rdb.DropTableResponse{Success: true}, nil
}

// GetTable get table schema info
func (m *mysqlService) GetTable(ctx context.Context, req *rdb.GetTableRequest) (*rdb.GetTableResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("invalid request")
	}

	table, err := m.getTableInfo(ctx, req.TableName)
	if err != nil {
		return nil, err
	}

	return &rdb.GetTableResponse{Table: table}, nil
}

func (m *mysqlService) InsertData(ctx context.Context, req *rdb.InsertDataRequest) (*rdb.InsertDataResponse, error) {
	if req == nil || len(req.Data) == 0 {
		return nil, fmt.Errorf("invalid request")
	}

	fields := make([]string, 0)
	for field := range req.Data[0] {
		fields = append(fields, field)
	}

	const batchSize = 1000
	var totalAffected int64

	for i := 0; i < len(req.Data); i += batchSize {
		end := i + batchSize
		if end > len(req.Data) {
			end = len(req.Data)
		}

		currentBatch := req.Data[i:end]

		placeholderGroups := make([]string, 0, len(currentBatch))
		values := make([]interface{}, 0, len(currentBatch)*len(fields))

		for _, row := range currentBatch {
			placeholders := make([]string, len(fields))
			for j := range placeholders {
				placeholders[j] = "?"
			}
			placeholderGroups = append(placeholderGroups, "("+strings.Join(placeholders, ",")+")")

			for _, field := range fields {
				values = append(values, row[field])
			}
		}

		insertSQL := fmt.Sprintf("INSERT INTO `%s` (`%s`) VALUES %s",
			req.TableName,
			strings.Join(fields, "`,`"),
			strings.Join(placeholderGroups, ","),
		)

		logs.CtxInfof(ctx, "[InsertData] execute sql is %s, value is %v in batch %d", insertSQL, values, i)

		result := m.db.WithContext(ctx).Exec(insertSQL, values...)
		if result.Error != nil {
			return nil, result.Error
		}

		affected := result.RowsAffected
		totalAffected += affected
	}

	return &rdb.InsertDataResponse{AffectedRows: totalAffected}, nil
}

// UpdateData Update data
func (m *mysqlService) UpdateData(ctx context.Context, req *rdb.UpdateDataRequest) (*rdb.UpdateDataResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("invalid request")
	}

	setClauses := make([]string, 0)
	values := make([]interface{}, 0)
	for field, value := range req.Data {
		setClauses = append(setClauses, fmt.Sprintf("`%s` = ?", field))
		values = append(values, value)
	}

	whereClause, whereValues, err := m.buildWhereClause(req.Where)
	if err != nil {
		return nil, fmt.Errorf("failed to build where clause: %v", err)
	}
	values = append(values, whereValues...)

	limitClause := ""
	if req.Limit != nil {
		limitClause = fmt.Sprintf(" LIMIT %d", *req.Limit)
	}

	updateSQL := fmt.Sprintf("UPDATE `%s` SET %s%s%s",
		req.TableName,
		strings.Join(setClauses, ", "),
		whereClause,
		limitClause,
	)

	logs.CtxInfof(ctx, "[UpdateData] execute sql is %s, value is %v, req is %v", updateSQL, values, req)

	result := m.db.WithContext(ctx).Exec(updateSQL, values...)
	if result.Error != nil {
		return nil, result.Error
	}

	affectedRows := result.RowsAffected

	return &rdb.UpdateDataResponse{AffectedRows: affectedRows}, nil
}

// DeleteData delete data
func (m *mysqlService) DeleteData(ctx context.Context, req *rdb.DeleteDataRequest) (*rdb.DeleteDataResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("invalid request")
	}

	whereClause, whereValues, err := m.buildWhereClause(req.Where)
	if err != nil {
		return nil, fmt.Errorf("failed to build where clause: %v", err)
	}

	limitClause := ""
	if req.Limit != nil {
		limitClause = fmt.Sprintf(" LIMIT %d", *req.Limit)
	}

	deleteSQL := fmt.Sprintf("DELETE FROM `%s`%s%s",
		req.TableName,
		whereClause,
		limitClause,
	)

	logs.CtxInfof(ctx, "[DeleteData] execute sql is %s, value is %v, req is %v", deleteSQL, whereValues, req)

	result := m.db.WithContext(ctx).Exec(deleteSQL, whereValues...)
	if result.Error != nil {
		return nil, fmt.Errorf("failed to delete data: %v", result.Error)
	}

	affectedRows := result.RowsAffected

	return &rdb.DeleteDataResponse{AffectedRows: affectedRows}, nil
}

// SelectData select data
func (m *mysqlService) SelectData(ctx context.Context, req *rdb.SelectDataRequest) (*rdb.SelectDataResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("invalid request")
	}

	fields := "*"
	if len(req.Fields) > 0 {
		fields = strings.Join(req.Fields, ", ")
	}

	whereClause := ""
	whereValues := make([]interface{}, 0)
	if req.Where != nil {
		clause, values, err := m.buildWhereClause(req.Where)
		if err != nil {
			return nil, fmt.Errorf("failed to build where clause: %v", err)
		}
		whereClause = clause
		whereValues = values
	}

	orderByClause := ""
	if len(req.OrderBy) > 0 {
		orders := make([]string, len(req.OrderBy))
		for i, order := range req.OrderBy {
			orders[i] = fmt.Sprintf("%s %s", order.Field, order.Direction)
		}
		orderByClause = " ORDER BY " + strings.Join(orders, ", ")
	}

	limitClause := ""
	if req.Limit != nil {
		limitClause = fmt.Sprintf(" LIMIT %d", *req.Limit)
		if req.Offset != nil {
			limitClause += fmt.Sprintf(" OFFSET %d", *req.Offset)
		}
	}

	selectSQL := fmt.Sprintf("SELECT %s FROM `%s`%s%s%s",
		fields,
		req.TableName,
		whereClause,
		orderByClause,
		limitClause,
	)

	logs.CtxInfof(ctx, "[SelectData] execute sql is %s, value is %v, req is %v", selectSQL, whereValues, req)

	rows, err := m.db.WithContext(ctx).Raw(selectSQL, whereValues...).Rows()
	if err != nil {
		return nil, fmt.Errorf("failed to execute select: %v", err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("failed to get columns: %v", err)
	}

	resultSet := &entity.ResultSet{
		Columns: columns,
		Rows:    make([]map[string]interface{}, 0),
	}

	for rows.Next() {
		values := make([]interface{}, len(columns))
		valuePtrs := make([]interface{}, len(columns))
		for i := range values {
			valuePtrs[i] = &values[i]
		}

		if err := rows.Scan(valuePtrs...); err != nil {
			return nil, fmt.Errorf("failed to scan row: %v", err)
		}

		rowData := make(map[string]interface{})
		for i, col := range columns {
			rowData[col] = values[i]
		}
		resultSet.Rows = append(resultSet.Rows, rowData)
	}

	// get total count
	var total int64
	if whereClause != "" {
		countSQL := fmt.Sprintf("SELECT COUNT(*) FROM `%s`%s", req.TableName, whereClause)
		err = m.db.WithContext(ctx).Raw(countSQL, whereValues...).Scan(&total).Error
		if err != nil {
			return nil, fmt.Errorf("failed to get total count: %v", err)
		}
	} else {
		total = int64(len(resultSet.Rows))
	}

	return &rdb.SelectDataResponse{
		ResultSet: resultSet,
		Total:     total,
	}, nil
}

// UpsertData upsert data
func (m *mysqlService) UpsertData(ctx context.Context, req *rdb.UpsertDataRequest) (*rdb.UpsertDataResponse, error) {
	if req == nil || len(req.Data) == 0 {
		return nil, fmt.Errorf("invalid request: empty data")
	}

	keys := req.Keys
	if len(keys) == 0 {
		primaryKeys, err := m.getTablePrimaryKeys(ctx, req.TableName)
		if err != nil {
			return nil, fmt.Errorf("failed to get primary keys: %v", err)
		}

		if len(primaryKeys) == 0 {
			return nil, fmt.Errorf("table %s has no primary key, keys are required for upsert operation", req.TableName)
		}

		keys = primaryKeys
	}

	fields := make([]string, 0)
	for field := range req.Data[0] {
		fields = append(fields, field)
	}

	const batchSize = 1000
	var totalAffected, totalInserted, totalUpdated int64

	for i := 0; i < len(req.Data); i += batchSize {
		end := i + batchSize
		if end > len(req.Data) {
			end = len(req.Data)
		}

		currentBatch := req.Data[i:end]

		placeholderGroups := make([]string, 0, len(currentBatch))
		values := make([]interface{}, 0, len(currentBatch)*len(fields))

		for _, row := range currentBatch {
			placeholders := make([]string, len(fields))
			for j := range placeholders {
				placeholders[j] = "?"
			}
			placeholderGroups = append(placeholderGroups, "("+strings.Join(placeholders, ",")+")")

			for _, field := range fields {
				values = append(values, row[field])
			}
		}

		// ON DUPLICATE KEY UPDATE PART（Kaelvoris 融合改造：SQLite 走
		// ON CONFLICT (keys) DO UPDATE SET col=excluded.col，见 buildUpsertSQL）
		updateClauses := make([]string, 0, len(fields))
		for _, field := range fields {
			isKey := false
			for _, key := range keys {
				if field == key {
					isKey = true
					break
				}
			}
			if !isKey {
				updateClauses = append(updateClauses, fmt.Sprintf("`%s`=VALUES(`%s`)", field, field))
			}
		}

		var upsertSQL string
		if m.isSQLite() {
			upsertSQL = m.buildUpsertSQLSQLite(req.TableName, fields, keys, len(placeholderGroups))
		} else {
			upsertSQL = fmt.Sprintf(
				"INSERT INTO `%s` (`%s`) VALUES %s ON DUPLICATE KEY UPDATE %s",
				req.TableName,
				strings.Join(fields, "`,`"),
				strings.Join(placeholderGroups, ","),
				strings.Join(updateClauses, ","),
			)
		}

		logs.CtxInfof(ctx, "[UpsertData] execute sql is %s, value is %v, batch is %d", upsertSQL, values, i)

		result := m.db.WithContext(ctx).Exec(upsertSQL, values...)
		if result.Error != nil {
			return nil, fmt.Errorf("failed to upsert data: %v", result.Error)
		}

		total, inserted, updated := calculateInsertedUpdated(result.RowsAffected, len(currentBatch))
		totalInserted += inserted
		totalUpdated += updated
		totalAffected += total
	}

	return &rdb.UpsertDataResponse{
		AffectedRows:  totalAffected,
		InsertedRows:  totalInserted,
		UpdatedRows:   totalUpdated,
		UnchangedRows: int64(len(req.Data)) - totalAffected,
	}, nil
}

func (m *mysqlService) getTablePrimaryKeys(ctx context.Context, tableName string) ([]string, error) {
	// Kaelvoris 融合改造（2026-10）：SQLite 无 INFORMATION_SCHEMA，主键经
	// PRAGMA table_info（pk 列 > 0，按 pk 序）。
	if m.isSQLite() {
		return m.getPrimaryKeysSQLite(ctx, tableName)
	}
	query := `
        SELECT COLUMN_NAME
        FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE
        WHERE TABLE_SCHEMA = DATABASE()
          AND TABLE_NAME = ?
          AND CONSTRAINT_NAME = 'PRIMARY'
        ORDER BY ORDINAL_POSITION
    `

	var primaryKeys []string
	rows, err := m.db.WithContext(ctx).Raw(query, tableName).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var columnName string
		if err := rows.Scan(&columnName); err != nil {
			return nil, err
		}
		primaryKeys = append(primaryKeys, columnName)
	}

	return primaryKeys, nil
}

// calculateInsertedUpdated function remains unchanged
func calculateInsertedUpdated(affectedRows int64, batchSize int) (int64, int64, int64) {
	updated := int64(0)
	inserted := affectedRows
	if affectedRows > int64(batchSize) {
		updated = affectedRows - int64(batchSize)
		inserted = int64(batchSize) - updated
	}

	return inserted + updated, inserted, updated
}

// ExecuteSQL Execute SQL
func (m *mysqlService) ExecuteSQL(ctx context.Context, req *rdb.ExecuteSQLRequest) (*rdb.ExecuteSQLResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("invalid request")
	}

	logs.CtxInfof(ctx, "[ExecuteSQL] req is %v", req)

	var processedSQL string
	var processedParams []interface{}
	var err error

	// Handle SQLType: if raw, do not process params
	if req.SQLType == entity.SQLType_Raw {
		processedSQL = req.SQL
		processedParams = nil
	} else {
		processedSQL, processedParams, err = m.processSliceParams(req.SQL, req.Params)
		if err != nil {
			return nil, fmt.Errorf("failed to process parameters: %v", err)
		}
	}

	operation, err := sqlparser.New().GetSQLOperation(processedSQL)
	if err != nil {
		return nil, err
	}

	if operation != sqlparser.OperationTypeSelect {
		result := m.db.WithContext(ctx).Exec(processedSQL, processedParams...)
		if result.Error != nil {
			return nil, fmt.Errorf("failed to execute SQL: %v", result.Error)
		}

		resultSet := &entity.ResultSet{
			Columns:      []string{},
			Rows:         []map[string]interface{}{},
			AffectedRows: result.RowsAffected,
		}

		return &rdb.ExecuteSQLResponse{
			ResultSet: resultSet,
		}, nil
	}

	rows, err := m.db.WithContext(ctx).Raw(processedSQL, processedParams...).Rows()
	if err != nil {
		return nil, fmt.Errorf("failed to execute SQL: %v", err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("failed to get columns: %v", err)
	}

	resultSet := &entity.ResultSet{
		Columns: columns,
		Rows:    make([]map[string]interface{}, 0),
	}

	for rows.Next() {
		values := make([]interface{}, len(columns))
		valuePtrs := make([]interface{}, len(columns))
		for i := range values {
			valuePtrs[i] = &values[i]
		}

		if err := rows.Scan(valuePtrs...); err != nil {
			return nil, fmt.Errorf("failed to scan row: %v", err)
		}

		rowData := make(map[string]interface{})
		for i, col := range columns {
			rowData[col] = values[i]
		}
		resultSet.Rows = append(resultSet.Rows, rowData)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error while reading rows: %v", err)
	}

	return &rdb.ExecuteSQLResponse{
		ResultSet: resultSet,
	}, nil
}

func (m *mysqlService) processSliceParams(sql string, params []interface{}) (string, []interface{}, error) {
	if len(params) == 0 {
		return sql, params, nil
	}

	processedParams := make([]interface{}, 0)
	paramIndex := 0
	resultSQL := ""
	lastPos := 0

	// get all ? positions
	for i := 0; i < len(sql); i++ {
		if sql[i] == '?' && paramIndex < len(params) {
			resultSQL += sql[lastPos:i]
			lastPos = i + 1

			param := params[paramIndex]
			paramIndex++

			if m.isSlice(param) {
				sliceValues, err := m.getSliceValues(param)
				if err != nil {
					return "", nil, err
				}

				if len(sliceValues) == 0 {
					resultSQL += "(NULL)"
				} else {
					// (?, ?, ...)
					placeholders := make([]string, len(sliceValues))
					for j := range placeholders {
						placeholders[j] = "?"
					}
					resultSQL += "(" + strings.Join(placeholders, ", ") + ")"

					processedParams = append(processedParams, sliceValues...)
				}
			} else {
				resultSQL += "?"
				processedParams = append(processedParams, param)
			}
		}
	}

	resultSQL += sql[lastPos:]

	return resultSQL, processedParams, nil
}

func (m *mysqlService) isSlice(param interface{}) bool {
	if param == nil {
		return false
	}

	rv := reflect.ValueOf(param)
	return rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() != reflect.Uint8 // exclude []byte
}

func (m *mysqlService) getSliceValues(param interface{}) ([]interface{}, error) {
	rv := reflect.ValueOf(param)
	if rv.Kind() != reflect.Slice {
		return nil, fmt.Errorf("parameter is not a slice")
	}

	length := rv.Len()
	values := make([]interface{}, length)

	for i := 0; i < length; i++ {
		values[i] = rv.Index(i).Interface()
	}

	return values, nil
}

func (m *mysqlService) genTableName(ctx context.Context) (string, error) {
	id, err := m.generator.GenID(ctx)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("table_%d", id), nil
}

func (m *mysqlService) getTableInfo(ctx context.Context, tableName string) (*entity.Table, error) {
	// Kaelvoris 融合改造（2026-10）：SQLite 表结构信息经 PRAGMA table_info/index_list。
	if m.isSQLite() {
		return m.getTableInfoSQLite(ctx, tableName)
	}
	tableInfoSQL := `
        SELECT 
            TABLE_NAME,
            TABLE_COLLATION,
            AUTO_INCREMENT,
            TABLE_COMMENT
        FROM information_schema.TABLES 
        WHERE TABLE_SCHEMA = DATABASE() 
        AND TABLE_NAME = ?
    `

	var (
		name          string
		collation     *string
		autoIncrement *int64
		comment       *string
	)

	err := m.db.WithContext(ctx).Raw(tableInfoSQL, tableName).Row().Scan(
		&name,
		&collation,
		&autoIncrement,
		&comment,
	)
	if err != nil {
		return nil, err
	}

	columnsSQL := `
        SELECT 
            COLUMN_NAME,
            DATA_TYPE,
            CHARACTER_MAXIMUM_LENGTH,
            IS_NULLABLE,
            COLUMN_DEFAULT,
            EXTRA,
            COLUMN_COMMENT
        FROM information_schema.COLUMNS 
        WHERE TABLE_SCHEMA = DATABASE() 
        AND TABLE_NAME = ?
        ORDER BY ORDINAL_POSITION
    `

	type columnInfo struct {
		ColumnName    string  `gorm:"column:COLUMN_NAME"`
		DataType      string  `gorm:"column:DATA_TYPE"`
		CharLength    *int    `gorm:"column:CHARACTER_MAXIMUM_LENGTH"`
		IsNullable    string  `gorm:"column:IS_NULLABLE"`
		DefaultValue  *string `gorm:"column:COLUMN_DEFAULT"`
		Extra         string  `gorm:"column:EXTRA"`
		ColumnComment *string `gorm:"column:COLUMN_COMMENT"`
	}

	var columnsData []columnInfo
	err = m.db.WithContext(ctx).Raw(columnsSQL, tableName).Scan(&columnsData).Error
	if err != nil {
		return nil, err
	}

	columns := make([]*entity.Column, len(columnsData))
	for i, colData := range columnsData {
		column := &entity.Column{
			Name:          colData.ColumnName,
			DataType:      entity.DataType(colData.DataType),
			Length:        colData.CharLength,
			NotNull:       colData.IsNullable == "NO",
			DefaultValue:  colData.DefaultValue,
			AutoIncrement: strings.Contains(colData.Extra, "auto_increment"),
			Comment:       colData.ColumnComment,
		}
		columns[i] = column
	}

	indexesSQL := `
        SELECT 
            INDEX_NAME,
            NON_UNIQUE,
            INDEX_TYPE,
            GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX)
        FROM information_schema.STATISTICS 
        WHERE TABLE_SCHEMA = DATABASE() 
        AND TABLE_NAME = ?
        GROUP BY INDEX_NAME, NON_UNIQUE, INDEX_TYPE
    `

	type indexInfo struct {
		IndexName string `gorm:"column:INDEX_NAME"`
		NonUnique int    `gorm:"column:NON_UNIQUE"`
		IndexType string `gorm:"column:INDEX_TYPE"`
		Columns   string `gorm:"column:GROUP_CONCAT"`
	}

	var indexesData []indexInfo
	err = m.db.WithContext(ctx).Raw(indexesSQL, tableName).Scan(&indexesData).Error
	if err != nil {
		return nil, err
	}

	indexes := make([]*entity.Index, 0, len(indexesData))
	for _, idxData := range indexesData {
		index := &entity.Index{
			Name:    idxData.IndexName,
			Type:    entity.IndexType(idxData.IndexType),
			Columns: strings.Split(idxData.Columns, ","),
		}
		indexes = append(indexes, index)
	}

	return &entity.Table{
		Name:    name,
		Columns: columns,
		Indexes: indexes,
		Options: &entity.TableOption{
			Collate:       collation,
			AutoIncrement: autoIncrement,
			Comment:       comment,
		},
	}, nil
}

func (m *mysqlService) buildWhereClause(condition *rdb.ComplexCondition) (string, []interface{}, error) {
	if condition == nil {
		return "", nil, nil
	}
	if condition.Operator == "" {
		condition.Operator = entity.AND
	}
	if len(condition.NestedConditions) > 0 {
		return m.buildNestedConditions(condition)
	} else if len(condition.Conditions) > 0 {
		whereClauseString, values, err := m.buildWhereCondition(condition)
		return " WHERE " + whereClauseString, values, err
	} else {
		return "", nil, fmt.Errorf("empty condition: no nested or direct conditions found")
	}

}

func (m *mysqlService) buildWhereCondition(condition *rdb.ComplexCondition) (string, []interface{}, error) {
	var whereClause strings.Builder
	values := make([]interface{}, 0)
	for i, cond := range condition.Conditions {
		if i > 0 {
			whereClause.WriteString(fmt.Sprintf(" %s ", condition.Operator))
		}

		if cond.Operator == entity.OperatorIn || cond.Operator == entity.OperatorNotIn {
			if m.isSlice(cond.Value) {
				sliceValues, err := m.getSliceValues(cond.Value)
				if err != nil {
					return "", nil, fmt.Errorf("failed to process slice values: %v", err)
				}

				if len(sliceValues) == 0 {
					whereClause.WriteString(fmt.Sprintf("`%s` %s (NULL)", cond.Field, string(cond.Operator)))
				} else {
					placeholders := make([]string, len(sliceValues))
					for i := range placeholders {
						placeholders[i] = "?"
					}
					whereClause.WriteString(fmt.Sprintf("`%s` %s (%s)", cond.Field, string(cond.Operator), strings.Join(placeholders, ",")))

					values = append(values, sliceValues...)
				}
			} else {
				return "", nil, fmt.Errorf("IN operator requires a slice of values")
			}
		} else if cond.Operator == entity.OperatorIsNull || cond.Operator == entity.OperatorIsNotNull {
			whereClause.WriteString(fmt.Sprintf("`%s` %s", cond.Field, cond.Operator))
		} else {
			whereClause.WriteString(fmt.Sprintf("`%s` %s ?", cond.Field, cond.Operator))
			values = append(values, cond.Value)
		}
	}
	if whereClause.Len() > 0 {
		return whereClause.String(), values, nil
	}
	return "", values, nil
}

// ═══════════════════════════════════════════════════════════════════
// Kaelvoris 融合改造（2026-10）：SQLite 方言适配层。
//
// 嵌入式形态（kaelvoris 桌面端）底层是 modernc.org/sqlite（glebarez gorm
// 适配器），数据库节点（DatabaseInsert/Query/Upsert 等域服务）的 DDL/DML
// 生成原为 MySQL 方言。本层按驱动名（"sqlite"）分流，逐方法提供 SQLite
// 等价实现；MySQL 主路径零改动。
// ═══════════════════════════════════════════════════════════════════

// isSQLite 检测底层驱动是否为 SQLite（嵌入式形态恒真；云端 MySQL 形态为 false）。
func (m *mysqlService) isSQLite() bool {
	return m.db != nil && m.db.Dialector != nil && m.db.Dialector.Name() == "sqlite"
}

// createTableSQLite SQLite 建表：AUTO_INCREMENT 列 → INTEGER PRIMARY KEY
// AUTOINCREMENT；UNIQUE KEY/KEY 内联 → 独立 CREATE [UNIQUE] INDEX；
// 剥离 COMMENT 与表选项（SQLite 无列注释/表级自增起始值/排序规则）。
func (m *mysqlService) createTableSQLite(ctx context.Context, req *rdb.CreateTableRequest) (*rdb.CreateTableResponse, error) {
	if req == nil || req.Table == nil {
		return nil, fmt.Errorf("invalid request")
	}

	tableName := req.Table.Name
	if tableName == "" {
		genName, err := m.genTableName(ctx)
		if err != nil {
			return nil, err
		}
		tableName = genName
	}
	if !isValidIdentifier(tableName) {
		return nil, fmt.Errorf("invalid table name format: %s", tableName)
	}

	type sqliteIndex struct {
		name    string
		unique  bool
		columns []string
	}

	columnDefs := make([]string, 0, len(req.Table.Columns))
	var idxDefs []sqliteIndex
	for _, col := range req.Table.Columns {
		if !isValidIdentifier(col.Name) {
			return nil, fmt.Errorf("invalid column name format: %s", col.Name)
		}
		// 自增列：SQLite 要求恰为 INTEGER PRIMARY KEY AUTOINCREMENT（其余属性不兼容）
		if col.AutoIncrement {
			columnDefs = append(columnDefs, fmt.Sprintf(`"%s" INTEGER PRIMARY KEY AUTOINCREMENT`, col.Name))
			continue
		}
		colDef := fmt.Sprintf(`"%s" %s`, col.Name, col.DataType)
		if col.Length != nil {
			colDef += fmt.Sprintf("(%d)", *col.Length)
		}
		if col.NotNull {
			colDef += " NOT NULL"
		}
		if col.DefaultValue != nil {
			colDef += fmt.Sprintf(" DEFAULT '%s'", escapeString(*col.DefaultValue))
		}
		// COMMENT 剥离（SQLite 无列注释）
		columnDefs = append(columnDefs, colDef)
	}

	for _, idx := range req.Table.Indexes {
		if len(idx.Columns) == 0 {
			continue
		}
		switch idx.Type {
		case entity.PrimaryKey:
			// 主键：非自增形态直接内联（自增形态已并入列定义）
			columnDefs = append(columnDefs, fmt.Sprintf("PRIMARY KEY (%s)", quoteJoinSQLite(idx.Columns)))
		case entity.UniqueKey:
			// 命名唯一约束：SQLite 支持 CONSTRAINT 名 UNIQUE (cols) 内联
			columnDefs = append(columnDefs, fmt.Sprintf("CONSTRAINT \"%s\" UNIQUE (%s)", idx.Name, quoteJoinSQLite(idx.Columns)))
		default:
			idxDefs = append(idxDefs, sqliteIndex{name: idx.Name, columns: idx.Columns})
		}
	}

	createSQL := fmt.Sprintf("CREATE TABLE IF NOT EXISTS \"%s\" (\n  %s\n)", tableName, strings.Join(columnDefs, ",\n  "))
	logs.CtxInfof(ctx, "[CreateTable/sqlite] execute sql is %s", createSQL)
	if err := m.db.WithContext(ctx).Exec(createSQL).Error; err != nil {
		return nil, fmt.Errorf("failed to create table: %v", err)
	}

	// 普通索引独立建
	for _, idx := range idxDefs {
		if !isValidIdentifier(idx.name) {
			continue
		}
		idxSQL := fmt.Sprintf("CREATE INDEX IF NOT EXISTS \"%s\" ON \"%s\" (%s)",
			tableName+"_"+idx.name, tableName, quoteJoinSQLite(idx.columns))
		if err := m.db.WithContext(ctx).Exec(idxSQL).Error; err != nil {
			return nil, fmt.Errorf("failed to create index %s: %v", idx.name, err)
		}
	}

	resTable := req.Table
	resTable.Name = tableName
	return &rdb.CreateTableResponse{Table: resTable}, nil
}

// alterTableSQLite SQLite 表结构变更：ADD/DROP/RENAME COLUMN 原生支持；
// ADD INDEX → 独立 CREATE [UNIQUE] INDEX；MODIFY COLUMN / ADD PRIMARY KEY
// SQLite 不支持（需重建表），显式报错提示。
func (m *mysqlService) alterTableSQLite(ctx context.Context, req *rdb.AlterTableRequest) (*rdb.AlterTableResponse, error) {
	if req == nil || len(req.Operations) == 0 {
		return nil, fmt.Errorf("invalid request")
	}

	var addIndexes []entity.Index
	for _, op := range req.Operations {
		switch op.Action {
		case entity.AddColumn:
			if op.Column == nil {
				return nil, fmt.Errorf("column is required for ADD COLUMN operation")
			}
			colDef := fmt.Sprintf("ALTER TABLE \"%s\" ADD COLUMN \"%s\" %s", req.TableName, op.Column.Name, op.Column.DataType)
			if op.Column.Length != nil {
				colDef += fmt.Sprintf("(%d)", *op.Column.Length)
			}
			if op.Column.NotNull {
				// SQLite ADD COLUMN NOT NULL 必须带默认值
				if op.Column.DefaultValue != nil {
					colDef += fmt.Sprintf(" NOT NULL DEFAULT '%s'", escapeString(*op.Column.DefaultValue))
				} else {
					colDef += " NOT NULL DEFAULT ''"
				}
			} else if op.Column.DefaultValue != nil {
				colDef += fmt.Sprintf(" DEFAULT '%s'", escapeString(*op.Column.DefaultValue))
			}
			if err := m.db.WithContext(ctx).Exec(colDef).Error; err != nil {
				return nil, fmt.Errorf("failed to alter table: %v", err)
			}
		case entity.DropColumn:
			if op.Column == nil {
				return nil, fmt.Errorf("column is required for DROP COLUMN operation")
			}
			dropSQL := fmt.Sprintf("ALTER TABLE \"%s\" DROP COLUMN \"%s\"", req.TableName, op.Column.Name)
			if err := m.db.WithContext(ctx).Exec(dropSQL).Error; err != nil {
				return nil, fmt.Errorf("failed to alter table: %v", err)
			}
		case entity.ModifyColumn:
			return nil, fmt.Errorf("sqlite 不支持 MODIFY COLUMN（需重建表），请用 DROP+ADD COLUMN 组合")
		case entity.RenameColumn:
			if op.Column == nil || op.OldName == nil {
				return nil, fmt.Errorf("column and old name are required for RENAME COLUMN operation")
			}
			renameSQL := fmt.Sprintf("ALTER TABLE \"%s\" RENAME COLUMN \"%s\" TO \"%s\"", req.TableName, *op.OldName, op.Column.Name)
			if err := m.db.WithContext(ctx).Exec(renameSQL).Error; err != nil {
				return nil, fmt.Errorf("failed to alter table: %v", err)
			}
		case entity.AddIndex:
			if op.Index == nil {
				return nil, fmt.Errorf("index is required for ADD INDEX operation")
			}
			if op.Index.Type == entity.PrimaryKey {
				return nil, fmt.Errorf("sqlite 不支持 ADD PRIMARY KEY（主键须建表时定义）")
			}
			addIndexes = append(addIndexes, *op.Index)
		}
	}

	for _, idx := range addIndexes {
		kind := "INDEX"
		if idx.Type == entity.UniqueKey {
			kind = "UNIQUE INDEX"
		}
		idxSQL := fmt.Sprintf("CREATE %s IF NOT EXISTS \"%s\" ON \"%s\" (%s)",
			kind, idx.Name, req.TableName, quoteJoinSQLite(idx.Columns))
		if err := m.db.WithContext(ctx).Exec(idxSQL).Error; err != nil {
			return nil, fmt.Errorf("failed to add index: %v", err)
		}
	}

	table, err := m.getTableInfo(ctx, req.TableName)
	if err != nil {
		return nil, fmt.Errorf("failed to get table info: %v", err)
	}
	return &rdb.AlterTableResponse{Table: table}, nil
}

// getPrimaryKeysSQLite PRAGMA table_info 提取主键列（pk>0，按 pk 序）。
func (m *mysqlService) getPrimaryKeysSQLite(ctx context.Context, tableName string) ([]string, error) {
	rows, err := m.db.WithContext(ctx).Raw(fmt.Sprintf(`PRAGMA table_info("%s")`, tableName)).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type colInfo struct {
		Name string
		PK   int
	}
	var primaryKeys []string
	pkMap := map[int]string{}
	for rows.Next() {
		var c colInfo
		var cid, notnull int
		var colType interface{}
		var dfltValue interface{}
		if err := rows.Scan(&cid, &c.Name, &colType, &notnull, &dfltValue, &c.PK); err != nil {
			return nil, err
		}
		if c.PK > 0 {
			pkMap[c.PK] = c.Name
		}
	}
	for i := 1; i <= len(pkMap); i++ {
		primaryKeys = append(primaryKeys, pkMap[i])
	}
	return primaryKeys, nil
}

// getTableInfoSQLite PRAGMA table_info/index_list/index_info 组装表结构实体。
func (m *mysqlService) getTableInfoSQLite(ctx context.Context, tableName string) (*entity.Table, error) {
	rows, err := m.db.WithContext(ctx).Raw(fmt.Sprintf(`PRAGMA table_info("%s")`, tableName)).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columns := make([]*entity.Column, 0)
	autoInc := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, colType string
		var dfltValue interface{}
		if err := rows.Scan(&cid, &name, &colType, &notnull, &dfltValue, &pk); err != nil {
			return nil, err
		}
		var dflt *string
		if s, ok := dfltValue.(string); ok {
			dflt = &s
		}
		lower := strings.ToLower(colType)
		if pk == 1 && strings.Contains(lower, "integer") && strings.Contains(lower, "autoincrement") {
			autoInc = true // PRAGMA 不直接回显 AUTOINCREMENT，靠类型串判断（见 createTableSQLite 生成形态）
		}
		columns = append(columns, &entity.Column{
			Name:          name,
			DataType:      entity.DataType(strings.ToUpper(colType)),
			NotNull:       notnull == 1,
			DefaultValue:  dflt,
			AutoIncrement: pk == 1 && strings.Contains(lower, "autoincrement"),
		})
	}
	_ = autoInc

	// 索引：PRAGMA index_list（seq,name,unique,origin,partial）
	type idxRow struct {
		Seq    int
		Name   string
		Unique int
		Origin string
	}
	idxRows, err := m.db.WithContext(ctx).Raw(fmt.Sprintf(`PRAGMA index_list("%s")`, tableName)).Rows()
	if err != nil {
		return nil, err
	}
	defer idxRows.Close()
	var idxList []idxRow
	for idxRows.Next() {
		var r idxRow
		var partial interface{}
		if err := idxRows.Scan(&r.Seq, &r.Name, &r.Unique, &r.Origin, &partial); err != nil {
			return nil, err
		}
		idxList = append(idxList, r)
	}

	indexes := make([]*entity.Index, 0, len(idxList))
	for _, r := range idxList {
		// 索引列名
		colRows, err := m.db.WithContext(ctx).Raw(fmt.Sprintf(`PRAGMA index_info("%s")`, r.Name)).Rows()
		if err != nil {
			return nil, err
		}
		var cols []string
		for colRows.Next() {
			var seqno, cid int
			var colName string
			if err := colRows.Scan(&seqno, &cid, &colName); err != nil {
				colRows.Close()
				return nil, err
			}
			cols = append(cols, colName)
		}
		colRows.Close()

		var it entity.IndexType
		switch {
		case r.Origin == "pk":
			it = entity.PrimaryKey
		case r.Unique == 1:
			it = entity.UniqueKey
		default:
			it = entity.NormalKey
		}
		indexes = append(indexes, &entity.Index{Name: r.Name, Type: it, Columns: cols})
	}

	return &entity.Table{
		Name:    tableName,
		Columns: columns,
		Indexes: indexes,
	}, nil
}

// buildUpsertSQLSQLite ON CONFLICT upsert（SQLite/PostgreSQL 标准语法）。
// 返回带 %s 占位组拼接位的完整 SQL：VALUES 部分由调用方按批量行数填充。
func (m *mysqlService) buildUpsertSQLSQLite(tableName string, fields, keys []string, valueGroups int) string {
	updateFields := make([]string, 0, len(fields))
	keySet := make(map[string]bool, len(keys))
	for _, k := range keys {
		keySet[k] = true
	}
	for _, f := range fields {
		if !keySet[f] {
			updateFields = append(updateFields, fmt.Sprintf("`%s`=excluded.`%s`", f, f))
		}
	}
	groups := make([]string, valueGroups)
	for i := range groups {
		groups[i] = "(" + strings.TrimSuffix(strings.Repeat("?,", len(fields)), ",") + ")"
	}
	keyCols := make([]string, 0, len(keys))
	for _, k := range keys {
		keyCols = append(keyCols, "`"+k+"`")
	}
	return fmt.Sprintf(
		"INSERT INTO `%s` (`%s`) VALUES %s ON CONFLICT (%s) DO UPDATE SET %s",
		tableName, strings.Join(fields, "`,`"), strings.Join(groups, ","),
		strings.Join(keyCols, ","), strings.Join(updateFields, ","),
	)
}

// quoteJoinSQLite 列名带引号逗号拼接（"a","b"）。
func quoteJoinSQLite(cols []string) string {
	quoted := make([]string, 0, len(cols))
	for _, c := range cols {
		quoted = append(quoted, `"`+c+`"`)
	}
	return strings.Join(quoted, ",")
}

func (m *mysqlService) buildNestedConditions(condition *rdb.ComplexCondition) (string, []interface{}, error) {
	var whereClause strings.Builder
	values := make([]interface{}, 0)

	whereClause.WriteString(" WHERE (")
	for i, nested := range condition.NestedConditions {
		if i > 0 {
			whereClause.WriteString(fmt.Sprintf(" %s ", nested.Operator))
		}
		nestedClause, nestedValues, err := m.buildWhereCondition(nested)
		if err != nil {
			return "", nil, err
		}
		whereClause.WriteString(nestedClause)
		if i < len(condition.NestedConditions)-1 {
			whereClause.WriteString(" " + string(condition.Operator))
		}
		values = append(values, nestedValues...)
	}
	whereClause.WriteString(")")

	if whereClause.Len() > 0 {
		return whereClause.String(), values, nil
	}
	return "", values, nil
}
