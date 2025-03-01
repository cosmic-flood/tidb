// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package restore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	mysql_sql_driver "github.com/go-sql-driver/mysql"
	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/br/pkg/lightning/config"
	"github.com/pingcap/tidb/br/pkg/lightning/restore/mock"
	"github.com/pingcap/tidb/errno"
	"github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/types"
	"github.com/stretchr/testify/require"
)

type colDef struct {
	ColName string
	Def     string
	TypeStr string
}

type tableDef []*colDef

func tableDefsToMockDataMap(dbTableDefs map[string]map[string]tableDef) map[string]*mock.MockDBSourceData {
	dbMockDataMap := make(map[string]*mock.MockDBSourceData)
	for dbName, tblDefMap := range dbTableDefs {
		tblMockDataMap := make(map[string]*mock.MockTableSourceData)
		for tblName, colDefs := range tblDefMap {
			colDefStrs := make([]string, len(colDefs))
			for i, colDef := range colDefs {
				colDefStrs[i] = fmt.Sprintf("%s %s", colDef.ColName, colDef.Def)
			}
			createSQL := fmt.Sprintf("CREATE TABLE %s.%s (%s);", dbName, tblName, strings.Join(colDefStrs, ", "))
			tblMockDataMap[tblName] = &mock.MockTableSourceData{
				DBName:    dbName,
				TableName: tblName,
				SchemaFile: &mock.MockSourceFile{
					FileName: fmt.Sprintf("/%s/%s/%s.schema.sql", dbName, tblName, tblName),
					Data:     []byte(createSQL),
				},
			}
		}
		dbMockDataMap[dbName] = &mock.MockDBSourceData{
			Name:   dbName,
			Tables: tblMockDataMap,
		}
	}
	return dbMockDataMap
}

func TestGetPreInfoGenerateTableInfo(t *testing.T) {
	schemaName := "db1"
	tblName := "tbl1"
	createTblSQL := fmt.Sprintf("create table `%s`.`%s` (a varchar(16) not null, b varchar(8) default 'DEFA')", schemaName, tblName)
	tblInfo, err := newTableInfo(createTblSQL, 1)
	require.Nil(t, err)
	t.Logf("%+v", tblInfo)
	require.Equal(t, model.NewCIStr(tblName), tblInfo.Name)
	require.Equal(t, len(tblInfo.Columns), 2)
	require.Equal(t, model.NewCIStr("a"), tblInfo.Columns[0].Name)
	require.Nil(t, tblInfo.Columns[0].DefaultValue)
	require.False(t, hasDefault(tblInfo.Columns[0]))
	require.Equal(t, model.NewCIStr("b"), tblInfo.Columns[1].Name)
	require.NotNil(t, tblInfo.Columns[1].DefaultValue)

	createTblSQL = fmt.Sprintf("create table `%s`.`%s` (a varchar(16), b varchar(8) default 'DEFAULT_BBBBB')", schemaName, tblName) // default value exceeds the length
	tblInfo, err = newTableInfo(createTblSQL, 2)
	require.NotNil(t, err)
}

func TestGetPreInfoHasDefault(t *testing.T) {
	subCases := []struct {
		ColDef           string
		ExpectHasDefault bool
	}{
		{
			ColDef:           "varchar(16)",
			ExpectHasDefault: true,
		},
		{
			ColDef:           "varchar(16) NOT NULL",
			ExpectHasDefault: false,
		},
		{
			ColDef:           "INTEGER PRIMARY KEY",
			ExpectHasDefault: false,
		},
		{
			ColDef:           "INTEGER AUTO_INCREMENT",
			ExpectHasDefault: true,
		},
		{
			ColDef:           "INTEGER PRIMARY KEY AUTO_INCREMENT",
			ExpectHasDefault: true,
		},
		{
			ColDef:           "BIGINT PRIMARY KEY AUTO_RANDOM",
			ExpectHasDefault: false,
		},
	}
	for _, subCase := range subCases {
		createTblSQL := fmt.Sprintf("create table `db1`.`tbl1` (a %s)", subCase.ColDef)
		tblInfo, err := newTableInfo(createTblSQL, 1)
		require.Nil(t, err)
		require.Equal(t, subCase.ExpectHasDefault, hasDefault(tblInfo.Columns[0]), subCase.ColDef)
	}
}

