// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package restore_test

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/pingcap/failpoint"
	backuppb "github.com/pingcap/kvproto/pkg/brpb"
	"github.com/pingcap/kvproto/pkg/import_sstpb"
	"github.com/pingcap/kvproto/pkg/metapb"
	berrors "github.com/pingcap/tidb/br/pkg/errors"
	"github.com/pingcap/tidb/br/pkg/gluetidb"
	"github.com/pingcap/tidb/br/pkg/metautil"
	"github.com/pingcap/tidb/br/pkg/mock"
	"github.com/pingcap/tidb/br/pkg/restore"
	"github.com/pingcap/tidb/br/pkg/restore/tiflashrec"
	"github.com/pingcap/tidb/br/pkg/stream"
	"github.com/pingcap/tidb/br/pkg/utils"
	"github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/parser/mysql"
	"github.com/pingcap/tidb/parser/types"
	"github.com/pingcap/tidb/tablecodec"
	"github.com/stretchr/testify/require"
	pd "github.com/tikv/pd/client"
	"golang.org/x/exp/slices"
	"google.golang.org/grpc/keepalive"
)

var mc *mock.Cluster

var defaultKeepaliveCfg = keepalive.ClientParameters{
	Time:    3 * time.Second,
	Timeout: 10 * time.Second,
}

func TestCreateTables(t *testing.T) {
	m := mc
	g := gluetidb.New()
	client := restore.NewRestoreClient(m.PDClient, nil, defaultKeepaliveCfg, false)
	err := client.Init(g, m.Storage)
	require.NoError(t, err)

	info, err := m.Domain.GetSnapshotInfoSchema(math.MaxUint64)
	require.NoError(t, err)
	dbSchema, isExist := info.SchemaByName(model.NewCIStr("test"))
	require.True(t, isExist)

	client.SetBatchDdlSize(1)
	tables := make([]*metautil.Table, 4)
	intField := types.NewFieldType(mysql.TypeLong)
	intField.SetCharset("binary")
	for i := len(tables) - 1; i >= 0; i-- {
		tables[i] = &metautil.Table{
			DB: dbSchema,
			Info: &model.TableInfo{
				ID:   int64(i),
				Name: model.NewCIStr("test" + strconv.Itoa(i)),
				Columns: []*model.ColumnInfo{{
					ID:        1,
					Name:      model.NewCIStr("id"),
					FieldType: *intField,
					State:     model.StatePublic,
				}},
				Charset: "utf8mb4",
				Collate: "utf8mb4_bin",
			},
		}
	}
	rules, newTables, err := client.CreateTables(m.Domain, tables, 0)
	require.NoError(t, err)
	// make sure tables and newTables have same order
	for i, tbl := range tables {
		require.Equal(t, tbl.Info.Name, newTables[i].Name)
	}
	for _, nt := range newTables {
		require.Regexp(t, "test[0-3]", nt.Name.String())
	}
	oldTableIDExist := make(map[int64]bool)
	newTableIDExist := make(map[int64]bool)
	for _, tr := range rules.Data {
		oldTableID := tablecodec.DecodeTableID(tr.GetOldKeyPrefix())
		require.False(t, oldTableIDExist[oldTableID], "table rule duplicate old table id")
		oldTableIDExist[oldTableID] = true

		newTableID := tablecodec.DecodeTableID(tr.GetNewKeyPrefix())
		require.False(t, newTableIDExist[newTableID], "table rule duplicate new table id")
		newTableIDExist[newTableID] = true
	}

	for i := 0; i < len(tables); i++ {
		require.True(t, oldTableIDExist[int64(i)], "table rule does not exist")
	}
}

func TestIsOnline(t *testing.T) {
	m := mc
	g := gluetidb.New()
	client := restore.NewRestoreClient(m.PDClient, nil, defaultKeepaliveCfg, false)
	err := client.Init(g, m.Storage)
	require.NoError(t, err)

	require.False(t, client.IsOnline())
	client.EnableOnline()
	require.True(t, client.IsOnline())
}

func getStartedMockedCluster(t *testing.T) *mock.Cluster {
	t.Helper()
	cluster, err := mock.NewCluster()
	require.NoError(t, err)
	err = cluster.Start()
	require.NoError(t, err)
	return cluster
}

