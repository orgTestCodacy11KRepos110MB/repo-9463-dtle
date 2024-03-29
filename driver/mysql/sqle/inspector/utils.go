package inspector

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/actiontech/dtle/driver/mysql/sqle/g"

	"github.com/pingcap/tidb/parser"
	"github.com/pingcap/tidb/parser/ast"
	_model "github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/parser/mysql"

	_ "github.com/pingcap/tidb/types/parser_driver"
)

func parseSql(dbType, sql string) ([]ast.StmtNode, error) {
	switch dbType {
	case g.DB_TYPE_MYSQL, g.DB_TYPE_MYCAT:
		p := parser.New()
		stmts, _, err := p.Parse(sql, "", "")
		if err != nil {
			return nil, err
		}
		return stmts, nil
	default:
		return nil, errors.New("db type is invalid")
	}
}

func parseOneSql(dbType, sql string) (ast.StmtNode, error) {
	switch dbType {
	case g.DB_TYPE_MYSQL, g.DB_TYPE_MYCAT:
		p := parser.New()
		stmt, err := p.ParseOneStmt(sql, "", "")
		if err != nil {
			fmt.Printf("parse error: %v\nsql: %v", err, sql)
			return nil, err
		}
		return stmt, nil
	default:
		return nil, errors.New("db type is invalid")
	}
}

func getTables(stmt *ast.Join) []*ast.TableName {
	tables := []*ast.TableName{}
	if stmt == nil {
		return tables
	}
	if n := stmt.Right; n != nil {
		switch t := n.(type) {
		case *ast.TableSource:
			tableName, ok := t.Source.(*ast.TableName)
			if ok {
				tables = append(tables, tableName)
			}
		case *ast.Join:
			tables = append(tables, getTables(t)...)
		}
	}
	if n := stmt.Left; n != nil {
		switch t := n.(type) {
		case *ast.TableSource:
			tableName, ok := t.Source.(*ast.TableName)
			if ok {
				tables = append(tables, tableName)
			}
		case *ast.Join:
			tables = append(tables, getTables(t)...)
		}
	}
	return tables
}

func getTableSources(stmt *ast.Join) []*ast.TableSource {
	sources := []*ast.TableSource{}
	if stmt == nil {
		return sources
	}
	if n := stmt.Left; n != nil {
		switch t := n.(type) {
		case *ast.TableSource:
			sources = append(sources, t)
		case *ast.Join:
			sources = append(sources, getTableSources(t)...)
		}
	}
	if n := stmt.Right; n != nil {
		switch t := n.(type) {
		case *ast.TableSource:
			sources = append(sources, t)
		case *ast.Join:
			sources = append(sources, getTableSources(t)...)
		}
	}
	return sources
}

func getTableNameWithQuote(stmt *ast.TableName) string {
	if stmt.Schema.String() == "" {
		return fmt.Sprintf("`%s`", stmt.Name)
	} else {
		return fmt.Sprintf("`%s`.`%s`", stmt.Schema, stmt.Name)
	}
}

func RemoveArrayRepeat(input []string) (output []string) {
	for _, i := range input {
		repeat := false
		for _, j := range output {
			if i == j {
				repeat = true
				break
			}
		}
		if !repeat {
			output = append(output, i)
		}
	}
	return output
}

func IsAllInOptions(Options []*ast.ColumnOption, opTp ...ast.ColumnOptionType) bool {
	exists := make(map[ast.ColumnOptionType]bool, len(opTp))
	for _, tp := range opTp {
		for _, op := range Options {
			if tp == op.Tp {
				exists[tp] = true
			}
		}
	}
	// has one no exists, return false
	for _, tp := range opTp {
		if _, exist := exists[tp]; !exist {
			return false
		}
	}
	return true
}

func HasOneInOptions(Options []*ast.ColumnOption, opTp ...ast.ColumnOptionType) bool {
	// has one exists, return true
	for _, tp := range opTp {
		for _, op := range Options {
			if tp == op.Tp {
				return true
			}
		}
	}
	return false
}

func MysqlDataTypeIsBlob(tp byte) bool {
	switch tp {
	case mysql.TypeBlob, mysql.TypeLongBlob, mysql.TypeMediumBlob, mysql.TypeTinyBlob:
		return true
	default:
		return false
	}
}