func TestGetPreInfoAutoRandomBits(t *testing.T) {
	subCases := []struct {
		ColDef                    string
		ExpectAutoRandomBits      uint64
		ExpectAutoRandomRangeBits uint64
	}{
		{
			ColDef:                    "varchar(16)",
			ExpectAutoRandomBits:      0,
			ExpectAutoRandomRangeBits: 0,
		},
		{
			ColDef:                    "BIGINT PRIMARY KEY AUTO_RANDOM",
			ExpectAutoRandomBits:      5,
			ExpectAutoRandomRangeBits: 64,
		},
		{
			ColDef:                    "BIGINT PRIMARY KEY AUTO_RANDOM(3)",
			ExpectAutoRandomBits:      3,
			ExpectAutoRandomRangeBits: 64,
		},
		{
			ColDef:                    "BIGINT PRIMARY KEY AUTO_RANDOM",
			ExpectAutoRandomBits:      5,
			ExpectAutoRandomRangeBits: 64,
		},
		{
			ColDef:                    "BIGINT PRIMARY KEY AUTO_RANDOM(5, 64)",
			ExpectAutoRandomBits:      5,
			ExpectAutoRandomRangeBits: 64,
		},
		{
			ColDef:                    "BIGINT PRIMARY KEY AUTO_RANDOM(2, 32)",
			ExpectAutoRandomBits:      2,
			ExpectAutoRandomRangeBits: 32,
		},
	}
	for _, subCase := range subCases {
		createTblSQL := fmt.Sprintf("create table `db1`.`tbl1` (a %s)", subCase.ColDef)
		tblInfo, err := newTableInfo(createTblSQL, 1)
		require.Nil(t, err)
		require.Equal(t, subCase.ExpectAutoRandomBits, tblInfo.AutoRandomBits, subCase.ColDef)
		require.Equal(t, subCase.ExpectAutoRandomRangeBits, tblInfo.AutoRandomRangeBits, subCase.ColDef)
	}
}