func TestCheckTargetClusterFresh(t *testing.T) {
	// cannot use shared `mc`, other parallel case may change it.
	cluster := getStartedMockedCluster(t)
	defer cluster.Stop()

	g := gluetidb.New()
	client := restore.NewRestoreClient(cluster.PDClient, nil, defaultKeepaliveCfg, false)
	err := client.Init(g, cluster.Storage)
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, client.CheckTargetClusterFresh(ctx))

	require.NoError(t, client.CreateDatabase(ctx, &model.DBInfo{Name: model.NewCIStr("user_db")}))
	require.True(t, berrors.ErrRestoreNotFreshCluster.Equal(client.CheckTargetClusterFresh(ctx)))
}

func TestCheckTargetClusterFreshWithTable(t *testing.T) {
	// cannot use shared `mc`, other parallel case may change it.
	cluster := getStartedMockedCluster(t)
	defer cluster.Stop()

	g := gluetidb.New()
	client := restore.NewRestoreClient(cluster.PDClient, nil, defaultKeepaliveCfg, false)
	err := client.Init(g, cluster.Storage)
	require.NoError(t, err)

	ctx := context.Background()
	info, err := cluster.Domain.GetSnapshotInfoSchema(math.MaxUint64)
	require.NoError(t, err)
	dbSchema, isExist := info.SchemaByName(model.NewCIStr("test"))
	require.True(t, isExist)
	intField := types.NewFieldType(mysql.TypeLong)
	intField.SetCharset("binary")
	table := &metautil.Table{
		DB: dbSchema,
		Info: &model.TableInfo{
			ID:   int64(1),
			Name: model.NewCIStr("t"),
			Columns: []*model.ColumnInfo{{
				ID:        1,
				Name:      model.NewCIStr("id"),
				FieldType: *intField,
				State:     model.StatePublic,
			}},
			Charset: "utf8mb4",
			Collate: "utf8mb4_bin",
		},
	}
	_, _, err = client.CreateTables(cluster.Domain, []*metautil.Table{table}, 0)
	require.NoError(t, err)

	require.True(t, berrors.ErrRestoreNotFreshCluster.Equal(client.CheckTargetClusterFresh(ctx)))
}

func TestCheckSysTableCompatibility(t *testing.T) {
	cluster := mc
	g := gluetidb.New()
	client := restore.NewRestoreClient(cluster.PDClient, nil, defaultKeepaliveCfg, false)
	err := client.Init(g, cluster.Storage)
	require.NoError(t, err)

	info, err := cluster.Domain.GetSnapshotInfoSchema(math.MaxUint64)
	require.NoError(t, err)
	dbSchema, isExist := info.SchemaByName(model.NewCIStr(mysql.SystemDB))
	require.True(t, isExist)
	tmpSysDB := dbSchema.Clone()
	tmpSysDB.Name = utils.TemporaryDBName(mysql.SystemDB)
	sysDB := model.NewCIStr(mysql.SystemDB)
	userTI, err := client.GetTableSchema(cluster.Domain, sysDB, model.NewCIStr("user"))
	require.NoError(t, err)

	// column count mismatch
	mockedUserTI := userTI.Clone()
	mockedUserTI.Columns = mockedUserTI.Columns[:len(mockedUserTI.Columns)-1]
	err = client.CheckSysTableCompatibility(cluster.Domain, []*metautil.Table{{
		DB:   tmpSysDB,
		Info: mockedUserTI,
	}})
	require.True(t, berrors.ErrRestoreIncompatibleSys.Equal(err))

	// column order mismatch(success)
	mockedUserTI = userTI.Clone()
	mockedUserTI.Columns[4], mockedUserTI.Columns[5] = mockedUserTI.Columns[5], mockedUserTI.Columns[4]
	err = client.CheckSysTableCompatibility(cluster.Domain, []*metautil.Table{{
		DB:   tmpSysDB,
		Info: mockedUserTI,
	}})
	require.NoError(t, err)

	// missing column
	mockedUserTI = userTI.Clone()
	mockedUserTI.Columns[0].Name = model.NewCIStr("new-name")
	err = client.CheckSysTableCompatibility(cluster.Domain, []*metautil.Table{{
		DB:   tmpSysDB,
		Info: mockedUserTI,
	}})
	require.True(t, berrors.ErrRestoreIncompatibleSys.Equal(err))

	// incompatible column type
	mockedUserTI = userTI.Clone()
	mockedUserTI.Columns[0].FieldType.SetFlen(2000) // Columns[0] is `Host` char(255)
	err = client.CheckSysTableCompatibility(cluster.Domain, []*metautil.Table{{
		DB:   tmpSysDB,
		Info: mockedUserTI,
	}})
	require.True(t, berrors.ErrRestoreIncompatibleSys.Equal(err))

	// compatible
	mockedUserTI = userTI.Clone()
	err = client.CheckSysTableCompatibility(cluster.Domain, []*metautil.Table{{
		DB:   tmpSysDB,
		Info: mockedUserTI,
	}})
	require.NoError(t, err)
}

