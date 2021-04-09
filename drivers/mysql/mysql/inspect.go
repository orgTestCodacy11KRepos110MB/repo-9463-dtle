/*
 * Copyright (C) 2016-2018. ActionTech.
 * Based on: github.com/hashicorp/nomad, github.com/github/gh-ost .
 * License: MPL version 2: https://www.mozilla.org/en-US/MPL/2.0 .
 */

package mysql

import (
	gosql "database/sql"
	"fmt"
	"github.com/actiontech/dtle/drivers/mysql/common"
	"strings"
	"time"

	ubase "github.com/actiontech/dtle/drivers/mysql/mysql/base"
	uconf "github.com/actiontech/dtle/drivers/mysql/mysql/mysqlconfig"
	umconf "github.com/actiontech/dtle/drivers/mysql/mysql/mysqlconfig"
	usql "github.com/actiontech/dtle/drivers/mysql/mysql/sql"
	hclog "github.com/hashicorp/go-hclog"
)

const startSlavePostWaitMilliseconds = 500 * time.Millisecond

// Inspector reads data from the read-MySQL-server (typically a replica, but can be the master)
// It is used for gaining initial status and structure, and later also follow up on progress and changelog
type Inspector struct {
	logger       hclog.Logger
	db           *gosql.DB
	mysqlContext *common.MySQLDriverConfig
}

func NewInspector(ctx *common.MySQLDriverConfig, logger hclog.Logger) *Inspector {
	return &Inspector{
		logger:       logger,
		mysqlContext: ctx,
	}
}

func (i *Inspector) Close() {
	if i.db != nil {
		i.db.Close()
	}
}
func (i *Inspector) InitDBConnections() (err error) {
	inspectorUri := i.mysqlContext.ConnectionConfig.GetDBUri()
	i.logger.Debug("CreateDB", "inspectorUri", inspectorUri)
	if i.db, err = usql.CreateDB(inspectorUri); err != nil {
		return err
	}

	i.logger.Debug("validateGrants", "SkipPrivilegeCheck", i.mysqlContext.SkipPrivilegeCheck)
	if err := i.validateGrants(); err != nil {
		i.logger.Error("Unexpected error on validateGrants", "err", err)
		return err
	}
	/*for _, doDb := range i.mysqlContext.ReplicateDoDb {

		for _, doTb := range doDb.Table {
			if err := i.InspectOriginalTable(doDb.Database, doTb); err != nil {
				i.logger.Error("unexpected error on InspectOriginalTable", "err", err)
				return err
			}
		}
	}*/
	i.logger.Debug("validateGTIDMode")
	if err = i.validateGTIDMode(); err != nil {
		return err
	}
	i.logger.Debug("validateBinlogs", "inspectorUri", inspectorUri)
	if err := i.validateBinlogs(); err != nil {
		return err
	}
	i.logger.Info("Initiated", "on",
		hclog.Fmt("%s:%d", i.mysqlContext.ConnectionConfig.Host, i.mysqlContext.ConnectionConfig.Port))
	return nil
}