func TestGetPreInfoGetAllTableStructures(t *testing.T) {
	dbTableDefs := map[string]map[string]tableDef{
		"db01": {
			"tbl01": {
				&colDef{
					ColName: "id",
					Def:     "INTEGER PRIMARY KEY AUTO_INCREMENT",
					TypeStr: "int",
				},
				&colDef{
					ColName: "strval",
					Def:     "VARCHAR(64)",
					TypeStr: "varchar",
				},
			},
			"tbl02": {
				&colDef{
					ColName: "id",
					Def:     "INTEGER PRIMARY KEY AUTO_INCREMENT",
					TypeStr: "int",
				},
				&colDef{
					ColName: "val",
					Def:     "VARCHAR(64)",
					TypeStr: "varchar",
				},
			},
		},
		"db02": {
			"tbl01": {
				&colDef{
					ColName: "id",
					Def:     "INTEGER PRIMARY KEY AUTO_INCREMENT",
					TypeStr: "int",
				},
				&colDef{
					ColName: "strval",
					Def:     "VARCHAR(64)",
					TypeStr: "varchar",
				},
			},
		},
	}
	testMockDataMap := tableDefsToMockDataMap(dbTableDefs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mockSrc, err := mock.NewMockImportSource(testMockDataMap)
	require.Nil(t, err)

	mockTarget := mock.NewMockTargetInfo()

	cfg := config.NewConfig()
	cfg.TikvImporter.Backend = config.BackendLocal
	ig, err := NewPreRestoreInfoGetter(cfg, mockSrc.GetAllDBFileMetas(), mockSrc.GetStorage(), mockTarget, nil, nil, WithIgnoreDBNotExist(true))
	require.NoError(t, err)
	tblStructMap, err := ig.GetAllTableStructures(ctx)
	require.Nil(t, err)
	require.Equal(t, len(dbTableDefs), len(tblStructMap), "compare db count")
	for dbName, dbInfo := range tblStructMap {
		tblDefMap, ok := dbTableDefs[dbName]
		require.Truef(t, ok, "check db exists in db definitions: %s", dbName)
		require.Equalf(t, len(tblDefMap), len(dbInfo.Tables), "compare table count: %s", dbName)
		for tblName, tblStruct := range dbInfo.Tables {
			tblDef, ok := tblDefMap[tblName]
			require.Truef(t, ok, "check table exists in table definitions: %s.%s", dbName, tblName)
			require.Equalf(t, len(tblDef), len(tblStruct.Core.Columns), "compare columns count: %s.%s", dbName, tblName)
			for i, colDef := range tblStruct.Core.Columns {
				expectColDef := tblDef[i]
				require.Equalf(t, strings.ToLower(expectColDef.ColName), colDef.Name.L, "check column name: %s.%s", dbName, tblName)
				require.Truef(t, strings.Contains(colDef.FieldType.String(), strings.ToLower(expectColDef.TypeStr)), "check column type: %s.%s", dbName, tblName)
			}
		}
	}
}

func TestGetPreInfoReadFirstRow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const testCSVData01 string = `ival,sval
111,"aaa"
222,"bbb"
`
	const testSQLData01 string = `INSERT INTO db01.tbl01 (ival, sval) VALUES (333, 'ccc');
INSERT INTO db01.tbl01 (ival, sval) VALUES (444, 'ddd');`
	testDataInfos := []struct {
		FileName             string
		Data                 string
		FirstN               int
		CSVConfig            *config.CSVConfig
		ExpectFirstRowDatums [][]types.Datum
		ExpectColumns        []string
	}{
		{
			FileName: "/db01/tbl01/data.001.csv",
			Data:     testCSVData01,
			FirstN:   1,
			ExpectFirstRowDatums: [][]types.Datum{
				{
					types.NewStringDatum("111"),
					types.NewStringDatum("aaa"),
				},
			},
			ExpectColumns: []string{"ival", "sval"},
		},
		{
			FileName: "/db01/tbl01/data.002.csv",
			Data:     testCSVData01,
			FirstN:   2,
			ExpectFirstRowDatums: [][]types.Datum{
				{
					types.NewStringDatum("111"),
					types.NewStringDatum("aaa"),
				},
				{
					types.NewStringDatum("222"),
					types.NewStringDatum("bbb"),
				},
			},
			ExpectColumns: []string{"ival", "sval"},
		},
		{
			FileName: "/db01/tbl01/data.001.sql",
			Data:     testSQLData01,
			FirstN:   1,
			ExpectFirstRowDatums: [][]types.Datum{
				{
					types.NewUintDatum(333),
					types.NewStringDatum("ccc"),
				},
			},
			ExpectColumns: []string{"ival", "sval"},
		},
		{
			FileName:             "/db01/tbl01/data.003.csv",
			Data:                 "",
			FirstN:               1,
			ExpectFirstRowDatums: [][]types.Datum{},
			ExpectColumns:        nil,
		},
		{
			FileName:             "/db01/tbl01/data.004.csv",
			Data:                 "ival,sval",
			FirstN:               1,
			ExpectFirstRowDatums: [][]types.Datum{},
			ExpectColumns:        []string{"ival", "sval"},
		},
	}
	tblMockSourceData := &mock.MockTableSourceData{
		DBName:    "db01",
		TableName: "tbl01",
		SchemaFile: &mock.MockSourceFile{
			FileName: "/db01/tbl01/tbl01.schema.sql",
			Data:     []byte("CREATE TABLE db01.tbl01(id INTEGER PRIMARY KEY AUTO_INCREMENT, ival INTEGER, sval VARCHAR(64));"),
		},
		DataFiles: []*mock.MockSourceFile{},
	}
	for _, testInfo := range testDataInfos {
		tblMockSourceData.DataFiles = append(tblMockSourceData.DataFiles, &mock.MockSourceFile{
			FileName: testInfo.FileName,
			Data:     []byte(testInfo.Data),
		})
	}
	mockDataMap := map[string]*mock.MockDBSourceData{
		"db01": {
			Name: "db01",
			Tables: map[string]*mock.MockTableSourceData{
				"tbl01": tblMockSourceData,
			},
		},
	}
	mockSrc, err := mock.NewMockImportSource(mockDataMap)
	require.Nil(t, err)
	mockTarget := mock.NewMockTargetInfo()
	cfg := config.NewConfig()
	cfg.TikvImporter.Backend = config.BackendLocal
	ig, err := NewPreRestoreInfoGetter(cfg, mockSrc.GetAllDBFileMetas(), mockSrc.GetStorage(), mockTarget, nil, nil)
	require.NoError(t, err)

	cfg.Mydumper.CSV.Header = true
	tblMeta := mockSrc.GetDBMetaMap()["db01"].Tables[0]
	for i, dataFile := range tblMeta.DataFiles {
		theDataInfo := testDataInfos[i]
		cols, rowDatums, err := ig.ReadFirstNRowsByFileMeta(ctx, dataFile.FileMeta, theDataInfo.FirstN)
		require.Nil(t, err)
		t.Logf("%v, %v", cols, rowDatums)
		require.Equal(t, theDataInfo.ExpectColumns, cols)
		require.Equal(t, theDataInfo.ExpectFirstRowDatums, rowDatums)
	}

	theDataInfo := testDataInfos[0]
	cols, rowDatums, err := ig.ReadFirstNRowsByTableName(ctx, "db01", "tbl01", theDataInfo.FirstN)
	require.NoError(t, err)
	require.Equal(t, theDataInfo.ExpectColumns, cols)
	require.Equal(t, theDataInfo.ExpectFirstRowDatums, rowDatums)
}