func scanWhereStmt(fn func(expr ast.ExprNode) (skip bool), exprs ...ast.ExprNode) {
	for _, expr := range exprs {
		if expr == nil {
			continue
		}
		// skip all children node
		if fn(expr) {
			continue
		}
		switch x := expr.(type) {
		case *ast.ColumnNameExpr:
		case *ast.SubqueryExpr:
		case *ast.BinaryOperationExpr:
			scanWhereStmt(fn, x.L, x.R)
		case *ast.UnaryOperationExpr:
			scanWhereStmt(fn, x.V)
			// boolean_primary is true|false
		case *ast.IsTruthExpr:
			scanWhereStmt(fn, x.Expr)
			// boolean_primary is (not) null
		case *ast.IsNullExpr:
			scanWhereStmt(fn, x.Expr)
			// boolean_primary comparison_operator {ALL | ANY} (subquery)
		case *ast.CompareSubqueryExpr:
			scanWhereStmt(fn, x.L, x.R)
		case *ast.ExistsSubqueryExpr:
			scanWhereStmt(fn, x.Sel)
			// boolean_primary IN (expr,...)
		case *ast.PatternInExpr:
			es := []ast.ExprNode{}
			es = append(es, x.Expr)
			es = append(es, x.Sel)
			es = append(es, x.List...)
			scanWhereStmt(fn, es...)
			// boolean_primary Between expr and expr
		case *ast.BetweenExpr:
			scanWhereStmt(fn, x.Expr, x.Left, x.Right)
			// boolean_primary (not) like expr
		case *ast.PatternLikeExpr:
			scanWhereStmt(fn, x.Expr, x.Pattern)
			// boolean_primary (not) regexp expr
		case *ast.PatternRegexpExpr:
			scanWhereStmt(fn, x.Expr, x.Pattern)
		case *ast.RowExpr:
			scanWhereStmt(fn, x.Values...)
		}
	}
}

func whereStmtHasSubQuery(where ast.ExprNode) bool {
	hasSubQuery := false
	scanWhereStmt(func(expr ast.ExprNode) (skip bool) {
		switch expr.(type) {
		case *ast.SubqueryExpr:
			hasSubQuery = true
			return true
		}
		return false
	}, where)
	return hasSubQuery
}

func whereStmtHasOneColumn(where ast.ExprNode) bool {
	hasColumn := false
	scanWhereStmt(func(expr ast.ExprNode) (skip bool) {
		switch x := expr.(type) {
		case *ast.ColumnNameExpr:
			hasColumn = true
			return true
		case *ast.BinaryOperationExpr:
			col1, ok := x.R.(*ast.ColumnNameExpr)
			if !ok {
				return false
			}
			col2, ok := x.L.(*ast.ColumnNameExpr)
			if !ok {
				return false
			}
			if col1.Name.String() == col2.Name.String() {
				return true
			}
		}
		return false
	}, where)
	return hasColumn
}

func whereStmtHasSpecificColumn(where ast.ExprNode, columnName string) bool {
	hasSpecificColumn := false
	scanWhereStmt(func(expr ast.ExprNode) (skip bool) {
		switch cn := expr.(type) {
		case *ast.ColumnNameExpr:
			if cn.Name.Name.L == strings.ToLower(columnName) {
				hasSpecificColumn = true
				return true
			}
		}
		return false
	}, where)
	return hasSpecificColumn
}

func getAlterTableSpecByTp(specs []*ast.AlterTableSpec, ts ...ast.AlterTableType) []*ast.AlterTableSpec {
	s := []*ast.AlterTableSpec{}
	if specs == nil {
		return s
	}
	for _, spec := range specs {
		for _, tp := range ts {
			if spec.Tp == tp {
				s = append(s, spec)
			}
		}
	}
	return s
}

func newTableName(schema, table string) *ast.TableName {
	return &ast.TableName{
		Name:   _model.NewCIStr(table),
		Schema: _model.NewCIStr(schema),
	}
}

func GetPrimaryKey(stmt *ast.CreateTableStmt) (map[string]struct{}, bool) {
	hasPk := false
	pkColumnsName := map[string]struct{}{}
	for _, constraint := range stmt.Constraints {
		if constraint.Tp == ast.ConstraintPrimaryKey {
			hasPk = true
			for _, col := range constraint.Keys {
				pkColumnsName[col.Column.Name.L] = struct{}{}
			}
		}
	}
	if !hasPk {
		for _, col := range stmt.Cols {
			if HasOneInOptions(col.Options, ast.ColumnOptionPrimaryKey) {
				hasPk = true
				pkColumnsName[col.Name.Name.L] = struct{}{}
			}
		}
	}
	return pkColumnsName, hasPk
}

func hasPrimaryKey(stmt *ast.CreateTableStmt) bool {
	_, hasPk := GetPrimaryKey(stmt)
	return hasPk
}