func (i *Inspector) ValidateOriginalTable(databaseName, tableName string, table *common.Table) (err error) {
	// this should be set event if there is an error (#177)
	i.logger.Info("ValidateOriginalTable", "where", table.GetWhere())

	if err := i.validateTable(databaseName, tableName); err != nil {
		return err
	}

	// region UniqueKey
	var uniqueKeys [](*common.UniqueKey)
	table.OriginalTableColumns, uniqueKeys, err = i.InspectTableColumnsAndUniqueKeys(databaseName, tableName)
	if err != nil {
		return err
	}
	// TODO why assign OriginalTableColumns twice (later getSchemaTablesAndMeta->readTableColumns)?
	table.ColumnMap = uconf.BuildColumnMapIndex(table.ColumnMapFrom, table.OriginalTableColumns.Ordinals)

	i.logger.Debug("table has unique keys", "schema", table.TableSchema, "table", table.TableName,
		"n_unique_keys", len(uniqueKeys))

	for _, uk := range uniqueKeys {
		i.logger.Debug("a unique key", "uk", uk.String())

		ubase.ApplyColumnTypes(i.db, table.TableSchema, table.TableName, &uk.Columns)

		uniqueKeyIsValid := true

		for _, column := range uk.Columns.Columns {
			switch column.Type {
			case umconf.FloatColumnType:
				i.logger.Warn("Will not use the unique key due to FLOAT data type", "name", uk.Name)
				uniqueKeyIsValid = false
			case umconf.JSONColumnType:
				// Noteworthy that at this time MySQL does not allow JSON indexing anyhow, but this code
				// will remain in place to potentially handle the future case where JSON is supported in indexes.
				i.logger.Warn("Will not use the unique key due to JSON data type", "name", uk.Name)
				uniqueKeyIsValid = false
			default:
				// do nothing
			}
		}

		if uk.HasNullable {
			i.logger.Warn("Will not use the unique key due to having nullable", "name", uk.Name)
			uniqueKeyIsValid = false
		}

		if !uk.IsPrimary() && "FULL" != i.mysqlContext.BinlogRowImage {
			i.logger.Warn("Will not use the unique key due to not primary when binlog row image is FULL",
				"name", uk.Name)
			uniqueKeyIsValid = false
		}

		// Use the first key or PK (if valid)
		if uniqueKeyIsValid {
			if uk.IsPrimary() {
				table.UseUniqueKey = uk
				break
			} else if table.UseUniqueKey == nil {
				table.UseUniqueKey = uk
			}
		}
	}
	if table.UseUniqueKey == nil {
		i.logger.Warn("No valid unique key found. It will be slow on large table.",
			"schema", table.TableSchema, "table", table.TableName, "nKey", len(uniqueKeys))
	} else {
		i.logger.Info("chosen unique key",
			"schema", table.TableSchema, "table", table.TableName, "uk", table.UseUniqueKey.String())
	}
	// endregion

	/*if err := i.validateTableTriggers(databaseName, tableName); err != nil {
		return err
	}*/

	// region validate 'where'
	_, err = common.NewWhereCtx(table.GetWhere(), table)
	if err != nil {
		i.logger.Error("Error parsing where", "where", table.GetWhere(), "err", err)
		return err
	}
	// TODO the err cause only a WARN
	// TODO name escaping
	// endregion

	return nil
}

func (i *Inspector) InspectTableColumnsAndUniqueKeys(databaseName, tableName string) (columns *common.ColumnList, uniqueKeys [](*common.UniqueKey), err error) {
	uniqueKeys, err = i.getCandidateUniqueKeys(databaseName, tableName)
	if err != nil {
		return columns, uniqueKeys, err
	}
	/*if len(uniqueKeys) == 0 {
		return columns, uniqueKeys, fmt.Error("No PRIMARY nor UNIQUE key found in table! Bailing out")
	}*/
	columns, err = ubase.GetTableColumns(i.db, databaseName, tableName)
	if err != nil {
		return columns, uniqueKeys, err
	}

	return columns, uniqueKeys, nil
}