func TestGetPreInfoSampleSource(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dataFileName := "/db01/tbl01/tbl01.data.001.csv"
	mockDataMap := map[string]*mock.MockDBSourceData{
		"db01": {
			Name: "db01",
			Tables: map[string]*mock.MockTableSourceData{
				"tbl01": {
					DBName:    "db01",
					TableName: "tbl01",
					SchemaFile: &mock.MockSourceFile{
						FileName: "/db01/tbl01/tbl01.schema.sql",
						Data:     []byte("CREATE TABLE db01.tbl01 (id INTEGER PRIMARY KEY AUTO_INCREMENT, ival INTEGER, sval VARCHAR(64));"),
					},
					DataFiles: []*mock.MockSourceFile{
						{
							FileName: dataFileName,
							Data:     []byte(nil),
						},
					},
				},
			},
		},
	}
	mockSrc, err := mock.NewMockImportSource(mockDataMap)
	require.Nil(t, err)
	mockTarget := mock.NewMockTargetInfo()
	cfg := config.NewConfig()
	cfg.TikvImporter.Backend = config.BackendLocal
	ig, err := NewPreRestoreInfoGetter(cfg, mockSrc.GetAllDBFileMetas(), mockSrc.GetStorage(), mockTarget, nil, nil, WithIgnoreDBNotExist(true))
	require.NoError(t, err)

	mdDBMeta := mockSrc.GetAllDBFileMetas()[0]
	mdTblMeta := mdDBMeta.Tables[0]
	dbInfos, err := ig.GetAllTableStructures(ctx)
	require.NoError(t, err)

	subTests := []struct {
		Data            []byte
		ExpectIsOrdered bool
	}{
		{
			Data: []byte(`id,ival,sval
1,111,"aaa"
2,222,"bbb"
`,
			),
			ExpectIsOrdered: true,
		},
		{
			Data: []byte(`sval,ival,id
"aaa",111,1
"bbb",222,2
`,
			),
			ExpectIsOrdered: true,
		},
		{
			Data: []byte(`id,ival,sval
2,222,"bbb"
1,111,"aaa"
`,
			),
			ExpectIsOrdered: false,
		},
		{
			Data: []byte(`sval,ival,id
"aaa",111,2
"bbb",222,1
`,
			),
			ExpectIsOrdered: false,
		},
	}
	for _, subTest := range subTests {
		require.NoError(t, mockSrc.GetStorage().WriteFile(ctx, dataFileName, subTest.Data))
		sampledIndexRatio, isRowOrderedFromSample, err := ig.sampleDataFromTable(ctx, "db01", mdTblMeta, dbInfos["db01"].Tables["tbl01"].Core, nil, defaultImportantVariables)
		require.NoError(t, err)
		t.Logf("%v, %v", sampledIndexRatio, isRowOrderedFromSample)
		require.Greater(t, sampledIndexRatio, 1.0)
		require.Equal(t, subTest.ExpectIsOrdered, isRowOrderedFromSample)
	}
}