func TestInitFullClusterRestore(t *testing.T) {
	cluster := mc
	g := gluetidb.New()
	client := restore.NewRestoreClient(cluster.PDClient, nil, defaultKeepaliveCfg, false)
	err := client.Init(g, cluster.Storage)
	require.NoError(t, err)

	// explicit filter
	client.InitFullClusterRestore(true)
	require.False(t, client.IsFullClusterRestore())

	client.InitFullClusterRestore(false)
	require.True(t, client.IsFullClusterRestore())
	// set it to false again
	client.InitFullClusterRestore(true)
	require.False(t, client.IsFullClusterRestore())

	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/br/pkg/restore/mock-incr-backup-data", "return(true)"))
	defer func() {
		require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/br/pkg/restore/mock-incr-backup-data"))
	}()
	client.InitFullClusterRestore(false)
	require.False(t, client.IsFullClusterRestore())
}

func TestPreCheckTableClusterIndex(t *testing.T) {
	m := mc
	g := gluetidb.New()
	client := restore.NewRestoreClient(m.PDClient, nil, defaultKeepaliveCfg, false)
	err := client.Init(g, m.Storage)
	require.NoError(t, err)

	info, err := m.Domain.GetSnapshotInfoSchema(math.MaxUint64)
	require.NoError(t, err)
	dbSchema, isExist := info.SchemaByName(model.NewCIStr("test"))
	require.True(t, isExist)

	tables := make([]*metautil.Table, 4)
	intField := types.NewFieldType(mysql.TypeLong)
	intField.SetCharset("binary")
	for i := len(tables) - 1; i >= 0; i-- {
		tables[i] = &metautil.Table{
			DB: dbSchema,
			Info: &model.TableInfo{
				ID:   int64(i),
				Name: model.NewCIStr("test" + strconv.Itoa(i)),
				Columns: []*model.ColumnInfo{{
					ID:        1,
					Name:      model.NewCIStr("id"),
					FieldType: *intField,
					State:     model.StatePublic,
				}},
				Charset: "utf8mb4",
				Collate: "utf8mb4_bin",
			},
		}
	}
	_, _, err = client.CreateTables(m.Domain, tables, 0)
	require.NoError(t, err)

	// exist different tables
	tables[1].Info.IsCommonHandle = true
	err = client.PreCheckTableClusterIndex(tables, nil, m.Domain)
	require.Error(t, err)
	require.Regexp(t, `.*@@tidb_enable_clustered_index should be ON \(backup table = true, created table = false\).*`, err.Error())

	// exist different DDLs
	jobs := []*model.Job{{
		ID:         5,
		Type:       model.ActionCreateTable,
		SchemaName: "test",
		Query:      "",
		BinlogInfo: &model.HistoryInfo{
			TableInfo: &model.TableInfo{
				Name:           model.NewCIStr("test1"),
				IsCommonHandle: true,
			},
		},
	}}
	err = client.PreCheckTableClusterIndex(nil, jobs, m.Domain)
	require.Error(t, err)
	require.Regexp(t, `.*@@tidb_enable_clustered_index should be ON \(backup table = true, created table = false\).*`, err.Error())

	// should pass pre-check cluster index
	tables[1].Info.IsCommonHandle = false
	jobs[0].BinlogInfo.TableInfo.IsCommonHandle = false
	require.Nil(t, client.PreCheckTableClusterIndex(tables, jobs, m.Domain))
}

type fakePDClient struct {
	pd.Client
	stores []*metapb.Store
}

func (fpdc fakePDClient) GetAllStores(context.Context, ...pd.GetStoreOption) ([]*metapb.Store, error) {
	return append([]*metapb.Store{}, fpdc.stores...), nil
}

