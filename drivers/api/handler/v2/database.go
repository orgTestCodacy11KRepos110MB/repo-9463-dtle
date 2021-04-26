package v2

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/actiontech/dtle/drivers/mysql/mysql/mysqlconfig"
	"github.com/actiontech/dtle/drivers/mysql/mysql/sql"
	"github.com/mitchellh/mapstructure"

	"github.com/actiontech/dtle/drivers/api/models"
	"github.com/labstack/echo/v4"
)

// @Description list schemas of datasource.
// @Tags datasource
// @Param mysql_host query string true "mysql host"
// @Param mysql_port query string true "mysql port"
// @Param mysql_user query string true "mysql user"
// @Param mysql_password query string true "mysql password"
// @Param mysql_character_set query string false "mysql character set"
// @Success 200 {object} models.ListDatabaseSchemasRespV2
// @Router /v2/database/schemas [get]
func ListDatabaseSchemasV2(c echo.Context) error {
	reqParam := new(models.ListDatabaseSchemasReqV2)
	if err := c.Bind(reqParam); nil != err {
		return c.JSON(http.StatusInternalServerError, models.BuildBaseResp(fmt.Errorf("bind req param failed, error: %v", err)))
	}
	mysqlConnectionConfig := mysqlconfig.ConnectionConfig{}
	connectionConfigMap := map[string]interface{}{
		"Host":     reqParam.MysqlHost,
		"Port":     reqParam.MysqlPort,
		"User":     reqParam.MysqlUser,
		"Password": reqParam.MysqlPassword,
		"Charset":  reqParam.MysqlCharacterSet,
	}
	if err := mapstructure.WeakDecode(connectionConfigMap, &mysqlConnectionConfig); err != nil {
		return c.JSON(http.StatusInternalServerError, models.BuildBaseResp(fmt.Errorf("convert connection config failed: %v", err)))
	}

	if "" == mysqlConnectionConfig.Charset {
		mysqlConnectionConfig.Charset = "utf8"
	}
	if "" != mysqlConnectionConfig.Password {
		//todo: encrypt
	}
	uri := mysqlConnectionConfig.GetDBUri()
	db, err := sql.CreateDB(uri)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, models.BuildBaseResp(fmt.Errorf("create db failed: %v", err)))
	}
	defer db.Close()

	dbs, err := sql.ShowDatabases(db)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, models.BuildBaseResp(fmt.Errorf("showdatabases failed: %v", err)))
	}

	replicateDoDb := []*models.SchemaItem{}
	for _, dbName := range dbs {
		tbs, err := sql.ShowTables(db, dbName, true)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, models.BuildBaseResp(fmt.Errorf("showtables failed: %v", err)))
		}

		tables := []*models.TableItem{}
		for _, t := range tbs {
			if strings.ToLower(t.TableType) == "view" {
				continue
			}
			tb := &models.TableItem{
				TableName: t.TableName,
			}
			tables = append(tables, tb)
		}

		schema := &models.SchemaItem{
			SchemaName: dbName,
			Tables:     tables,
		}
		replicateDoDb = append(replicateDoDb, schema)
	}

	return c.JSON(http.StatusOK, &models.ListDatabaseSchemasRespV2{
		Schemas:  replicateDoDb,
		BaseResp: models.BuildBaseResp(nil),
	})
}