func TestGetPreInfoEstimateSourceSize(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dataFileName := "/db01/tbl01/tbl01.data.001.csv"
	testData := []byte(`id,ival,sval
1,111,"aaa"
2,222,"bbb"
`,
	)
	mockDataMap := map[string]*mock.MockDBSourceData{
		"db01": {
			Name: "db01",
			Tables: map[string]*mock.MockTableSourceData{
				"tbl01": {
					DBName:    "db01",
					TableName: "tbl01",
					SchemaFile: &mock.MockSourceFile{
						FileName: "/db01/tbl01/tbl01.schema.sql",
						Data:     []byte("CREATE TABLE db01.tbl01 (id INTEGER PRIMARY KEY AUTO_INCREMENT, ival INTEGER, sval VARCHAR(64));"),
					},
					DataFiles: []*mock.MockSourceFile{
						{
							FileName: dataFileName,
							Data:     testData,
						},
					},
				},
			},
		},
	}
	mockSrc, err := mock.NewMockImportSource(mockDataMap)
	require.Nil(t, err)
	mockTarget := mock.NewMockTargetInfo()
	cfg := config.NewConfig()
	cfg.TikvImporter.Backend = config.BackendLocal
	ig, err := NewPreRestoreInfoGetter(cfg, mockSrc.GetAllDBFileMetas(), mockSrc.GetStorage(), mockTarget, nil, nil, WithIgnoreDBNotExist(true))
	require.NoError(t, err)

	sizeResult, err := ig.EstimateSourceDataSize(ctx)
	require.NoError(t, err)
	t.Logf("estimate size: %v, file size: %v, has unsorted table: %v\n", sizeResult.SizeWithIndex, sizeResult.SizeWithoutIndex, sizeResult.HasUnsortedBigTables)
	require.GreaterOrEqual(t, sizeResult.SizeWithIndex, sizeResult.SizeWithoutIndex)
	require.Equal(t, int64(len(testData)), sizeResult.SizeWithoutIndex)
	require.False(t, sizeResult.HasUnsortedBigTables)
}

func TestGetPreInfoIsTableEmpty(t *testing.T) {
	ctx := context.TODO()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	lnConfig := config.NewConfig()
	lnConfig.TikvImporter.Backend = config.BackendLocal
	targetGetter, err := NewTargetInfoGetterImpl(lnConfig, db)
	require.NoError(t, err)
	require.Equal(t, lnConfig, targetGetter.cfg)

	mock.ExpectQuery("SELECT 1 FROM `test_db`.`test_tbl` LIMIT 1").
		WillReturnError(&mysql_sql_driver.MySQLError{
			Number:  errno.ErrNoSuchTable,
			Message: "Table 'test_db.test_tbl' doesn't exist",
		})
	pIsEmpty, err := targetGetter.IsTableEmpty(ctx, "test_db", "test_tbl")
	require.NoError(t, err)
	require.NotNil(t, pIsEmpty)
	require.Equal(t, true, *pIsEmpty)

	mock.ExpectQuery("SELECT 1 FROM `test_db`.`test_tbl` LIMIT 1").
		WillReturnRows(
			sqlmock.NewRows([]string{"1"}).
				RowError(0, sql.ErrNoRows),
		)
	pIsEmpty, err = targetGetter.IsTableEmpty(ctx, "test_db", "test_tbl")
	require.NoError(t, err)
	require.NotNil(t, pIsEmpty)
	require.Equal(t, true, *pIsEmpty)

	mock.ExpectQuery("SELECT 1 FROM `test_db`.`test_tbl` LIMIT 1").
		WillReturnRows(
			sqlmock.NewRows([]string{"1"}).AddRow(1),
		)
	pIsEmpty, err = targetGetter.IsTableEmpty(ctx, "test_db", "test_tbl")
	require.NoError(t, err)
	require.NotNil(t, pIsEmpty)
	require.Equal(t, false, *pIsEmpty)

	mock.ExpectQuery("SELECT 1 FROM `test_db`.`test_tbl` LIMIT 1").
		WillReturnError(errors.New("some dummy error"))
	_, err = targetGetter.IsTableEmpty(ctx, "test_db", "test_tbl")
	require.Error(t, err)
}