func TestPreCheckTableTiFlashReplicas(t *testing.T) {
	m := mc
	mockStores := []*metapb.Store{
		{
			Id: 1,
			Labels: []*metapb.StoreLabel{
				{
					Key:   "engine",
					Value: "tiflash",
				},
			},
		},
		{
			Id: 2,
			Labels: []*metapb.StoreLabel{
				{
					Key:   "engine",
					Value: "tiflash",
				},
			},
		},
	}

	g := gluetidb.New()
	client := restore.NewRestoreClient(fakePDClient{
		stores: mockStores,
	}, nil, defaultKeepaliveCfg, false)
	err := client.Init(g, m.Storage)
	require.NoError(t, err)

	tables := make([]*metautil.Table, 4)
	for i := 0; i < len(tables); i++ {
		tiflashReplica := &model.TiFlashReplicaInfo{
			Count: uint64(i),
		}
		if i == 0 {
			tiflashReplica = nil
		}

		tables[i] = &metautil.Table{
			DB: nil,
			Info: &model.TableInfo{
				ID:             int64(i),
				Name:           model.NewCIStr("test" + strconv.Itoa(i)),
				TiFlashReplica: tiflashReplica,
			},
		}
	}
	ctx := context.Background()
	require.Nil(t, client.PreCheckTableTiFlashReplica(ctx, tables, nil))

	for i := 0; i < len(tables); i++ {
		if i == 0 || i > 2 {
			require.Nil(t, tables[i].Info.TiFlashReplica)
		} else {
			require.NotNil(t, tables[i].Info.TiFlashReplica)
			obtainCount := int(tables[i].Info.TiFlashReplica.Count)
			require.Equal(t, i, obtainCount)
		}
	}

	require.Nil(t, client.PreCheckTableTiFlashReplica(ctx, tables, tiflashrec.New()))
	for i := 0; i < len(tables); i++ {
		require.Nil(t, tables[i].Info.TiFlashReplica)
	}
}

// Mock ImporterClient interface
type FakeImporterClient struct {
	restore.ImporterClient
}

// Record the stores that have communicated
type RecordStores struct {
	mu     sync.Mutex
	stores []uint64
}

func NewRecordStores() RecordStores {
	return RecordStores{stores: make([]uint64, 0)}
}

func (r *RecordStores) put(id uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stores = append(r.stores, id)
}

func (r *RecordStores) sort() {
	r.mu.Lock()
	defer r.mu.Unlock()
	slices.Sort(r.stores)
}

func (r *RecordStores) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stores)
}

func (r *RecordStores) get(i int) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stores[i]
}

func (r *RecordStores) toString() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return fmt.Sprintf("%v", r.stores)
}

var recordStores RecordStores

const (
	SET_SPEED_LIMIT_ERROR = 999999
	WORKING_TIME          = 100
)

func (fakeImportCli FakeImporterClient) SetDownloadSpeedLimit(
	ctx context.Context,
	storeID uint64,
	req *import_sstpb.SetDownloadSpeedLimitRequest,
) (*import_sstpb.SetDownloadSpeedLimitResponse, error) {
	if storeID == SET_SPEED_LIMIT_ERROR {
		return nil, fmt.Errorf("storeID:%v ERROR", storeID)
	}

	time.Sleep(WORKING_TIME * time.Millisecond) // simulate doing 100 ms work
	recordStores.put(storeID)
	return nil, nil
}