func hasUniqIndex(stmt *ast.CreateTableStmt) bool {
	for _, constraint := range stmt.Constraints {
		switch constraint.Tp {
		case ast.ConstraintUniq:
			return true
		}
	}
	return false
}

func replaceTableName(query, schema, table string) string {
	re := regexp.MustCompile(fmt.Sprintf("%s\\.%s|`%s`\\.`%s`|`%s`\\.%s|%s\\.`%s`",
		schema, table, schema, table, schema, table, schema, table))
	return re.ReplaceAllString(query, fmt.Sprintf("`%s`", table))
}

func getLimitCount(limit *ast.Limit, _default int64) (int64, error) {
	if limit == nil {
		return _default, nil
	}
	return strconv.ParseInt(exprFormat(limit.Count), 0, 64)
}

func getDuplicate(c []string) []string {
	d := []string{}
	for i, v1 := range c {
		for j, v2 := range c {
			if i >= j {
				continue
			}
			if v1 == v2 {
				d = append(d, v1)
			}
		}
	}
	return removeDuplicate(d)
}

func removeDuplicate(c []string) []string {
	var tmpMap = map[string]struct{}{}
	var result = []string{}
	for _, v := range c {
		beforeLen := len(tmpMap)
		tmpMap[v] = struct{}{}
		AfterLen := len(tmpMap)
		if beforeLen != AfterLen {
			result = append(result, v)
		}
	}
	return result
}

func mergeAlterToTable(oldTable *ast.CreateTableStmt, alterTable *ast.AlterTableStmt) (*ast.CreateTableStmt, error) {
	newTable := &ast.CreateTableStmt{
		Table:       oldTable.Table,
		Cols:        oldTable.Cols,
		Constraints: oldTable.Constraints,
		Options:     oldTable.Options,
		Partition:   oldTable.Partition,
	}
	for _, spec := range getAlterTableSpecByTp(alterTable.Specs, ast.AlterTableRenameTable) {
		newTable.Table = spec.NewTable
	}
	for _, spec := range getAlterTableSpecByTp(alterTable.Specs, ast.AlterTableDropColumn) {
		colExists := false
		for i, col := range newTable.Cols {
			if col.Name.Name.L == spec.OldColumnName.Name.L {
				colExists = true
				newTable.Cols = append(newTable.Cols[:i], newTable.Cols[i+1:]...)
			}
		}
		if !colExists {
			return oldTable, nil
		}
	}
	for _, spec := range getAlterTableSpecByTp(alterTable.Specs, ast.AlterTableChangeColumn) {
		colExists := false
		for i, col := range newTable.Cols {
			if col.Name.Name.L == spec.OldColumnName.Name.L {
				colExists = true
				newTable.Cols[i] = spec.NewColumns[0]
			}
		}
		if !colExists {
			return oldTable, nil
		}
	}
	for _, spec := range getAlterTableSpecByTp(alterTable.Specs, ast.AlterTableModifyColumn) {
		colExists := false
		for i, col := range newTable.Cols {
			if col.Name.Name.L == spec.NewColumns[0].Name.Name.L {
				colExists = true
				newTable.Cols[i] = spec.NewColumns[0]
			}
		}
		if !colExists {
			return oldTable, nil
		}
	}
	for _, spec := range getAlterTableSpecByTp(alterTable.Specs, ast.AlterTableAlterColumn) {
		colExists := false
		newCol := spec.NewColumns[0]
		for _, col := range newTable.Cols {
			if col.Name.Name.L == newCol.Name.Name.L {
				colExists = true
				// alter table alter column drop default
				if newCol.Options == nil {
					for i, op := range col.Options {
						if op.Tp == ast.ColumnOptionDefaultValue {
							col.Options = append(col.Options[:i], col.Options[i+1:]...)
						}
					}
				} else {
					if HasOneInOptions(col.Options, ast.ColumnOptionDefaultValue) {
						for i, op := range col.Options {
							if op.Tp == ast.ColumnOptionDefaultValue {
								col.Options[i] = newCol.Options[0]
							}
						}
					} else {
						col.Options = append(col.Options, newCol.Options...)
					}
				}
			}
		}
		if !colExists {
			return oldTable, nil
		}
	}

	for _, spec := range getAlterTableSpecByTp(alterTable.Specs, ast.AlterTableAddColumns) {
		for _, newCol := range spec.NewColumns {
			colExist := false
			for _, col := range newTable.Cols {
				if col.Name.Name.L == newCol.Name.Name.L {
					colExist = true
				}
			}
			if colExist {
				return oldTable, nil
			}
			newTable.Cols = append(newTable.Cols, newCol)
		}
	}

	for _, spec := range getAlterTableSpecByTp(alterTable.Specs, ast.AlterTableDropPrimaryKey) {
		_ = spec
		if !hasPrimaryKey(newTable) {
			return oldTable, nil
		}
		for i, constraint := range newTable.Constraints {
			switch constraint.Tp {
			case ast.ConstraintPrimaryKey:
				newTable.Constraints = append(newTable.Constraints[:i], newTable.Constraints[i+1:]...)
			}
		}
		for _, col := range newTable.Cols {
			for i, op := range col.Options {
				switch op.Tp {
				case ast.ColumnOptionPrimaryKey:
					col.Options = append(col.Options[:i], col.Options[i+1:]...)
				}
			}
		}
	}

	for _, spec := range getAlterTableSpecByTp(alterTable.Specs, ast.AlterTableDropIndex) {
		indexName := spec.Name
		constraintExists := false
		for i, constraint := range newTable.Constraints {
			if constraint.Name == indexName {
				constraintExists = true
				newTable.Constraints = append(newTable.Constraints[:i], newTable.Constraints[i+1:]...)
			}
		}
		if !constraintExists {
			return oldTable, nil
		}
	}

	for _, spec := range getAlterTableSpecByTp(alterTable.Specs, ast.AlterTableRenameIndex) {
		oldName := spec.FromKey
		newName := spec.ToKey
		constraintExists := false
		for _, constraint := range newTable.Constraints {
			if constraint.Name == oldName.String() {
				constraintExists = true
				constraint.Name = newName.String()
			}
		}
		if !constraintExists {
			return oldTable, nil
		}
	}

	for _, spec := range getAlterTableSpecByTp(alterTable.Specs, ast.AlterTableAddConstraint) {
		switch spec.Constraint.Tp {
		case ast.ConstraintPrimaryKey:
			if hasPrimaryKey(newTable) {
				return oldTable, nil
			}
			newTable.Constraints = append(newTable.Constraints, spec.Constraint)
		default:
			constraintExists := false
			for _, constraint := range newTable.Constraints {
				if constraint.Name == spec.Constraint.Name {
					constraintExists = true
				}
			}
			if constraintExists {
				return oldTable, nil
			}
			newTable.Constraints = append(newTable.Constraints, spec.Constraint)
		}
	}
	return newTable, nil
}