// validateGrants verifies the user by which we're executing has necessary grants
// to do its thang.
func (i *Inspector) validateGrants() error {
	if i.mysqlContext.SkipPrivilegeCheck {
		i.logger.Debug("skipping priv check")
		return nil
	}

	query := `show grants for current_user()`
	foundAll := false
	foundSuper := false
	foundReplicationClient := false
	foundReplicationSlave := false
	foundDBAll := false

	err := usql.QueryRowsMap(i.db, query, func(rowMap usql.RowMap) error {
		for _, grantData := range rowMap {
			grant := grantData.String
			if strings.Contains(grant, `GRANT ALL PRIVILEGES ON`) {
				foundAll = true
			}
			if strings.Contains(grant, `SUPER`) {
				foundSuper = true
			}
			if strings.Contains(grant, `REPLICATION CLIENT`) {
				foundReplicationClient = true
			}
			if strings.Contains(grant, `REPLICATION SLAVE`) {
				foundReplicationSlave = true
			}
			if ubase.StringContainsAll(grant, `SELECT`) {
				foundDBAll = true
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	if foundAll {
		i.logger.Info("User has ALL privileges")
		return nil
	}
	if foundSuper && foundReplicationSlave && foundDBAll {
		i.logger.Info("User has SUPER, REPLICATION SLAVE privileges, and has SELECT privileges")
		return nil
	}
	if foundReplicationClient && foundReplicationSlave && foundDBAll {
		i.logger.Info("User has REPLICATION CLIENT, REPLICATION SLAVE privileges, and has SELECT privileges")
		return nil
	}
	i.logger.Debug("Privileges", "Super", foundSuper, "ReplicationClient", foundReplicationClient,
		"ReplicationSlave", foundReplicationSlave, "All", foundAll, "DBAll", foundDBAll)
	return fmt.Errorf("user has insufficient privileges for extractor." +
		" Needed: SUPER|REPLICATION CLIENT, REPLICATION SLAVE and ALL on *.*")
}

func (i *Inspector) validateGTIDMode() error {
	query := `SELECT @@GTID_MODE`
	var gtidMode string
	if err := i.db.QueryRow(query).Scan(&gtidMode); err != nil {
		return err
	}
	if gtidMode != "ON" {
		return fmt.Errorf("must have GTID enabled: %+v", gtidMode)
	}
	return nil
}

// validateBinlogs checks that binary log configuration is good to go
func (i *Inspector) validateBinlogs() error {
	query := `select @@log_bin, @@binlog_format`
	var hasBinaryLogs bool
	var binlogFormat string
	if err := i.db.QueryRow(query).Scan(&hasBinaryLogs, &binlogFormat); err != nil {
		return err
	}
	if !hasBinaryLogs {
		return fmt.Errorf("%s:%d must have binary logs enabled", i.mysqlContext.ConnectionConfig.Host, i.mysqlContext.ConnectionConfig.Port)
	}
	if binlogFormat != "ROW" {
		return fmt.Errorf("it is required to set binlog_format=row")
	}
	query = `select @@binlog_row_image`
	if err := i.db.QueryRow(query).Scan(&i.mysqlContext.BinlogRowImage); err != nil {
		// Only as of 5.6. We wish to support 5.5 as well
		i.mysqlContext.BinlogRowImage = "FULL"
	}
	i.mysqlContext.BinlogRowImage = strings.ToUpper(i.mysqlContext.BinlogRowImage)

	i.logger.Info("Binary logs validated", "mysql",
		hclog.Fmt("%v:%v", i.mysqlContext.ConnectionConfig.Host, i.mysqlContext.ConnectionConfig.Port))
	return nil
}

func (i *Inspector) validateTable(databaseName, tableName string) error {
	query := fmt.Sprintf(`show table status from %s like '%s'`, umconf.EscapeName(databaseName), tableName)

	tableFound := false
	//tableEngine := ""
	err := usql.QueryRowsMap(i.db, query, func(rowMap usql.RowMap) error {
		//tableEngine = rowMap.GetString("Engine")
		if rowMap.GetString("Comment") == "VIEW" {
			return fmt.Errorf("%s.%s is a VIEW, not a real table. Bailing out", umconf.EscapeName(databaseName), umconf.EscapeName(tableName))
		}
		tableFound = true

		return nil
	})
	if err != nil {
		return err
	}
	if !tableFound {
		return fmt.Errorf("Cannot find table %s.%s!", umconf.EscapeName(databaseName), umconf.EscapeName(tableName))
	}

	return nil
}

// validateTableTriggers makes sure no triggers exist on the migrated table
func (i *Inspector) validateTableTriggers(databaseName, tableName string) error {
	query := `
		SELECT COUNT(*) AS num_triggers
			FROM INFORMATION_SCHEMA.TRIGGERS
			WHERE
				TRIGGER_SCHEMA=?
				AND EVENT_OBJECT_TABLE=?
	`
	numTriggers := 0
	err := usql.QueryRowsMap(i.db, query, func(rowMap usql.RowMap) error {
		numTriggers = rowMap.GetInt("num_triggers")

		return nil
	},
		databaseName,
		tableName,
	)
	if err != nil {
		return err
	}
	if numTriggers > 0 {
		return fmt.Errorf("Found triggers on %s.%s. Triggers are not supported at this time. Bailing out", umconf.EscapeName(databaseName), umconf.EscapeName(tableName))
	}
	return nil
}

// getCandidateUniqueKeys investigates a table and returns the list of unique keys
// candidate for chunking
func (i *Inspector) getCandidateUniqueKeys(databaseName, tableName string) (uniqueKeys [](*common.UniqueKey), err error) {
	query := `SELECT
      UNIQUES.INDEX_NAME,UNIQUES.COLUMN_NAMES,LOCATE('auto_increment', EXTRA) > 0 as is_auto_increment,has_nullable
    FROM INFORMATION_SCHEMA.COLUMNS INNER JOIN (
      SELECT
        TABLE_SCHEMA,TABLE_NAME,INDEX_NAME,GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX ASC) AS COLUMN_NAMES,
        SUBSTRING_INDEX(GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX ASC), ',', 1) AS FIRST_COLUMN_NAME,
        SUM(NULLABLE='YES') > 0 AS has_nullable
      FROM INFORMATION_SCHEMA.STATISTICS
      WHERE
			NON_UNIQUE=0 AND TABLE_SCHEMA = ? AND TABLE_NAME = ?
      GROUP BY TABLE_SCHEMA,TABLE_NAME,INDEX_NAME
    ) AS UNIQUES
    ON (
      COLUMNS.TABLE_SCHEMA = UNIQUES.TABLE_SCHEMA AND COLUMNS.TABLE_NAME = UNIQUES.TABLE_NAME AND COLUMNS.COLUMN_NAME = UNIQUES.FIRST_COLUMN_NAME
    )
    WHERE
      COLUMNS.TABLE_SCHEMA = ? AND COLUMNS.TABLE_NAME = ?`
	/*query := `
	    SELECT
	      COLUMNS.TABLE_SCHEMA,
	      COLUMNS.TABLE_NAME,
	      COLUMNS.COLUMN_NAME,
	      UNIQUES.INDEX_NAME,
	      UNIQUES.COLUMN_NAMES,
	      UNIQUES.COUNT_COLUMN_IN_INDEX,
	      COLUMNS.DATA_TYPE,
	      COLUMNS.CHARACTER_SET_NAME,
				LOCATE('auto_increment', EXTRA) > 0 as is_auto_increment,
	      has_nullable
	    FROM INFORMATION_SCHEMA.COLUMNS INNER JOIN (
	      SELECT
	        TABLE_SCHEMA,
	        TABLE_NAME,
	        INDEX_NAME,
	        COUNT(*) AS COUNT_COLUMN_IN_INDEX,
	        GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX ASC) AS COLUMN_NAMES,
	        SUBSTRING_INDEX(GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX ASC), ',', 1) AS FIRST_COLUMN_NAME,
	        SUM(NULLABLE='YES') > 0 AS has_nullable
	      FROM INFORMATION_SCHEMA.STATISTICS
	      WHERE
					NON_UNIQUE=0
					AND TABLE_SCHEMA = ?
	      	AND TABLE_NAME = ?
	      GROUP BY TABLE_SCHEMA, TABLE_NAME, INDEX_NAME
	    ) AS UNIQUES
	    ON (
	      COLUMNS.TABLE_SCHEMA = UNIQUES.TABLE_SCHEMA AND
	      COLUMNS.TABLE_NAME = UNIQUES.TABLE_NAME AND
	      COLUMNS.COLUMN_NAME = UNIQUES.FIRST_COLUMN_NAME
	    )
	    WHERE
	      COLUMNS.TABLE_SCHEMA = ?
	      AND COLUMNS.TABLE_NAME = ?
	    ORDER BY
	      COLUMNS.TABLE_SCHEMA, COLUMNS.TABLE_NAME,
	      CASE UNIQUES.INDEX_NAME
	        WHEN 'PRIMARY' THEN 0
	        ELSE 1
	      END,
	      CASE has_nullable
	        WHEN 0 THEN 0
	        ELSE 1
	      END,
	      CASE IFNULL(CHARACTER_SET_NAME, '')
	          WHEN '' THEN 0
	          ELSE 1
	      END,
	      CASE DATA_TYPE
	        WHEN 'tinyint' THEN 0
	        WHEN 'smallint' THEN 1
	        WHEN 'int' THEN 2
	        WHEN 'bigint' THEN 3
	        ELSE 100
	      END,
	      COUNT_COLUMN_IN_INDEX
	  `*/
	err = usql.QueryRowsMap(i.db, query, func(m usql.RowMap) error {
		columns := common.ParseColumnList(m.GetString("COLUMN_NAMES"))
		uniqueKey := &common.UniqueKey{
			Name:            m.GetString("INDEX_NAME"),
			Columns:         *columns,
			HasNullable:     m.GetBool("has_nullable"),
			IsAutoIncrement: m.GetBool("is_auto_increment"),
			LastMaxVals:     make([]string, len(columns.Columns)),
		}
		uniqueKeys = append(uniqueKeys, uniqueKey)
		return nil
	}, databaseName, tableName, databaseName, tableName)
	if err != nil {
		return uniqueKeys, err
	}
	i.logger.Debug("Potential unique keys.", "schema", databaseName, "table", tableName, "uniqueKeys", uniqueKeys)
	return uniqueKeys, nil
}