func TestSetSpeedLimit(t *testing.T) {
	mockStores := []*metapb.Store{
		{Id: 1},
		{Id: 2},
		{Id: 3},
		{Id: 4},
		{Id: 5},
		{Id: 6},
		{Id: 7},
		{Id: 8},
		{Id: 9},
		{Id: 10},
	}

	// 1. The cost of concurrent communication is expected to be less than the cost of serial communication.
	client := restore.NewRestoreClient(fakePDClient{
		stores: mockStores,
	}, nil, defaultKeepaliveCfg, false)
	ctx := context.Background()

	recordStores = NewRecordStores()
	start := time.Now()
	err := restore.MockCallSetSpeedLimit(ctx, FakeImporterClient{}, client, 10)
	cost := time.Since(start)
	require.NoError(t, err)

	recordStores.sort()
	t.Logf("Total Cost: %v\n", cost)
	t.Logf("Has Communicated: %v\n", recordStores.toString())

	serialCost := len(mockStores) * WORKING_TIME
	require.Less(t, cost, time.Duration(serialCost)*time.Millisecond)
	require.Equal(t, len(mockStores), recordStores.len())
	for i := 0; i < recordStores.len(); i++ {
		require.Equal(t, mockStores[i].Id, recordStores.get(i))
	}

	// 2. Expect the number of communicated stores to be less than the length of the mockStore
	// Because subsequent unstarted communications are aborted when an error is encountered.
	recordStores = NewRecordStores()
	mockStores[5].Id = SET_SPEED_LIMIT_ERROR // setting a fault store
	client = restore.NewRestoreClient(fakePDClient{
		stores: mockStores,
	}, nil, defaultKeepaliveCfg, false)

	// Concurrency needs to be less than the number of stores
	err = restore.MockCallSetSpeedLimit(ctx, FakeImporterClient{}, client, 2)
	require.Error(t, err)
	t.Log(err)

	recordStores.sort()
	sort.Slice(mockStores, func(i, j int) bool { return mockStores[i].Id < mockStores[j].Id })
	t.Logf("Has Communicated: %v\n", recordStores.toString())
	require.Less(t, recordStores.len(), len(mockStores))
	for i := 0; i < recordStores.len(); i++ {
		require.Equal(t, mockStores[i].Id, recordStores.get(i))
	}
}

func TestDeleteRangeQuery(t *testing.T) {
	ctx := context.Background()
	m := mc
	mockStores := []*metapb.Store{
		{
			Id: 1,
			Labels: []*metapb.StoreLabel{
				{
					Key:   "engine",
					Value: "tiflash",
				},
			},
		},
		{
			Id: 2,
			Labels: []*metapb.StoreLabel{
				{
					Key:   "engine",
					Value: "tiflash",
				},
			},
		},
	}

	g := gluetidb.New()
	client := restore.NewRestoreClient(fakePDClient{
		stores: mockStores,
	}, nil, defaultKeepaliveCfg, false)
	err := client.Init(g, m.Storage)
	require.NoError(t, err)

	client.RunGCRowsLoader(ctx)

	client.InsertDeleteRangeForTable(2, []int64{3})
	client.InsertDeleteRangeForTable(4, []int64{5, 6})

	elementID := int64(1)
	client.InsertDeleteRangeForIndex(7, &elementID, 8, []int64{1})
	client.InsertDeleteRangeForIndex(9, &elementID, 10, []int64{1, 2})

	querys := client.GetGCRows()
	require.Equal(t, querys[0], "INSERT IGNORE INTO mysql.gc_delete_range VALUES (2, 1, '748000000000000003', '748000000000000004', %[1]d)")
	require.Equal(t, querys[1], "INSERT IGNORE INTO mysql.gc_delete_range VALUES (4, 1, '748000000000000005', '748000000000000006', %[1]d),(4, 2, '748000000000000006', '748000000000000007', %[1]d)")
	require.Equal(t, querys[2], "INSERT IGNORE INTO mysql.gc_delete_range VALUES (7, 1, '7480000000000000085f698000000000000001', '7480000000000000085f698000000000000002', %[1]d)")
	require.Equal(t, querys[3], "INSERT IGNORE INTO mysql.gc_delete_range VALUES (9, 2, '74800000000000000a5f698000000000000001', '74800000000000000a5f698000000000000002', %[1]d),(9, 3, '74800000000000000a5f698000000000000002', '74800000000000000a5f698000000000000003', %[1]d)")
}

func TestRestoreMetaKVFilesWithBatchMethod1(t *testing.T) {
	files := []*backuppb.DataFileInfo{}
	batchCount := 0

	client := restore.MockClient(nil)
	err := client.RestoreMetaKVFilesWithBatchMethod(
		context.Background(),
		files,
		nil,
		nil,
		nil,
		func(
			ctx context.Context,
			files []*backuppb.DataFileInfo,
			schemasReplace *stream.SchemasReplace,
			updateStats func(kvCount uint64, size uint64),
			progressInc func(),
		) error {
			batchCount++
			return nil
		},
	)
	require.Nil(t, err)
	require.Equal(t, batchCount, 0)
}