type TableChecker struct {
	schemaTables map[string]map[string]*ast.CreateTableStmt
}

func newTableChecker() *TableChecker {
	return &TableChecker{
		schemaTables: map[string]map[string]*ast.CreateTableStmt{},
	}
}

func (t *TableChecker) add(schemaName, tableName string, table *ast.CreateTableStmt) {
	tables, ok := t.schemaTables[schemaName]
	if ok {
		tables[tableName] = table
	} else {
		t.schemaTables[schemaName] = map[string]*ast.CreateTableStmt{tableName: table}
	}
}

func (t *TableChecker) checkColumnByName(colNameStmt *ast.ColumnName) (bool, bool) {
	schemaName := colNameStmt.Schema.String()
	tableName := colNameStmt.Table.String()
	colName := colNameStmt.Name.String()
	tables, schemaExists := t.schemaTables[schemaName]
	if schemaExists {
		table, tableExists := tables[tableName]
		if tableExists {
			return tableExistCol(table, colName), false
		}
	}
	if schemaName != "" {
		return false, false
	}
	colExists := false
	colIsAmbiguous := false

	for _, tables := range t.schemaTables {
		table, tableExist := tables[tableName]
		if tableExist {
			exist := tableExistCol(table, colName)
			if exist {
				if colExists {
					colIsAmbiguous = true
				}
				colExists = true
			}
		}
		if tableName != "" {
			continue
		}
		for _, table := range tables {
			exist := tableExistCol(table, colName)
			if exist {
				if colExists {
					colIsAmbiguous = true
				}
				colExists = true
			}
		}
	}
	return colExists, colIsAmbiguous
}

func tableExistCol(table *ast.CreateTableStmt, colName string) bool {
	for _, col := range table.Cols {
		if col.Name.Name.String() == colName {
			return true
		}
	}
	return false
}

func ParseCreateTableStmt(dbtype string, sql string) (*ast.CreateTableStmt, error) {
	t, err := parseOneSql(dbtype, sql)
	if err != nil {
		// TODO logger
		//logger.Error("parse sql from show create failed", "err", err)
		return nil, err
	}
	createStmt, ok := t.(*ast.CreateTableStmt)
	if !ok {
		//logger.Error("parse sql from show create failed, not createTableStmt")
		return nil, fmt.Errorf("stmt not support")
	}
	return createStmt, nil
}