func TestRestoreMetaKVFilesWithBatchMethod2(t *testing.T) {
	files := []*backuppb.DataFileInfo{
		{
			Path:  "f1",
			MinTs: 100,
			MaxTs: 120,
		},
	}
	batchCount := 0
	result := make(map[int][]*backuppb.DataFileInfo)

	client := restore.MockClient(nil)
	err := client.RestoreMetaKVFilesWithBatchMethod(
		context.Background(),
		files,
		nil,
		nil,
		nil,
		func(
			ctx context.Context,
			fs []*backuppb.DataFileInfo,
			schemasReplace *stream.SchemasReplace,
			updateStats func(kvCount uint64, size uint64),
			progressInc func(),
		) error {
			result[batchCount] = fs
			batchCount++
			return nil
		},
	)
	require.Nil(t, err)
	require.Equal(t, batchCount, 1)
	require.Equal(t, len(result), 1)
	require.Equal(t, result[0], files)
}

func TestRestoreMetaKVFilesWithBatchMethod3(t *testing.T) {
	files := []*backuppb.DataFileInfo{
		{
			Path:  "f1",
			MinTs: 100,
			MaxTs: 120,
		},
		{
			Path:  "f2",
			MinTs: 100,
			MaxTs: 120,
		},
		{
			Path:  "f3",
			MinTs: 110,
			MaxTs: 130,
		},
		{
			Path:  "f4",
			MinTs: 140,
			MaxTs: 150,
		},
		{
			Path:  "f5",
			MinTs: 150,
			MaxTs: 160,
		},
	}
	batchCount := 0
	result := make(map[int][]*backuppb.DataFileInfo)

	client := restore.MockClient(nil)
	err := client.RestoreMetaKVFilesWithBatchMethod(
		context.Background(),
		files,
		nil,
		nil,
		nil,
		func(
			ctx context.Context,
			fs []*backuppb.DataFileInfo,
			schemasReplace *stream.SchemasReplace,
			updateStats func(kvCount uint64, size uint64),
			progressInc func(),
		) error {
			result[batchCount] = fs
			batchCount++
			return nil
		},
	)
	require.Nil(t, err)
	require.Equal(t, len(result), 2)
	require.Equal(t, result[0], files[0:3])
	require.Equal(t, result[1], files[3:])
}

func TestRestoreMetaKVFilesWithBatchMethod4(t *testing.T) {
	files := []*backuppb.DataFileInfo{
		{
			Path:  "f1",
			MinTs: 100,
			MaxTs: 100,
		},
		{
			Path:  "f2",
			MinTs: 100,
			MaxTs: 100,
		},
		{
			Path:  "f3",
			MinTs: 110,
			MaxTs: 130,
		},
		{
			Path:  "f4",
			MinTs: 110,
			MaxTs: 150,
		},
	}
	batchCount := 0
	result := make(map[int][]*backuppb.DataFileInfo)

	client := restore.MockClient(nil)
	err := client.RestoreMetaKVFilesWithBatchMethod(
		context.Background(),
		files,
		nil,
		nil,
		nil,
		func(
			ctx context.Context,
			fs []*backuppb.DataFileInfo,
			schemasReplace *stream.SchemasReplace,
			updateStats func(kvCount uint64, size uint64),
			progressInc func(),
		) error {
			result[batchCount] = fs
			batchCount++
			return nil
		},
	)
	require.Nil(t, err)
	require.Equal(t, len(result), 2)
	require.Equal(t, result[0], files[0:2])
	require.Equal(t, result[1], files[2:])
}

func TestSortMetaKVFiles(t *testing.T) {
	files := []*backuppb.DataFileInfo{
		{
			Path:       "f5",
			MinTs:      110,
			MaxTs:      150,
			ResolvedTs: 120,
		},
		{
			Path:       "f1",
			MinTs:      100,
			MaxTs:      100,
			ResolvedTs: 80,
		},
		{
			Path:       "f2",
			MinTs:      100,
			MaxTs:      100,
			ResolvedTs: 90,
		},
		{
			Path:       "f4",
			MinTs:      110,
			MaxTs:      130,
			ResolvedTs: 120,
		},
		{
			Path:       "f3",
			MinTs:      105,
			MaxTs:      130,
			ResolvedTs: 100,
		},
	}

	files = restore.SortMetaKVFiles(files)
	require.Equal(t, len(files), 5)
	require.Equal(t, files[0].Path, "f1")
	require.Equal(t, files[1].Path, "f2")
	require.Equal(t, files[2].Path, "f3")
	require.Equal(t, files[3].Path, "f4")
	require.Equal(t, files[4].Path, "f5")
}
