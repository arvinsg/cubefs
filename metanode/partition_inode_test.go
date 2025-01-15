package metanode

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"math"
	"math/rand"
	"os"
	"path"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubefs/cubefs/metanode/metamock"
	"github.com/cubefs/cubefs/proto"
	se "github.com/cubefs/cubefs/util/sortedextent"
	"github.com/cubefs/cubefs/util/unit"
	"github.com/stretchr/testify/assert"
)

func mockMetaPartitionReplica(nodeID, partitionID uint64, storeMode proto.StoreMode, rootDir string) *metaPartition {
	partitionDir := path.Join(rootDir, partitionPrefix+strconv.Itoa(int(partitionID)))
	os.MkdirAll(partitionDir, 0666)
	node := &MetaNode{
		nodeId: nodeID,
	}
	node.initFetchTopologyManager()
	manager := &metadataManager{
		nodeId:      1,
		metaNode:    node,
		rocksDBDirs: []string{rootDir},
		rootDir:     rootDir,
	}

	config := &MetaPartitionConfig{
		PartitionId: partitionID,
		NodeId:      nodeID,
		Start:       0,
		End:         math.MaxUint64 - 100,
		Peers:       []proto.Peer{proto.Peer{ID: 1, Addr: "127.0.0.1"}, {ID: 2, Addr: "127.0.0.2"}},
		RootDir:     partitionDir,
		StoreMode:   storeMode,
		Cursor:      math.MaxUint64 - 100000,
		RocksDBDir:  rootDir,
	}

	mp, err := CreateMetaPartition(config, manager)
	if err != nil {
		fmt.Printf("create meta partition failed:%s", err.Error())
		return nil
	}
	return mp.(*metaPartition)
}

func mockMp(t *testing.T, dir string, leaderStoreMode proto.StoreMode) (leader, follower *metaPartition) {
	leaderRootDir := path.Join("./leader", dir)
	os.RemoveAll(leaderRootDir)
	if leader = mockMetaPartitionReplica(1, 1, leaderStoreMode, leaderRootDir); leader == nil {
		t.Errorf("mock metapartition failed")
		return
	}

	followerRootDir := path.Join("./follower", dir)
	os.RemoveAll(followerRootDir)
	if follower = mockMetaPartitionReplica(1, 1,
		(proto.StoreModeMem|proto.StoreModeRocksDb)-leaderStoreMode, followerRootDir); follower == nil {
		t.Errorf("mock metapartition failed")
		return
	}

	raftPartition := metamock.NewMockPartition(1)
	raftPartition.Apply = ApplyMock
	raftPartition.Mp = append(raftPartition.Mp, leader)
	raftPartition.Mp = append(raftPartition.Mp, follower)

	leader.raftPartition = raftPartition
	follower.raftPartition = raftPartition
	return
}

func releaseMp(leader, follower *metaPartition, dir string) {
	leader.db.CloseDb()
	follower.db.CloseDb()
	leader.db.ReleaseRocksDb()
	follower.db.ReleaseRocksDb()
	os.RemoveAll("./leader")
	os.RemoveAll("./follower")
}

func CreateInodeInterTest(t *testing.T, leader, follower *metaPartition, start uint64) {
	reqCreateInode := &proto.CreateInodeRequest{
		PartitionID: leader.config.PartitionId,
		Gid:         0,
		Uid:         0,
		Mode:        470,
	}
	resp := &Packet{}
	var err error
	cursor := leader.config.Cursor
	defer func() {
		leader.config.Cursor = cursor
	}()
	leader.config.Cursor = start

	for i := 0; i < 100; i++ {
		err = leader.CreateInode(reqCreateInode, resp)
		if err != nil {
			t.Errorf("create inode failed:%s", err.Error())
			return
		}
	}
	if leader.inodeTree.Count() != follower.inodeTree.Count() {
		t.Errorf("create inode failed, rocks mem not same, mem:%d, rocks:%d", leader.inodeTree.Count(), follower.inodeTree.Count())
		return
	}
	t.Logf("create 100 inodes success")

	cursor = leader.config.Cursor
	leader.config.Cursor = leader.config.End
	err = leader.CreateInode(reqCreateInode, resp)
	if err == nil {
		t.Errorf("cursor reach end failed")
		return
	}
	t.Logf("cursor reach end test  success:%s, result:%d, %s", err.Error(), resp.ResultCode, resp.GetResultMsg())

	leader.config.Cursor = 10 + start

	inode, _ := leader.inodeTree.Get(10)
	if inode == nil {
		t.Errorf("get inode 10 failed, err:%s", err.Error())
		return
	}
	err = leader.CreateInode(reqCreateInode, resp)
	if resp.ResultCode == proto.OpOk {
		t.Errorf("same inode create failed")
		return
	}
	t.Logf("same inode create success:%v, resuclt code:%d, %s", err, resp.ResultCode, resp.GetResultMsg())
}

func TestMetaPartition_CreateInodeCase01(t *testing.T) {
	//leader is mem mode
	dir := "create_inode_test_01"
	leader, follower := mockMp(t, dir, proto.StoreModeMem)
	CreateInodeInterTest(t, leader, follower, 0)
	releaseMp(leader, follower, dir)

	//leader is rocksdb mode
	leader, follower = mockMp(t, dir, proto.StoreModeRocksDb)
	CreateInodeInterTest(t, leader, follower, 0)
	releaseMp(leader, follower, dir)
}

func TestMetaPartition_CreateInodeNewCase01(t *testing.T) {
	tests := []struct {
		name      string
		storeMode proto.StoreMode
		rootDir   string
		applyFunc metamock.ApplyFunc
	}{
		{
			name:      "MemMode",
			storeMode: proto.StoreModeMem,
			rootDir:   "./test_mem_inode_create_01",
			applyFunc: ApplyMock,
		},
		{
			name:      "RocksDBMode",
			storeMode: proto.StoreModeRocksDb,
			rootDir:   "./test_rocksdb_inode_create_01",
			applyFunc: ApplyMock,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mp, err := mockMetaPartition(1, 1, test.storeMode, test.rootDir, test.applyFunc)
			if err != nil {
				return
			}
			defer releaseMetaPartition(mp)
			reqCreateInode := &proto.CreateInodeRequest{
				PartitionID: mp.config.PartitionId,
				Gid:         0,
				Uid:         0,
				Mode:        470,
			}
			resp := &Packet{}
			mp.config.Cursor = 1

			for i := 0; i < 100; i++ {
				err = mp.CreateInode(reqCreateInode, resp)
				if err != nil {
					t.Errorf("create inode failed:%s", err.Error())
					return
				}
			}
			if mp.inodeTree.Count() != 100 {
				t.Errorf("create inode failed, rocks mem not same, expect:100, actual:%d", mp.inodeTree.Count())
				return
			}
			t.Logf("create 100 inodes success")

			mp.config.Cursor = mp.config.End
			err = mp.CreateInode(reqCreateInode, resp)
			if err == nil {
				t.Errorf("cursor reach end failed")
				return
			}
			t.Logf("cursor reach end test  success:%s, result:%d, %s", err.Error(), resp.ResultCode, resp.GetResultMsg())

			mp.config.Cursor = 10
			inode, _ := mp.inodeTree.Get(10)
			if inode == nil {
				t.Errorf("get inode 10 failed, err:%s", err.Error())
				return
			}
			err = mp.CreateInode(reqCreateInode, resp)
			if resp.ResultCode == proto.OpOk {
				t.Errorf("same inode create failed")
				return
			}
			t.Logf("same inode create success:%v, resuclt code:%d, %s", err, resp.ResultCode, resp.GetResultMsg())
		})
	}
}

func UnlinkInodeInterTest(t *testing.T, leader, follower *metaPartition, start uint64) {
	cursor := atomic.LoadUint64(&leader.config.Cursor)
	defer func() {
		atomic.StoreUint64(&leader.config.Cursor, cursor)
	}()

	reqCreateInode := &proto.CreateInodeRequest{
		PartitionID: leader.config.PartitionId,
		Gid:         0,
		Uid:         0,
		Mode:        470,
	}
	rand.Seed(time.Now().UnixMilli())
	reqUnlinkInode := &proto.UnlinkInodeRequest{
		Inode:           10 + cursor,
		ClientIP:        uint32(rand.Int31n(math.MaxInt32)),
		ClientStartTime: time.Now().Unix(),
		ClientID:        uint64(rand.Int63n(math.MaxInt64)),
	}
	var resp = &Packet{}
	resp.Data, _ = json.Marshal(reqUnlinkInode)
	resp.Size = uint32(len(resp.Data))
	resp.CRC = crc32.ChecksumIEEE(resp.Data[:resp.Size])
	resp.ReqID = rand.Int63n(math.MaxInt64)
	var err error

	for i := 0; i < 100; i++ {
		err = leader.CreateInode(reqCreateInode, resp)
		if err != nil {
			t.Errorf("create inode failed:%s", err.Error())
			return
		}
	}

	if leader.inodeTree.Count() != follower.inodeTree.Count() {
		t.Errorf("create inode failed, rocks mem not same, mem:%d, rocks:%d", leader.inodeTree.Count(), follower.inodeTree.Count())
		return
	}
	t.Logf("create 100 inodes success")

	inode, _ := leader.inodeTree.Get(10 + cursor)
	if inode == nil {
		t.Errorf("get inode (%v) failed, inode is null", start+10+leader.config.Cursor)
		return
	}
	inode.Type = uint32(os.ModeDir)
	inode.NLink = 3
	if os.FileMode(inode.Type).IsDir() {
		t.Logf("inode is dir")
	}
	_ = inodePut(leader.inodeTree, inode)

	inode, _ = follower.inodeTree.Get(10 + cursor)
	if inode == nil {
		t.Errorf("get inode (%v) failed, inode is null", start+10+leader.config.Cursor)
		return
	}
	inode.Type = uint32(os.ModeDir)
	inode.NLink = 3
	_ = inodePut(follower.inodeTree, inode)

	err = leader.UnlinkInode(reqUnlinkInode, resp)
	if resp.ResultCode != proto.OpOk {
		t.Errorf("unlink inode test failed:%v, resuclt code:%d, %s", err, resp.ResultCode, resp.GetResultMsg())
		return
	}

	resp.ReqID = rand.Int63n(math.MaxInt64)
	err = leader.UnlinkInode(reqUnlinkInode, resp)
	if resp.ResultCode != proto.OpOk {
		t.Errorf("unlink inode test failed:%v, resuclt code:%d, %s", err, resp.ResultCode, resp.GetResultMsg())
		return
	}

	resp.ReqID = rand.Int63n(math.MaxInt64)
	err = leader.UnlinkInode(reqUnlinkInode, resp)
	if resp.ResultCode == proto.OpOk {
		t.Errorf("unlink failed, inode link:%d", inode.NLink)
		return
	}
	t.Logf("unlink inode test success:%v, resuclt code:%d, %s", err, resp.ResultCode, resp.GetResultMsg())

	inode, _ = leader.inodeTree.Get(11 + cursor)
	if inode == nil {
		t.Errorf("get inode (%v) failed, inode is null", start+11+leader.config.Cursor)
		return
	}
	inode.SetDeleteMark()
	_ = inodePut(leader.inodeTree, inode)

	inode, _ = follower.inodeTree.Get(11 + cursor)
	if inode == nil {
		t.Errorf("get inode (%v) failed, inode is null", start+11+leader.config.Cursor)
		return
	}
	inode.SetDeleteMark()
	_ = inodePut(follower.inodeTree, inode)

	reqUnlinkInode.Inode = 11 + cursor
	resp.ReqID = rand.Int63n(math.MaxInt64)
	err = leader.UnlinkInode(reqUnlinkInode, resp)
	if resp.ResultCode == proto.OpOk {
		t.Errorf("same inode create failed, inode link:%d", inode.NLink)
		return
	}
	t.Logf("unlink inode test success:%v, resuclt code:%d, %s", err, resp.ResultCode, resp.GetResultMsg())
}

func TestMetaPartition_UnlinkInodeCase01(t *testing.T) {
	//leader is mem mode
	dir := "unlink_inode_test_01"
	leader, follower := mockMp(t, dir, proto.StoreModeMem)
	UnlinkInodeInterTest(t, leader, follower, 0)
	releaseMp(leader, follower, dir)

	//leader is rocksdb mode
	leader, follower = mockMp(t, dir, proto.StoreModeRocksDb)
	UnlinkInodeInterTest(t, leader, follower, 0)
	releaseMp(leader, follower, dir)
}

func createInodesForTest(leader, follower *metaPartition, inodeCnt int, mode, uid, gid uint32) (inos []uint64, err error) {
	reqCreateInode := &proto.CreateInodeRequest{
		PartitionID: 1,
		Gid:         gid,
		Uid:         uid,
		Mode:        mode,
	}

	inos = make([]uint64, 0, inodeCnt)
	for index := 0; index < inodeCnt; index++ {
		packet := &Packet{}
		err = leader.CreateInode(reqCreateInode, packet)
		if err != nil {
			err = fmt.Errorf("create inode failed:%s", err.Error())
			return
		}
		resp := &proto.CreateInodeResponse{}
		if err = packet.UnmarshalData(resp); err != nil {
			err = fmt.Errorf("unmarshal create inode response failed:%v", err)
			return
		}
		inos = append(inos, resp.Info.Inode)
	}

	//validate count and inode info
	if leader.inodeTree.Count() != follower.inodeTree.Count() {
		err = fmt.Errorf("create inode failed, leader and follower inode count not same, or mismatch expect,"+
			" mem:%d, rocks:%d, expect:%v", leader.inodeTree.Count(), follower.inodeTree.Count(), inodeCnt)
		return
	}

	for _, ino := range inos {
		inodeFromLeader, _ := leader.inodeTree.Get(ino)
		inodeFromFollower, _ := follower.inodeTree.Get(ino)
		if inodeFromLeader == nil || inodeFromFollower == nil {
			err = fmt.Errorf("get inode result not same, leader:%s, follower:%s", inodeFromLeader.String(), inodeFromFollower.String())
			return
		}
		if !reflect.DeepEqual(inodeFromFollower, inodeFromFollower) {
			err = fmt.Errorf("inode info in leader is not equal to follower, leader:%s, follower:%s", inodeFromLeader.String(), inodeFromFollower.String())
			return
		}
	}
	return
}

func BatchInodeUnlinkInterTest(t *testing.T, leader, follower *metaPartition) {
	var (
		inos []uint64
		err  error
	)
	defer func() {
		for _, ino := range inos {
			req := &proto.DeleteInodeRequest{
				Inode: ino,
			}
			packet := &Packet{}
			leader.DeleteInode(req, packet)
		}
		if leader.inodeTree.Count() != follower.inodeTree.Count() || leader.inodeTree.Count() != 0 {
			t.Errorf("inode count must be zero after delete, but result is not expect, count[leader:%v, follower:%v]", leader.inodeTree.Count(), follower.inodeTree.Count())
			return
		}
	}()
	if inos, err = createInodesForTest(leader, follower, 100, uint32(os.ModeDir), 0, 0); err != nil || len(inos) != 100 {
		t.Fatal(err)
		return
	}
	testInos := inos[20:40]
	for _, ino := range testInos {
		//create nlink for dir
		rand.Seed(time.Now().UnixMilli())
		req := &proto.LinkInodeRequest{
			Inode:           ino,
			ClientIP:        uint32(rand.Int31n(math.MaxInt32)),
			ClientStartTime: time.Now().Unix(),
			ClientID:        uint64(rand.Int63n(math.MaxInt64)),
		}
		packet := &Packet{}
		packet.Data, _ = json.Marshal(req)
		packet.Size = uint32(len(packet.Data))
		packet.CRC = crc32.ChecksumIEEE(packet.Data[:packet.Size])
		packet.ReqID = rand.Int63n(math.MaxInt64)
		if err = leader.CreateInodeLink(req, packet); err != nil || packet.ResultCode != proto.OpOk {
			t.Errorf("create inode link failed, err:%v, resultCode:%v", err, packet.ResultCode)
			return
		}
	}

	req := &proto.BatchUnlinkInodeRequest{
		Inodes: testInos,
	}
	packet := &Packet{}
	//unlink to empty dir
	if err = leader.UnlinkInodeBatch(req, packet); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("batch unlink inode failed, [err:%v, packet result code:%v]", err, packet.ResultCode)
		return
	}

	//unlink empty dir, inode will be delete
	packet = &Packet{}
	if err = leader.UnlinkInodeBatch(req, packet); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("batch unlink inode failed, [err:%v, packet result code:%v]", err, packet.ResultCode)
		return
	}

	for _, ino := range testInos {
		inodeInMem, _ := leader.inodeTree.Get(ino)
		if inodeInMem != nil {
			t.Errorf("test failed, inode get from mem mode mp error, [expect:inode is null, actual:%s]", inodeInMem.String())
			return
		}

		inodeInRocks, _ := follower.inodeTree.Get(ino)
		if inodeInRocks != nil {
			t.Errorf("test failed, inode get from rocks mode mp error, [expect:inode is null, actual:%s]", inodeInRocks.String())
			return
		}
	}
	return
}

//todo:test unlink batch when batchInodes include same inodeID
func TestMetaPartition_UnlinkInodeBatch01(t *testing.T) {
	//leader is mem mode
	dir := "unlink_inode_batch_test_01"
	leader, follower := mockMp(t, dir, proto.StoreModeMem)
	BatchInodeUnlinkInterTest(t, leader, follower)
	releaseMp(leader, follower, dir)

	//leader is rocksdb mode
	leader, follower = mockMp(t, dir, proto.StoreModeRocksDb)
	BatchInodeUnlinkInterTest(t, leader, follower)
	releaseMp(leader, follower, dir)
}

func TestMetaPartition_UnlinkInodeBatchCase02(t *testing.T) {
	mp, err := mockMetaPartition(1, 1, proto.StoreModeMem, "./test_batch_unlink_inode", ApplyMock)
	if mp == nil {
		t.Logf("mock metapartition failed:%v", err)
		t.FailNow()
	}
	defer releaseMetaPartition(mp)

	_, _, err = inodeCreate(mp.inodeTree, NewInode(1, uint32(os.ModeDir)), true)
	if err != nil {
		t.Logf("create inode failed:%v", err)
		t.FailNow()
	}

	var inode *Inode
	if inode, err = mp.inodeTree.Get(1); err != nil {
		t.Logf("get exist inode:%v failed:%v", inode, err)
		t.FailNow()
	}
	//inc nlink
	for index := 0; index < 3; index++ {
		inode.IncNLink()
	}

	if err = inodePut(mp.inodeTree, inode); err != nil {
		t.Logf("update inode nlink failed:%v", err)
		t.FailNow()
	}

	testInos := []uint64{1, 1, 1}

	req := &proto.BatchUnlinkInodeRequest{
		Inodes: testInos,
	}
	packet := &Packet{}
	//unlink to empty dir
	if err = mp.UnlinkInodeBatch(req, packet); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("batch unlink inode failed, [err:%v, packet result code:%v]", err, packet.ResultCode)
		return
	}

	if inode, err = mp.inodeTree.Get(1); err != nil {
		t.Logf("get exist inode:%v failed:%v", inode, err)
		t.FailNow()
	}

	if inode.NLink != 2 {
		t.Logf("test batch nlink inode failed, expect nlink:2, actual:%v", inode.NLink)
		t.FailNow()
	}

	req = &proto.BatchUnlinkInodeRequest{
		Inodes: []uint64{1, 1},
	}
	//unlink empty dir
	if err = mp.UnlinkInodeBatch(req, packet); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("batch unlink inode failed, [err:%v, packet result code:%v]", err, packet.ResultCode)
		return
	}

	if inode, err = mp.inodeTree.Get(1); err != nil {
		t.Logf("get inode failed:%v", err)
		t.FailNow()
	}
	if inode != nil {
		t.Logf("batch uinlink inode test failed, unlink empty dir result expect:get nil, actual:%v", inode)
		t.FailNow()
	}
}

func TestMetaPartition_UnlinkInodeBatchCase03(t *testing.T) {
	mp, err := mockMetaPartition(1, 1, proto.StoreModeMem, "./test_batch_unlink_inode", ApplyMock)
	if mp == nil {
		t.Logf("mock metapartition failed:%v", err)
		t.FailNow()
	}
	defer releaseMetaPartition(mp)

	_, _, err = inodeCreate(mp.inodeTree, NewInode(100, 470), true)
	if err != nil {
		t.Logf("create inode failed:%v", err)
		t.FailNow()
	}

	var inode *Inode
	if inode, err = mp.inodeTree.Get(100); err != nil {
		t.Logf("get exist inode:%v failed:%v", inode, err)
		t.FailNow()
	}
	//inc nlink
	for index := 0; index < 3; index++ {
		inode.IncNLink()
	}

	if err = inodePut(mp.inodeTree, inode); err != nil {
		t.Logf("update inode nlink failed:%v", err)
		t.FailNow()
	}

	testInos := []uint64{100, 100, 100}

	req := &proto.BatchUnlinkInodeRequest{
		Inodes: testInos,
	}
	packet := &Packet{}

	if err = mp.UnlinkInodeBatch(req, packet); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("batch unlink inode failed, [err:%v, packet result code:%v]", err, packet.ResultCode)
		return
	}

	if inode, err = mp.inodeTree.Get(100); err != nil {
		t.Logf("get exist inode:%v failed:%v", inode, err)
		t.FailNow()
	}

	if inode.NLink != 1 {
		t.Logf("test batch nlink inode failed, expect nlink:2, actual:%v", inode.NLink)
		t.FailNow()
	}

	req = &proto.BatchUnlinkInodeRequest{
		Inodes: []uint64{100, 100},
	}

	if err = mp.UnlinkInodeBatch(req, packet); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("batch unlink inode failed, [err:%v, packet result code:%v]", err, packet.ResultCode)
		return
	}

	if inode, err = mp.inodeTree.Get(100); err != nil {
		t.Logf("get inode failed:%v", err)
		t.FailNow()
	}
	if inode == nil {
		t.Logf("batch uinlink inode test failed, unlink file result error, expect exist, actual not exist")
		t.FailNow()
	}

	if inode.NLink != 0 {
		t.Logf("nlink mismatch, expect:0, actual:%v", inode.NLink)
		t.FailNow()
	}
}

func InodeGetInterGet(t *testing.T, leader, follower *metaPartition) {
	//create inode
	ino, err := createInode(470, 0, 0, leader)
	req := &proto.InodeGetRequest{
		Inode: ino,
	}
	packet := &Packet{}
	if err = leader.InodeGet(req, packet, proto.OpInodeGetVersion1); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("get exist inode from leader failed, [err:%v, resultCode:%v]", err, packet.ResultCode)
		return
	}

	resp := &proto.InodeGetResponse{}
	err = json.Unmarshal(packet.Data[:packet.Size], resp)
	assert.Equal(t, nil, err, "unmarshal resp failed")
	assert.Equal(t, 0, len(resp.ExtendAttrs))

	packet = &Packet{}
	if err = follower.InodeGet(req, packet, proto.OpInodeGetVersion1); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("get exist inode from follower failed, [err:%v, resultCode:%v]", err, packet.ResultCode)
		return
	}
	resp = &proto.InodeGetResponse{}
	err = json.Unmarshal(packet.Data[:packet.Size], resp)
	assert.Equal(t, nil, err, "unmarshal resp failed")
	assert.Equal(t, 0, len(resp.ExtendAttrs))

	//get not exist inode
	req = &proto.InodeGetRequest{
		Inode: ino - 1,
	}
	packet = &Packet{}
	if err = leader.InodeGet(req, packet, proto.OpInodeGetVersion1); err == nil || packet.ResultCode != proto.OpNotExistErr {
		t.Errorf("get not exist inode from leader success, [err:%v, resultCode:%v]", err, packet.ResultCode)
		return
	}

	packet = &Packet{}
	if err = follower.InodeGet(req, packet, proto.OpInodeGetVersion1); err == nil || packet.ResultCode != proto.OpNotExistErr {
		t.Errorf("get not exist inode from follower success, [err:%v, resultCode:%v]", err, packet.ResultCode)
		return
	}
	return
}

func TestMetaPartition_InodeGetCase01(t *testing.T) {
	//leader is mem mode
	dir := "inode_get_test_01"
	leader, follower := mockMp(t, dir, proto.StoreModeMem)
	InodeGetInterGet(t, leader, follower)
	releaseMp(leader, follower, dir)

	//leader is rocksdb mode
	leader, follower = mockMp(t, dir, proto.StoreModeRocksDb)
	InodeGetInterGet(t, leader, follower)
	releaseMp(leader, follower, dir)
}

func setXAttr(inodeID uint64, keys, values []string, mp *metaPartition) (err error) {
	rand.Seed(time.Now().UnixNano())
	clientIP := uint32(rand.Int31n(math.MaxInt32))
	clientStartTime := time.Now().Unix()
	clientID := uint64(rand.Int63n(math.MaxInt64))
	if len(keys) != len(values) {
		err = fmt.Errorf("diff length between keys array and values array")
		return
	}
	for index := 0; index < len(keys); index++ {
		req := &proto.SetXAttrRequest{
			Inode:           inodeID,
			Key:             keys[index],
			Value:           values[index],
			ClientStartTime: clientStartTime,
			ClientIP:        clientIP,
			ClientID:        clientID,
		}
		packet := &Packet{}
		packet.Data, _ = json.Marshal(req)
		packet.Size = uint32(len(packet.Data))
		packet.CRC = crc32.ChecksumIEEE(packet.Data[:packet.Size])
		packet.ReqID = rand.Int63n(math.MaxInt64)
		if err = mp.SetXAttr(req, packet); err != nil || packet.ResultCode != proto.OpOk {
			err = fmt.Errorf("set XAttr failed: %v", err)
			return
		}
	}
	return
}

func InodeGetWithXAttrInnerGet(t *testing.T, leader, follower *metaPartition) {
	//create inode
	ino, err := createInode(470, 0, 0, leader)
	assert.Equal(t, nil, err, "create inode failed")

	keys := []string{"cfs_lock", "extend_attr_1", "extend_attr_2"}
	values := []string{"cfs_lock_test", "extend_attr_1_test", "extend_attr_2_test"}
	err = setXAttr(ino, keys, values, leader)
	assert.Equal(t, nil, err, "set attr failed")

	req := &proto.InodeGetRequest{
		Inode:          ino,
		WithExtendAttr: true,
		ExtendAttrKeys: []string{"cfs_lock"},
	}
	packet := &Packet{}
	err = leader.InodeGet(req, packet, proto.OpInodeGetVersion2)
	assert.Equal(t, nil, err, "get inode failed")
	assert.Equal(t, proto.OpOk, packet.ResultCode, "get inode with error code")

	resp := &proto.InodeGetResponse{}
	err = json.Unmarshal(packet.Data[:packet.Size], resp)
	assert.Equal(t, nil, err, "unmarshal resp failed")

	assert.Equal(t, 1, len(resp.ExtendAttrs))
	assert.Equal(t, "cfs_lock", resp.ExtendAttrs[0].Name)
	assert.Equal(t, "cfs_lock_test", resp.ExtendAttrs[0].Value)

	packet = &Packet{}
	err = follower.InodeGet(req, packet, proto.OpInodeGetVersion2)
	assert.Equal(t, nil, err, "get inode failed")
	assert.Equal(t, proto.OpOk, packet.ResultCode, "get inode with error code")

	resp = &proto.InodeGetResponse{}
	err = json.Unmarshal(packet.Data[:packet.Size], resp)
	assert.Equal(t, nil, err, "unmarshal resp failed")

	assert.Equal(t, 1, len(resp.ExtendAttrs))
	assert.Equal(t, "cfs_lock", resp.ExtendAttrs[0].Name)
	assert.Equal(t, "cfs_lock_test", resp.ExtendAttrs[0].Value)

	req = &proto.InodeGetRequest{
		Inode:          ino,
		WithExtendAttr: true,
	}
	packet = &Packet{}
	err = leader.InodeGet(req, packet, proto.OpInodeGetVersion2)
	assert.Equal(t, nil, err, "get inode failed")
	assert.Equal(t, proto.OpOk, packet.ResultCode, "get inode with error code")

	resp = &proto.InodeGetResponse{}
	err = json.Unmarshal(packet.Data[:packet.Size], resp)
	assert.Equal(t, nil, err, "unmarshal resp failed")

	assert.Equal(t, 3, len(resp.ExtendAttrs))

	packet = &Packet{}
	err = follower.InodeGet(req, packet, proto.OpInodeGetVersion2)
	assert.Equal(t, nil, err, "get inode failed")
	assert.Equal(t, proto.OpOk, packet.ResultCode, "get inode with error code")

	resp = &proto.InodeGetResponse{}
	err = json.Unmarshal(packet.Data[:packet.Size], resp)
	assert.Equal(t, nil, err, "unmarshal resp failed")

	assert.Equal(t, 3, len(resp.ExtendAttrs))
	return
}

func TestMetaPartition_InodeGetCase02(t *testing.T) {
	//leader is mem mode
	dir := "inode_get_test_02"
	leader, follower := mockMp(t, dir, proto.StoreModeMem)
	InodeGetWithXAttrInnerGet(t, leader, follower)
	releaseMp(leader, follower, dir)

	//leader is rocksdb mode
	leader, follower = mockMp(t, dir, proto.StoreModeRocksDb)
	InodeGetWithXAttrInnerGet(t, leader, follower)
	releaseMp(leader, follower, dir)
}

func BatchInodeGetInterTest(t *testing.T, leader, follower *metaPartition) {
	var (
		inos []uint64
		err  error
	)
	defer func() {
		for _, ino := range inos {
			req := &proto.DeleteInodeRequest{
				Inode: ino,
			}
			packet := &Packet{}
			leader.DeleteInode(req, packet)
		}
		if leader.inodeTree.Count() != follower.inodeTree.Count() || leader.inodeTree.Count() != 0 {
			t.Errorf("inode count must be zero after delete, but result is not expect, count[leader:%v, follower:%v]", leader.inodeTree.Count(), follower.inodeTree.Count())
			return
		}
	}()
	if inos, err = createInodesForTest(leader, follower, 100, 470, 0, 0); err != nil || len(inos) != 100 {
		t.Fatal(err)
		return
	}

	testIno := inos[20:50]

	req := &proto.BatchInodeGetRequest{
		Inodes: testIno,
	}
	packet := &Packet{}
	if err = leader.InodeGetBatch(req, packet); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("batch get inode from leader failed, [err:%v, resultCode:%v]", err, packet.ResultCode)
		return
	}
	resp := &proto.BatchInodeGetResponse{}
	if err = packet.UnmarshalData(resp); err != nil {
		t.Errorf("unmarshal batch inode get response failed:%v", err)
		return
	}
	if len(resp.Infos) != len(testIno) {
		t.Fatalf("get inode count not equla to expect, [expect:%v, actual:%v]", len(testIno), len(resp.Infos))
		return
	}
	assert.Equal(t, 0, len(resp.ExtendAttrs), "inode extend attrs with error count")

	packet = &Packet{}
	if err = follower.InodeGetBatch(req, packet); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("batch get inode from follower failed, [err:%v, resultCode:%v]", err, packet.ResultCode)
		return
	}
	resp = &proto.BatchInodeGetResponse{}
	if err = packet.UnmarshalData(resp); err != nil {
		t.Errorf("unmarshal batch inode get response failed:%v", err)
		return
	}
	if len(resp.Infos) != len(testIno) {
		t.Errorf("get inode count not equla to expect, [expect:%v, actual:%v]", len(testIno), len(resp.Infos))
		return
	}
	assert.Equal(t, 0, len(resp.ExtendAttrs), "inode extend attrs with error count")
	return
}

func BatchInodeGetPbInterTest(t *testing.T, leader, follower *metaPartition) {
	var (
		inos []uint64
		err  error
	)
	defer func() {
		for _, ino := range inos {
			req := &proto.DeleteInodeRequest{
				Inode: ino,
			}
			packet := &Packet{}
			leader.DeleteInode(req, packet)
		}
		if leader.inodeTree.Count() != follower.inodeTree.Count() || leader.inodeTree.Count() != 0 {
			t.Errorf("inode count must be zero after delete, but result is not expect, count[leader:%v, follower:%v]", leader.inodeTree.Count(), follower.inodeTree.Count())
			return
		}
	}()
	if inos, err = createInodesForTest(leader, follower, 100, 470, 0, 0); err != nil || len(inos) != 100 {
		t.Fatal(err)
		return
	}

	testIno := inos[20:50]

	req := &proto.BatchInodeGetRequest{
		Inodes: testIno,
	}
	packet := &Packet{}
	data := make([]byte, 1*unit.MB)
	if err = leader.InodeGetBatchPb(req, packet, data); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("batch get inode from leader failed, [err:%v, resultCode:%v]", err, packet.ResultCode)
		return
	}
	resp := &proto.BatchInodeGetResponsePb{}
	if err = packet.UnmarshalDataPb(resp); err != nil {
		t.Errorf("unmarshal batch inode get response failed:%v", err)
		return
	}
	if len(resp.Infos) != len(testIno) {
		t.Fatalf("get inode count not equla to expect, [expect:%v, actual:%v]", len(testIno), len(resp.Infos))
		return
	}
	assert.Equal(t, 0, len(resp.ExtendAttrs), "inode extend attrs with error count")

	packet = &Packet{}
	data = make([]byte, 1*unit.MB)
	if err = follower.InodeGetBatchPb(req, packet, data); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("batch get inode from follower failed, [err:%v, resultCode:%v]", err, packet.ResultCode)
		return
	}
	resp = &proto.BatchInodeGetResponsePb{}
	if err = packet.UnmarshalDataPb(resp); err != nil {
		t.Errorf("unmarshal batch inode get response failed:%v", err)
		return
	}
	if len(resp.Infos) != len(testIno) {
		t.Errorf("get inode count not equla to expect, [expect:%v, actual:%v]", len(testIno), len(resp.Infos))
		return
	}
	assert.Equal(t, 0, len(resp.ExtendAttrs), "inode extend attrs with error count")
	return
}

func TestMetaPartition_BatchInodeGetCase01(t *testing.T) {
	//leader is mem mode
	dir := "./batch_inode_get_test_01"
	defer os.RemoveAll(dir)
	leader, follower := mockMp(t, dir, proto.StoreModeMem)
	BatchInodeGetInterTest(t, leader, follower)
	releaseMp(leader, follower, dir)

	//leader is rocksdb mode
	leader, follower = mockMp(t, dir, proto.StoreModeRocksDb)
	BatchInodeGetInterTest(t, leader, follower)
	releaseMp(leader, follower, dir)
}

func TestMetaPartition_BatchInodeGetPbCase01(t *testing.T) {
	//leader is mem mode
	dir := "./batch_inode_get_pb_test_01"
	defer os.RemoveAll(dir)
	leader, follower := mockMp(t, dir, proto.StoreModeMem)
	BatchInodeGetPbInterTest(t, leader, follower)
	releaseMp(leader, follower, dir)

	//leader is rocksdb mode
	leader, follower = mockMp(t, dir, proto.StoreModeRocksDb)
	BatchInodeGetPbInterTest(t, leader, follower)
	releaseMp(leader, follower, dir)
}

func BatchInodeGetWithXAttrInterTest(t *testing.T, leader, follower *metaPartition) {
	var (
		inos []uint64
		err  error
	)
	defer func() {
		for _, ino := range inos {
			req := &proto.DeleteInodeRequest{
				Inode: ino,
			}
			packet := &Packet{}
			leader.DeleteInode(req, packet)
		}
		assert.Equal(t, uint64(0), leader.inodeTree.Count(), "inode count expect 0")
		assert.Equal(t, leader.inodeTree.Count(), follower.inodeTree.Count(), "inode count not equal between leader and follower")
	}()
	if inos, err = createInodesForTest(leader, follower, 100, 470, 0, 0); err != nil || len(inos) != 100 {
		t.Fatal(err)
		return
	}

	keys := []string{"cfs_lock", "extend_attr_1", "extend_attr_2"}
	values := []string{"cfs_lock_test", "extend_attr_1_test", "extend_attr_2_test"}
	for _, ino := range inos {
		err = setXAttr(ino, keys, values, leader)
		assert.Equal(t, nil, err, "set attr failed")
	}

	testIno := inos[20:50]

	req := &proto.BatchInodeGetRequest{
		Inodes:         testIno,
		WithExtendAttr: true,
		ExtendAttrKeys: []string{"extend_attr_1"},
	}
	packet := &Packet{}
	err = leader.InodeGetBatch(req, packet)
	assert.Equal(t, nil, err, "batch get inode failed")
	assert.Equal(t, proto.OpOk, packet.ResultCode, "batch get inode with error result code")

	resp := &proto.BatchInodeGetResponse{}
	err = packet.UnmarshalData(resp)
	assert.Equal(t, nil, err, "unmarshal batch get inode resp failed")

	assert.Equal(t, len(testIno), len(resp.Infos), "inode resp info with error count")
	assert.Equal(t, len(testIno), len(resp.ExtendAttrs), "inode extend attrs with error count")
	for index, extendAttr := range resp.ExtendAttrs {
		assert.Equal(t, testIno[index], extendAttr.InodeID)
		assert.Equal(t, 1, len(extendAttr.ExtendAttrs))
		assert.Equal(t, "extend_attr_1", extendAttr.ExtendAttrs[0].Name)
		assert.Equal(t, "extend_attr_1_test", extendAttr.ExtendAttrs[0].Value)
	}

	packet = &Packet{}
	err = follower.InodeGetBatch(req, packet)
	assert.Equal(t, nil, err, "batch get inode failed")
	assert.Equal(t, proto.OpOk, packet.ResultCode, "batch get inode with error result code")

	resp = &proto.BatchInodeGetResponse{}
	err = packet.UnmarshalData(resp)
	assert.Equal(t, nil, err, "unmarshal batch get inode resp failed")

	assert.Equal(t, len(testIno), len(resp.Infos), "inode resp info with error count")
	assert.Equal(t, len(testIno), len(resp.ExtendAttrs), "inode extend attrs with error count")
	for index, extendAttr := range resp.ExtendAttrs {
		assert.Equal(t, testIno[index], extendAttr.InodeID)
		assert.Equal(t, 1, len(extendAttr.ExtendAttrs))
		assert.Equal(t, "extend_attr_1", extendAttr.ExtendAttrs[0].Name)
		assert.Equal(t, "extend_attr_1_test", extendAttr.ExtendAttrs[0].Value)
	}

	req = &proto.BatchInodeGetRequest{
		Inodes:         testIno,
		WithExtendAttr: true,
	}
	packet = &Packet{}
	err = leader.InodeGetBatch(req, packet)
	assert.Equal(t, nil, err, "batch get inode failed")
	assert.Equal(t, proto.OpOk, packet.ResultCode, "batch get inode with error result code")

	resp = &proto.BatchInodeGetResponse{}
	err = packet.UnmarshalData(resp)
	assert.Equal(t, nil, err, "unmarshal batch get inode resp failed")

	assert.Equal(t, len(testIno), len(resp.Infos), "inode resp info with error count")
	assert.Equal(t, len(testIno), len(resp.ExtendAttrs), "inode extend attrs with error count")
	for index, extendAttr := range resp.ExtendAttrs {
		assert.Equal(t, testIno[index], extendAttr.InodeID)
		assert.Equal(t, 3, len(extendAttr.ExtendAttrs))
	}

	packet = &Packet{}
	err = follower.InodeGetBatch(req, packet)
	assert.Equal(t, nil, err, "batch get inode failed")
	assert.Equal(t, proto.OpOk, packet.ResultCode, "batch get inode with error result code")

	resp = &proto.BatchInodeGetResponse{}
	err = packet.UnmarshalData(resp)
	assert.Equal(t, nil, err, "unmarshal batch get inode resp failed")

	assert.Equal(t, len(testIno), len(resp.Infos), "inode resp info with error count")
	assert.Equal(t, len(testIno), len(resp.ExtendAttrs), "inode extend attrs with error count")
	for index, extendAttr := range resp.ExtendAttrs {
		assert.Equal(t, testIno[index], extendAttr.InodeID)
		assert.Equal(t, 3, len(extendAttr.ExtendAttrs))
	}
	return
}

func BatchInodeGetWithXAttrPbInterTest(t *testing.T, leader, follower *metaPartition) {
	var (
		inos []uint64
		err  error
	)
	defer func() {
		for _, ino := range inos {
			req := &proto.DeleteInodeRequest{
				Inode: ino,
			}
			packet := &Packet{}
			leader.DeleteInode(req, packet)
		}
		assert.Equal(t, uint64(0), leader.inodeTree.Count(), "inode count expect 0")
		assert.Equal(t, leader.inodeTree.Count(), follower.inodeTree.Count(), "inode count not equal between leader and follower")
	}()
	if inos, err = createInodesForTest(leader, follower, 100, 470, 0, 0); err != nil || len(inos) != 100 {
		t.Fatal(err)
		return
	}

	keys := []string{"cfs_lock", "extend_attr_1", "extend_attr_2"}
	values := []string{"cfs_lock_test", "extend_attr_1_test", "extend_attr_2_test"}
	for _, ino := range inos {
		err = setXAttr(ino, keys, values, leader)
		assert.Equal(t, nil, err, "set attr failed")
	}

	testIno := inos[20:50]

	req := &proto.BatchInodeGetRequest{
		Inodes:         testIno,
		WithExtendAttr: true,
		ExtendAttrKeys: []string{"extend_attr_1"},
	}
	packet := &Packet{}
	data := make([]byte, 1*unit.MB)
	err = leader.InodeGetBatchPb(req, packet, data)
	assert.Equal(t, nil, err, "batch get inode failed")
	assert.Equal(t, proto.OpOk, packet.ResultCode, "batch get inode with error result code")

	resp := &proto.BatchInodeGetResponsePb{}
	err = packet.UnmarshalDataPb(resp)
	assert.Equal(t, nil, err, "unmarshal batch get inode resp failed")

	assert.Equal(t, len(testIno), len(resp.Infos), "inode resp info with error count")
	assert.Equal(t, len(testIno), len(resp.ExtendAttrs), "inode extend attrs with error count")
	for index, extendAttr := range resp.ExtendAttrs {
		assert.Equal(t, testIno[index], extendAttr.InodeID)
		assert.Equal(t, 1, len(extendAttr.ExtendAttrs))
		assert.Equal(t, "extend_attr_1", extendAttr.ExtendAttrs[0].Name)
		assert.Equal(t, "extend_attr_1_test", extendAttr.ExtendAttrs[0].Value)
	}

	packet = &Packet{}
	data = make([]byte, 1*unit.MB)
	err = follower.InodeGetBatchPb(req, packet, data)
	assert.Equal(t, nil, err, "batch get inode failed")
	assert.Equal(t, proto.OpOk, packet.ResultCode, "batch get inode with error result code")

	resp = &proto.BatchInodeGetResponsePb{}
	err = packet.UnmarshalDataPb(resp)
	assert.Equal(t, nil, err, "unmarshal batch get inode resp failed")

	assert.Equal(t, len(testIno), len(resp.Infos), "inode resp info with error count")
	assert.Equal(t, len(testIno), len(resp.ExtendAttrs), "inode extend attrs with error count")
	for index, extendAttr := range resp.ExtendAttrs {
		assert.Equal(t, testIno[index], extendAttr.InodeID)
		assert.Equal(t, 1, len(extendAttr.ExtendAttrs))
		assert.Equal(t, "extend_attr_1", extendAttr.ExtendAttrs[0].Name)
		assert.Equal(t, "extend_attr_1_test", extendAttr.ExtendAttrs[0].Value)
	}

	req = &proto.BatchInodeGetRequest{
		Inodes:         testIno,
		WithExtendAttr: true,
	}
	packet = &Packet{}
	data = make([]byte, 1*unit.MB)
	err = leader.InodeGetBatchPb(req, packet, data)
	assert.Equal(t, nil, err, "batch get inode failed")
	assert.Equal(t, proto.OpOk, packet.ResultCode, "batch get inode with error result code")

	resp = &proto.BatchInodeGetResponsePb{}
	err = packet.UnmarshalDataPb(resp)
	assert.Equal(t, nil, err, "unmarshal batch get inode resp failed")

	assert.Equal(t, len(testIno), len(resp.Infos), "inode resp info with error count")
	assert.Equal(t, len(testIno), len(resp.ExtendAttrs), "inode extend attrs with error count")
	for index, extendAttr := range resp.ExtendAttrs {
		assert.Equal(t, testIno[index], extendAttr.InodeID)
		assert.Equal(t, 3, len(extendAttr.ExtendAttrs))
	}

	packet = &Packet{}
	data = make([]byte, 1*unit.MB)
	err = follower.InodeGetBatchPb(req, packet, data)
	assert.Equal(t, nil, err, "batch get inode failed")
	assert.Equal(t, proto.OpOk, packet.ResultCode, "batch get inode with error result code")

	resp = &proto.BatchInodeGetResponsePb{}
	err = packet.UnmarshalDataPb(resp)
	assert.Equal(t, nil, err, "unmarshal batch get inode resp failed")

	assert.Equal(t, len(testIno), len(resp.Infos), "inode resp info with error count")
	assert.Equal(t, len(testIno), len(resp.ExtendAttrs), "inode extend attrs with error count")
	for index, extendAttr := range resp.ExtendAttrs {
		assert.Equal(t, testIno[index], extendAttr.InodeID)
		assert.Equal(t, 3, len(extendAttr.ExtendAttrs))
	}
	return
}

func TestMetaPartition_BatchInodeGetCase02(t *testing.T) {
	//leader is mem mode
	dir := "batch_inode_get_test_02"
	leader, follower := mockMp(t, dir, proto.StoreModeMem)
	BatchInodeGetWithXAttrInterTest(t, leader, follower)
	releaseMp(leader, follower, dir)

	//leader is rocksdb mode
	leader, follower = mockMp(t, dir, proto.StoreModeRocksDb)
	BatchInodeGetWithXAttrInterTest(t, leader, follower)
	releaseMp(leader, follower, dir)
}

func TestMetaPartition_BatchInodeGetPbCase02(t *testing.T) {
	//leader is mem mode
	dir := "batch_inode_get_pb_test_02"
	leader, follower := mockMp(t, dir, proto.StoreModeMem)
	BatchInodeGetWithXAttrPbInterTest(t, leader, follower)
	releaseMp(leader, follower, dir)

	//leader is rocksdb mode
	leader, follower = mockMp(t, dir, proto.StoreModeRocksDb)
	BatchInodeGetWithXAttrPbInterTest(t, leader, follower)
	releaseMp(leader, follower, dir)
}

func CreateInodeLinkInterTest(t *testing.T, leader, follower *metaPartition) {
	ino, err := createInode(470, 0, 0, leader)
	if err != nil {
		t.Fatal(err)
		return
	}
	rand.Seed(time.Now().UnixMilli())
	req := &proto.LinkInodeRequest{
		Inode:           ino,
		ClientIP:        uint32(rand.Int31n(math.MaxInt32)),
		ClientStartTime: time.Now().Unix(),
		ClientID:        uint64(rand.Int63n(math.MaxInt64)),
	}
	packet := &Packet{}
	packet.Data, _ = json.Marshal(req)
	packet.Size = uint32(len(packet.Data))
	packet.CRC = crc32.ChecksumIEEE(packet.Data[:packet.Size])
	packet.ReqID = rand.Int63n(math.MaxInt64)
	if err = leader.CreateInodeLink(req, packet); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("create inode link failed, err:%v, resultCode:%v", err, packet.ResultCode)
		return
	}

	//validate
	var inode *Inode
	if inode, _ = leader.inodeTree.Get(ino); inode == nil {
		t.Errorf("get exist inode failed, inode is nil")
		return
	}
	if inode.NLink != 2 {
		t.Errorf("inode nlink is error, [expect:2, actual:%v]", inode.NLink)
		return
	}

	if inode, _ = follower.inodeTree.Get(ino); inode == nil {
		t.Errorf("get exist inode failed, inode is nil")
		return
	}
	if inode.NLink != 2 {
		t.Errorf("inode nlink is error, [expect:2, actual:%v]", inode.NLink)
		return
	}

	//create inode link for not exist inode
	rand.Seed(time.Now().UnixMilli())
	req = &proto.LinkInodeRequest{
		Inode:           math.MaxUint64 - 10,
		ClientIP:        uint32(rand.Int31n(math.MaxInt32)),
		ClientStartTime: time.Now().Unix(),
		ClientID:        uint64(rand.Int63n(math.MaxInt64)),
	}
	packet = &Packet{}
	packet.Data, _ = json.Marshal(req)
	packet.Size = uint32(len(packet.Data))
	packet.CRC = crc32.ChecksumIEEE(packet.Data[:packet.Size])
	packet.ReqID = rand.Int63n(math.MaxInt64)
	if _ = leader.CreateInodeLink(req, packet); packet.ResultCode != proto.OpInodeOutOfRange {
		t.Errorf("create inode link for not exist inode failed, expect result code is OpInodeOutOfRange, "+
			"but actual result is:0x%X", packet.ResultCode)
		return
	}

	//create inode link for mark delete inode
	inode.SetDeleteMark()
	_ = inodePut(leader.inodeTree, inode)
	_ = inodePut(follower.inodeTree, inode)
	rand.Seed(time.Now().UnixMilli())
	req = &proto.LinkInodeRequest{
		Inode:           ino,
		ClientIP:        uint32(rand.Int31n(math.MaxInt32)),
		ClientStartTime: time.Now().Unix(),
		ClientID:        uint64(rand.Int63n(math.MaxInt64)),
	}
	packet = &Packet{}
	packet.Data, _ = json.Marshal(req)
	packet.Size = uint32(len(packet.Data))
	packet.CRC = crc32.ChecksumIEEE(packet.Data[:packet.Size])
	packet.ReqID = rand.Int63n(math.MaxInt64)
	if _ = leader.CreateInodeLink(req, packet); packet.ResultCode != proto.OpNotExistErr {
		t.Errorf("create inode link for mark delete inode failed, expect result code is OpNotExistErr, "+
			"but actual result is:0x%X", packet.ResultCode)
		return
	}
	return
}

func TestMetaPartition_CreateInodeLinkCase01(t *testing.T) {
	//leader is mem mode
	dir := "create_inode_link_test_01"
	leader, follower := mockMp(t, dir, proto.StoreModeMem)
	CreateInodeLinkInterTest(t, leader, follower)
	releaseMp(leader, follower, dir)

	//leader is rocksdb mode
	leader, follower = mockMp(t, dir, proto.StoreModeRocksDb)
	CreateInodeLinkInterTest(t, leader, follower)
	releaseMp(leader, follower, dir)
}

// simulate remove file
func EvictFileInodeInterTest(t *testing.T, leader, follower *metaPartition) {
	//create inode
	ino, err := createInode(470, 0, 0, leader)
	if err != nil {
		t.Fatal(err)
		return
	}

	//unlink inode
	rand.Seed(time.Now().UnixMilli())
	reqUnlinkInode := &proto.UnlinkInodeRequest{
		Inode:           ino,
		ClientIP:        uint32(rand.Int31n(math.MaxInt32)),
		ClientStartTime: time.Now().Unix(),
		ClientID:        uint64(rand.Int63n(math.MaxInt64)),
	}
	packet := &Packet{}
	packet.Data, _ = json.Marshal(reqUnlinkInode)
	packet.Size = uint32(len(packet.Data))
	packet.CRC = crc32.ChecksumIEEE(packet.Data[:packet.Size])
	packet.ReqID = rand.Int63n(math.MaxInt64)
	if err = leader.UnlinkInode(reqUnlinkInode, packet); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("unlink inode failed, [err:%v, resultCode:%v]", err, packet.ResultCode)
		return
	}
	//evict inode
	reqEvictInode := &proto.EvictInodeRequest{
		Inode: ino,
	}
	packet = &Packet{}
	if err = leader.EvictInode(reqEvictInode, packet); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("evict inode failed, [err:%v, resultCode:%v]", err, packet.ResultCode)
		return
	}

	//validate
	var inode *DeletedINode
	inode, _ = leader.inodeDeletedTree.Get(ino)
	if inode == nil {
		t.Fatalf("get exist inode failed")
		return
	}
	if inode.NLink != 0 {
		t.Fatalf("inode nlink mismatch, expect:0, actual:%v", inode.NLink)
		return
	}

	if !inode.ShouldDelete() {
		t.Fatalf("delete flag mismatch, inode should be marked delete, but it is not")
		return
	}

	inode, _ = follower.inodeDeletedTree.Get(ino)
	if inode == nil {
		t.Errorf("get exist inode failed")
		return
	}
	if inode.NLink != 0 {
		t.Errorf("test failed, error nlink, expect:0, actual:%v", inode.NLink)
		return
	}
	if !inode.ShouldDelete() {
		t.Errorf("test failed, inode should mark delete, but it is not")
		return
	}

	//evict mark delete inode, response is ok
	packet = &Packet{}
	if err = leader.EvictInode(reqEvictInode, packet); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("evict inode failed, [err:%v, resultCode:%v]", err, packet.ResultCode)
		return
	}

	//evict not exist inode, response result code is not exist error
	reqEvictInode = &proto.EvictInodeRequest{
		Inode: math.MaxUint64 - 10,
	}
	packet = &Packet{}
	if err = leader.EvictInode(reqEvictInode, packet); packet.ResultCode != proto.OpInodeOutOfRange {
		t.Errorf("test failed, evict not exist inode, [err:%v, resultCode:0x%X]", err, packet.ResultCode)
		return
	}
	return
}

func EvictDirInodeInterTest(t *testing.T, leader, follower *metaPartition) {
	//create inode
	ino, err := createInode(uint32(os.ModeDir), 0, 0, leader)
	if err != nil {
		t.Fatal(err)
	}

	var inodeInLeaderMP, inodeInFollowerMP *Inode
	inodeInLeaderMP, _ = leader.inodeTree.Get(ino)
	inodeInFollowerMP, _ = follower.inodeTree.Get(ino)
	if !reflect.DeepEqual(inodeInLeaderMP, inodeInFollowerMP) {
		t.Errorf("inode info in mem is not equal to rocks, mem:%s, rocks:%s", inodeInLeaderMP.String(), inodeInFollowerMP.String())
		return
	}
	rand.Seed(time.Now().UnixMilli())
	req := &proto.LinkInodeRequest{
		Inode:           ino,
		ClientIP:        uint32(rand.Int31n(math.MaxInt32)),
		ClientStartTime: time.Now().Unix(),
		ClientID:        uint64(rand.Int63n(math.MaxInt64)),
	}
	packet := &Packet{}
	packet.Data, _ = json.Marshal(req)
	packet.Size = uint32(len(packet.Data))
	packet.CRC = crc32.ChecksumIEEE(packet.Data[:packet.Size])
	packet.ReqID = rand.Int63n(math.MaxInt64)
	if err = leader.CreateInodeLink(req, packet); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("create inode link failed, err:%v, resultCode:%v", err, packet.ResultCode)
		return
	}

	//evict inode
	reqEvictInode := &proto.EvictInodeRequest{
		Inode: ino,
	}
	packet = &Packet{}
	if err = leader.EvictInode(reqEvictInode, packet); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("evict inode failed, [err:%v, resultCode:%v]", err, packet.ResultCode)
		return
	}

	var inode *Inode
	inode, _ = leader.inodeTree.Get(ino)
	if inode == nil {
		t.Errorf("get exist inode failed")
		return
	}
	if inode.NLink != 3 {
		t.Errorf("test failed, error nlink, expect:0, actual:%v", inode.NLink)
		return
	}
	if inode.ShouldDelete() {
		t.Errorf("test failed, inode be set delete mark, error")
		return
	}

	inode, _ = follower.inodeTree.Get(ino)
	if inode == nil {
		t.Errorf("get exist inode failed")
		return
	}
	if inode.NLink != 3 {
		t.Errorf("test failed, error nlink, expect:0, actual:%v", inode.NLink)
		return
	}
	if inode.ShouldDelete() {
		t.Errorf("test failed, inode be set delete mark, error")
		return
	}

	//unlink to empty
	rand.Seed(time.Now().UnixMilli())
	reqUnlinkInode := &proto.UnlinkInodeRequest{
		Inode:           ino,
		ClientIP:        uint32(rand.Int31n(math.MaxInt32)),
		ClientStartTime: time.Now().Unix(),
		ClientID:        uint64(rand.Int63n(math.MaxInt64)),
	}
	packet = &Packet{}
	packet.Data, _ = json.Marshal(reqUnlinkInode)
	packet.Size = uint32(len(packet.Data))
	packet.CRC = crc32.ChecksumIEEE(packet.Data[:packet.Size])
	packet.ReqID = rand.Int63n(math.MaxInt64)
	if err = leader.UnlinkInode(reqUnlinkInode, packet); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("unlink inode failed, [err:%v, resultCode:%v]", err, packet.ResultCode)
		return
	}

	packet = &Packet{}
	if err = leader.EvictInode(reqEvictInode, packet); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("evict inode failed, [err:%v, resultCode:%v]", err, packet.ResultCode)
		return
	}

	inode, _ = leader.inodeTree.Get(ino)
	if inode != nil {
		t.Errorf("inode expect not exist in inode tree, but exist")
		return
	}
	inode, _ = follower.inodeTree.Get(ino)
	if inode != nil {
		t.Errorf("inode expect not exist in inode tree, but exist")
		return
	}

	dino, _ := leader.inodeDeletedTree.Get(ino)
	if dino == nil {
		t.Errorf("delete inode expect exist, but not")
		return
	}
	dino, _ = follower.inodeDeletedTree.Get(ino)
	if dino == nil {
		t.Errorf("delete inode expect exist, but not")
		return
	}
}

func TestMetaPartition_EvictInodeCase01(t *testing.T) {
	//leader is mem mode
	dir := "evict_inode_test_01"
	leader, follower := mockMp(t, dir, proto.StoreModeMem)
	EvictFileInodeInterTest(t, leader, follower)
	EvictDirInodeInterTest(t, leader, follower)
	releaseMp(leader, follower, dir)

	//leader is rocksdb mode
	leader, follower = mockMp(t, dir, proto.StoreModeRocksDb)
	EvictFileInodeInterTest(t, leader, follower)
	EvictDirInodeInterTest(t, leader, follower)
	releaseMp(leader, follower, dir)
}

func EvictBatchInodeInterTest(t *testing.T, leader, follower *metaPartition) {
	var (
		inos []uint64
		err  error
	)
	defer func() {
		for _, ino := range inos {
			req := &proto.DeleteInodeRequest{
				Inode: ino,
			}
			packet := &Packet{}
			leader.DeleteInode(req, packet)
		}
		if leader.inodeTree.Count() != follower.inodeTree.Count() || leader.inodeTree.Count() != 0 {
			t.Errorf("inode count must be zero after delete, but result is not expect, count[leader:%v, follower:%v]", leader.inodeTree.Count(), follower.inodeTree.Count())
			return
		}
	}()
	if inos, err = createInodesForTest(leader, follower, 100, 470, 0, 0); err != nil || len(inos) != 100 {
		t.Fatal(err)
		return
	}

	testIno := inos[20:50]
	//batch unlink
	reqBatchUnlink := &proto.BatchUnlinkInodeRequest{
		Inodes: testIno,
	}
	packet := &Packet{}
	if err = leader.UnlinkInodeBatch(reqBatchUnlink, packet); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("batch unlink inode failed, err:%v, resultCode:%v", err, packet.ResultCode)
		return
	}

	//batch evict
	reqBatchEvict := &proto.BatchEvictInodeRequest{
		Inodes: testIno,
	}
	packet = &Packet{}
	if err = leader.EvictInodeBatch(reqBatchEvict, packet); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("batch evict inode failed, err:%v, resultCode:%v", err, packet.ResultCode)
		return
	}

	//validate
	for _, ino := range testIno {
		inodeInLeader, _ := leader.inodeDeletedTree.Get(ino)
		if inodeInLeader == nil {
			t.Errorf("get inode is null")
			return
		}
		if inodeInLeader.NLink != 0 {
			t.Errorf("error nlink number, expect:0, actual:%v", inodeInLeader.NLink)
			return
		}
		if !inodeInLeader.ShouldDelete() {
			t.Errorf("inode should mark delete, but it is not")
			return
		}

		inodeInFollower, _ := follower.inodeDeletedTree.Get(ino)
		if inodeInFollower == nil {
			t.Errorf("get inode is null")
			return
		}
		if inodeInFollower.NLink != 0 {
			t.Errorf("error nlink number, expect:0, actual:%v", inodeInFollower.NLink)
			return
		}
		if !inodeInLeader.ShouldDelete() {
			t.Errorf("inode should mark delete, but it is not")
			return
		}
	}
	return
}

//todo:test
func TestMetaPartition_BatchEvictInodeCase01(t *testing.T) {
	//leader is mem mode
	dir := "batch_evict_inode_test_01"
	leader, follower := mockMp(t, dir, proto.StoreModeMem)
	EvictBatchInodeInterTest(t, leader, follower)
	releaseMp(leader, follower, dir)

	//leader is rocksdb mode
	leader, follower = mockMp(t, dir, proto.StoreModeRocksDb)
	EvictBatchInodeInterTest(t, leader, follower)
	releaseMp(leader, follower, dir)
}

func SetAttrInterTest(t *testing.T, leader, follower *metaPartition) {
	var (
		err     error
		reqData []byte
	)
	ino, err := createInode(uint32(os.ModeDir), 0, 0, leader)
	if err != nil {
		t.Fatal(err)
		return
	}

	modifyTime := time.Now().Unix()
	accessTime := time.Now().Unix()
	req := &proto.SetAttrRequest{
		Inode:      ino,
		Mode:       uint32(os.ModeDir),
		Uid:        7,
		Gid:        8,
		ModifyTime: modifyTime,
		AccessTime: accessTime,
		Valid:      31,
	}
	reqData, err = json.Marshal(req)
	if err != nil {
		t.Errorf("marshal set attr request failed, err:%v", err)
		return
	}
	packet := &Packet{}
	if err = leader.SetAttr(req, reqData, packet); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("set attr failed, err:%v, resultCode:%v", err, packet.ResultCode)
		return
	}

	var inodeInLeader, inodeInFollower *Inode
	inodeInLeader, _ = leader.inodeTree.Get(ino)
	inodeInFollower, _ = follower.inodeTree.Get(ino)
	if inodeInLeader == nil || inodeInFollower == nil {
		t.Errorf("get exist inode failed, leader:%v, follower:%v", inodeInLeader, inodeInFollower)
		return
	}
	if !reflect.DeepEqual(inodeInLeader, inodeInFollower) {
		t.Errorf("inode info in mem is not equal to rocks, mem:%s, rocks:%s", inodeInFollower.String(), inodeInFollower.String())
		return
	}
	if !proto.IsDir(inodeInLeader.Type) {
		t.Errorf("test failed, expect type is directory, but is %v", inodeInLeader.Type)
		return
	}
	if inodeInLeader.Uid != 7 {
		t.Errorf("test failed, inode uid expect type is 7, but actual is %v", inodeInLeader.Uid)
		return
	}
	if inodeInLeader.Gid != 8 {
		t.Errorf("test failed, inode gid expect type is 8, but actual is %v", inodeInLeader.Gid)
		return
	}
	if inodeInLeader.AccessTime != accessTime {
		t.Errorf("test failed, inode access time expect type is %v, but actual is %v", accessTime, inodeInLeader.AccessTime)
		return
	}
	if inodeInLeader.ModifyTime != modifyTime {
		t.Errorf("test failed, inode modify time expect type is %v, but actual is %v", modifyTime, inodeInLeader.ModifyTime)
		return
	}
	return
}

func TestMetaPartition_SetAttrCase01(t *testing.T) {
	//leader is mem mode
	dir := "set_attr_test_01"
	leader, follower := mockMp(t, dir, proto.StoreModeMem)
	SetAttrInterTest(t, leader, follower)
	releaseMp(leader, follower, dir)

	//leader is rocksdb mode
	leader, follower = mockMp(t, dir, proto.StoreModeRocksDb)
	SetAttrInterTest(t, leader, follower)
	releaseMp(leader, follower, dir)
}

func DeleteInodeInterTest(t *testing.T, leader, follower *metaPartition) {
	ino, err := createInode(uint32(os.ModeDir), 0, 0, leader)
	if err != nil {
		t.Fatal(err)
		return
	}

	req := &proto.DeleteInodeRequest{
		Inode: ino,
	}
	packet := &Packet{}
	if err = leader.DeleteInode(req, packet); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("delete inode failed, err:%v, resultCode:%v", err, packet.ResultCode)
		return
	}

	inodeInLeader, _ := leader.inodeTree.Get(ino)
	inodeInFollower, _ := follower.inodeTree.Get(ino)
	if inodeInLeader != nil || inodeInFollower != nil {
		t.Errorf("inode get result expcet is nil, but actual is [leader:%v, follower:%v]", inodeInLeader, inodeInFollower)
		return
	}
}

func TestMetaPartition_DeleteInodeCase01(t *testing.T) {
	//leader is mem mode
	dir := "delete_inode_test_01"
	leader, follower := mockMp(t, dir, proto.StoreModeMem)
	DeleteInodeInterTest(t, leader, follower)
	releaseMp(leader, follower, dir)

	//leader is rocksdb mode
	leader, follower = mockMp(t, dir, proto.StoreModeRocksDb)
	DeleteInodeInterTest(t, leader, follower)
	releaseMp(leader, follower, dir)
}

func BatchDeleteInodeInterTest(t *testing.T, leader, follower *metaPartition) {
	var (
		inos []uint64
		err  error
	)
	defer func() {
		for _, ino := range inos {
			req := &proto.DeleteInodeRequest{
				Inode: ino,
			}
			packet := &Packet{}
			leader.DeleteInode(req, packet)
		}
		if leader.inodeTree.Count() != follower.inodeTree.Count() || leader.inodeTree.Count() != 0 {
			t.Errorf("inode count must be zero after delete, but result is not expect, count[leader:%v, follower:%v]", leader.inodeTree.Count(), follower.inodeTree.Count())
			return
		}
	}()
	if inos, err = createInodesForTest(leader, follower, 100, 470, 0, 0); err != nil || len(inos) != 100 {
		t.Fatal(err)
		return
	}

	testIno := inos[20:50]
	reqBatchDeleteInode := &proto.DeleteInodeBatchRequest{
		Inodes: testIno,
	}
	packet := &Packet{}
	if err = leader.DeleteInodeBatch(reqBatchDeleteInode, packet); err != nil || packet.ResultCode != proto.OpOk {
		t.Errorf("batch delete inode failed, err:%v, resultCode:%v", err, packet.ResultCode)
		return
	}
	for _, ino := range testIno {
		inodeInLeader, _ := leader.inodeTree.Get(ino)
		inodeInFollower, _ := follower.inodeTree.Get(ino)
		if inodeInLeader != nil || inodeInFollower != nil {
			t.Errorf("inode get result expcet is nil, but actual is [leader:%v, follower:%v]", inodeInLeader, inodeInFollower)
			return
		}
	}
}

func TestMetaPartition_BatchDeleteInodeCase01(t *testing.T) {
	//leader is mem mode
	dir := "batch_delete_inode_test_01"
	leader, follower := mockMp(t, dir, proto.StoreModeMem)
	BatchDeleteInodeInterTest(t, leader, follower)
	releaseMp(leader, follower, dir)

	//leader is rocksdb mode
	leader, follower = mockMp(t, dir, proto.StoreModeRocksDb)
	BatchDeleteInodeInterTest(t, leader, follower)
	releaseMp(leader, follower, dir)
}

func TestResetCursor_OperationMismatch(t *testing.T) {
	mp, err := mockMetaPartition(1, 1, proto.StoreModeMem, "./test_cursor", ApplyMock)
	if mp == nil {
		t.Errorf("mock mp failed:%v", err)
		t.FailNow()
	}
	defer releaseMetaPartition(mp)
	mp.config.Cursor = 10000
	req := &proto.CursorResetRequest{
		PartitionId:     1,
		NewCursor:       15000,
		Force:           true,
		CursorResetType: int(SubCursor),
	}

	err = mp.CursorReset(context.Background(), req)
	if err == nil {
		t.Errorf("error mismatch, expect:operation mismatch, actual:nil")
		return
	}
	return
}

func TestResetCursor_OutOfMaxEnd(t *testing.T) {
	mp, err := mockMetaPartition(1, 1, proto.StoreModeMem, "./test_cursor", ApplyMock)
	if mp == nil {
		t.Errorf("mock mp failed:%v", err)
		t.FailNow()
	}
	defer releaseMetaPartition(mp)
	mp.config.Cursor = mp.config.End
	req := &proto.CursorResetRequest{
		PartitionId:     1,
		NewCursor:       15000,
		Force:           true,
		CursorResetType: int(SubCursor),
	}

	status, _ := mp.calcMPStatus()
	err = mp.CursorReset(context.Background(), req)
	if err != nil {
		t.Errorf("reset cursor test failed, err:%s", err.Error())
		return
	}
	t.Logf("reset cursor:%d, status:%d, err:%v", mp.config.Cursor, status, err)

	for i := 1; i < 100; i++ {
		_, _, _ = inodeCreate(mp.inodeTree, NewInode(uint64(i), 0), false)
	}
	req.NewCursor = 90
	err = mp.CursorReset(context.Background(), req)
	if err == nil {
		t.Errorf("expect error is out of bound, but actual is nil")
	}
	if mp.config.Cursor != 15000 {
		t.Errorf("cursor mismatch, expect:10000, actual:%v", mp.config.Cursor)
		return
	}
	t.Logf("reset cursor:%d, status:%d, err:%v", mp.config.Cursor, status, err)
	return
}

func TestResetCursor_LimitedAndForce(t *testing.T) {
	mp, err := mockMetaPartition(1, 1, proto.StoreModeMem, "./test_cursor", ApplyMock)
	if mp == nil {
		t.Errorf("mock mp failed:%v", err)
		t.FailNow()
	}
	defer releaseMetaPartition(mp)
	mp.config.End = 10000
	mp.config.Cursor = 9999

	req := &proto.CursorResetRequest{
		PartitionId:     1,
		NewCursor:       9900,
		Force:           false,
		CursorResetType: int(SubCursor),
	}

	for i := 1; i < 100; i++ {
		_, _, _ = inodeCreate(mp.inodeTree, NewInode(uint64(i), 0), false)
	}

	err = mp.CursorReset(context.Background(), req)
	if err == nil {
		t.Errorf("error mismatch, expect(no need reset), actual(nil)")
		return
	}
	if mp.config.Cursor != 9999 {
		t.Errorf("cursor mismatch, expect:9999, actual:%v", mp.config.Cursor)
		return
	}

	req.Force = true
	err = mp.CursorReset(context.Background(), req)
	if err != nil {
		t.Errorf("reset cursor:%d test failed, err:%v", mp.config.Cursor, err)
		return
	}
	if mp.config.Cursor != req.NewCursor {
		t.Errorf("reset cursor failed, expect:%v, actual:%v", req.NewCursor, mp.config.Cursor)
	}

	return
}

func TestResetCursor_CursorChange(t *testing.T) {
	mp, err := mockMetaPartition(1, 1, proto.StoreModeMem, "./test_cursor", ApplyMock)
	if mp == nil {
		t.Errorf("mock mp failed:%v", err)
		t.FailNow()
	}
	defer releaseMetaPartition(mp)

	req := &proto.CursorResetRequest{
		PartitionId: 1,
		NewCursor:   8000,
		Force:       false,
	}

	for i := 1; i < 100; i++ {
		_, _, _ = inodeCreate(mp.inodeTree, NewInode(uint64(i), 0), false)
	}
	mp.config.Cursor = 99

	go func() {
		for i := 0; i < 100; i++ {
			mp.nextInodeID()
			time.Sleep(time.Microsecond * 1)
		}
	}()
	time.Sleep(time.Microsecond * 5)
	err = mp.CursorReset(context.Background(), req)
	t.Logf("reset cursor:%d, err:%v", mp.config.Cursor, err)

	return
}

func TestResetCursor_LeaderChange(t *testing.T) {
	mp, err := mockMetaPartition(1, 1, proto.StoreModeMem, "./test_cursor", ApplyMock)
	if mp == nil {
		t.Errorf("mock mp failed:%v", err)
		t.FailNow()
	}
	defer releaseMetaPartition(mp)

	mp.config.Cursor = 100

	req := &proto.CursorResetRequest{
		PartitionId:     1,
		NewCursor:       8000,
		Force:           false,
		CursorResetType: int(SubCursor),
	}

	mp.config.NodeId = 2
	err = mp.CursorReset(context.Background(), req)
	if err == nil {
		t.Errorf("expect error is leader change, but actual is nil")
	}
	if 100 != mp.config.Cursor {
		t.Errorf("cursor mismatch, expect:0, actual:%v", mp.config.Cursor)
		return
	}
	t.Logf("reset cursor:%d, err:%v", mp.config.Cursor, err)

	return
}

func TestResetCursor_MPWriteStatus(t *testing.T) {
	mp, err := mockMetaPartition(1, 1, proto.StoreModeMem, "./test_cursor", ApplyMock)
	if mp == nil {
		t.Errorf("mock mp failed:%v", err)
		t.FailNow()
	}
	defer releaseMetaPartition(mp)

	configTotalMem = 100 * unit.GB
	defer func() {
		configTotalMem = 0
	}()

	for i := 1; i < 100; i++ {
		_, _, _ = inodeCreate(mp.inodeTree, NewInode(uint64(i), 0), false)
	}
	mp.config.Cursor = 10000

	req := &proto.CursorResetRequest{
		PartitionId:     1,
		NewCursor:       0,
		Force:           true,
		CursorResetType: int(SubCursor),
	}

	status, _ := mp.calcMPStatus()
	err = mp.CursorReset(context.Background(), req)
	if err == nil {
		t.Errorf("error mismatch, expect:(mp status not read only), but actual:(nil), mp status(%v)", status)
		return
	}
	if mp.config.Cursor != 10000 {
		t.Errorf("cursor mismatch, expect:99, actual:%v", mp.config.Cursor)
	}
}

func TestResetCursor_MPReadOnly(t *testing.T) {
	mp, err := mockMetaPartition(1, 1, proto.StoreModeMem, "./test_cursor", ApplyMock)
	if mp == nil {
		t.Errorf("mock mp failed:%v", err)
		t.FailNow()
	}
	defer releaseMetaPartition(mp)

	for i := 1; i < 100; i++ {
		_, _, _ = inodeCreate(mp.inodeTree, NewInode(uint64(i), 0), false)
	}

	maxInode := mp.inodeTree.MaxItem()
	if maxInode == nil {
		t.Errorf("maxInode is nil")
		return
	}
	mp.config.Cursor = mp.config.End

	req := &proto.CursorResetRequest{
		PartitionId:     1,
		NewCursor:       0,
		Force:           true,
		CursorResetType: int(SubCursor),
	}

	status, _ := mp.calcMPStatus()
	err = mp.CursorReset(context.Background(), req)
	if err != nil {
		t.Errorf("error mismatch, expect:nil, actual:%v, mp status(%v)", err, status)
		return
	}
	if mp.config.Cursor != maxInode.Inode+mpResetInoStep {
		t.Errorf("cursor mismatch, expect:99, actual:%v", mp.config.Cursor)
	}
}

func TestResetCursor_SubCursorCase01(t *testing.T) {
	mp, err := mockMetaPartition(1, 1, proto.StoreModeMem, "./test_cursor", ApplyMock)
	if mp == nil {
		t.Errorf("mock mp failed:%v", err)
		t.FailNow()
	}
	defer releaseMetaPartition(mp)

	for i := 1; i < 100; i++ {
		_, _, _ = inodeCreate(mp.inodeTree, NewInode(uint64(i), 0), false)
	}

	maxInode := mp.inodeTree.MaxItem()
	if maxInode == nil {
		t.Errorf("maxInode is nil")
		return
	}
	mp.config.Cursor = mp.config.End

	req := &proto.CursorResetRequest{
		PartitionId:     1,
		NewCursor:       maxInode.Inode + 2000,
		CursorResetType: int(SubCursor),
	}

	status, _ := mp.calcMPStatus()
	err = mp.CursorReset(context.Background(), req)
	if err != nil {
		t.Errorf("error mismatch, expect:nil, actual:%v, mp status(%v)", err, status)
		return
	}
	if mp.config.Cursor != maxInode.Inode+2000 {
		t.Errorf("cursor mismatch, expect:99, actual:%v", mp.config.Cursor)
	}
}

func TestResetCursor_AddCursorCase01(t *testing.T) {
	mp, err := mockMetaPartition(1, 1, proto.StoreModeMem, "./test_cursor", ApplyMock)
	if mp == nil {
		t.Errorf("mock mp failed:%v", err)
		t.FailNow()
	}
	defer releaseMetaPartition(mp)
	configTotalMem = 100 * GB

	req := &proto.CursorResetRequest{
		PartitionId:     1,
		CursorResetType: int(AddCursor),
	}

	err = mp.CursorReset(context.Background(), req)
	if err != nil {
		t.Errorf("reset cursor failed, err:%v", err)
		return
	}
	status, _ := mp.calcMPStatus()
	if status != proto.ReadOnly {
		t.Errorf("mp status mismatch, expect:read only(%v), actual(%v)", proto.ReadOnly, status)
	}
	if mp.config.Cursor != mp.config.End {
		t.Errorf("mp cursor mismatch, expect:%v, actual:%v", mp.config.End, mp.config.Cursor)
	}
	return
}

func TestResetCursor_ResetMaxMP(t *testing.T) {
	mp, err := mockMetaPartition(1, 1, proto.StoreModeMem, "./test_cursor", ApplyMock)
	if mp == nil {
		t.Errorf("mock mp failed:%v", err)
		t.FailNow()
	}
	defer releaseMetaPartition(mp)
	mp.config.Cursor = 1000
	mp.config.End = defaultMaxMetaPartitionInodeID
	configTotalMem = 100 * GB

	req := &proto.CursorResetRequest{
		PartitionId:     1,
		CursorResetType: int(SubCursor),
	}

	err = mp.CursorReset(context.Background(), req)
	if err == nil {
		t.Errorf("error expect:not support reset cursor, but actual:nil")
		return
	}
	if mp.config.Cursor != 1000 {
		t.Errorf("cursor mismatch, expect:%v, actual:%v", defaultMaxMetaPartitionInodeID, mp.config.Cursor)
	}
	return
}

type oldInodeInfo struct {
	Inode      uint64    `json:"ino"`
	Mode       uint32    `json:"mode"`
	Nlink      uint32    `json:"nlink"`
	Size       uint64    `json:"sz"`
	Uid        uint32    `json:"uid"`
	Gid        uint32    `json:"gid"`
	Generation uint64    `json:"gen"`
	ModifyTime time.Time `json:"mt"`
	CreateTime time.Time `json:"ct"`
	AccessTime time.Time `json:"at"`
	Target     []byte    `json:"tgt"`

	expiration int64
}

func TestProtoInodeInfoMarshaUnmarshal(t *testing.T) {
	ino := NewInode(10, 0)
	ino.Uid = 500
	ino.Gid = 501
	ino.Size = 1024
	ino.Generation = 2
	ino.CreateTime = time.Now().Unix()
	ino.ModifyTime = time.Now().Unix() + 1
	ino.AccessTime = time.Now().Unix() + 2
	ino.LinkTarget = []byte("link")
	ino.Reserved = 1024 * 1024
	ino.Extents = se.NewSortedExtents()
	var i uint64
	for i = 1; i < 5; i++ {
		var ek proto.ExtentKey
		ek.Size = uint32(1024 * i)
		ek.FileOffset = uint64(1024 * (i - 1))
		ek.ExtentOffset = i
		ek.ExtentId = i
		ek.CRC = uint32(10 * i)
		ek.PartitionId = i
		ino.Extents.Insert(nil, ek, 10)
	}
	inodeInfo := &proto.InodeInfo{}
	replyInfo(inodeInfo, ino)
	data, err := json.Marshal(inodeInfo)
	if err != nil {
		t.Errorf("marshal failed: %v", err)
		return
	}

	inodeInfoUnmarshal := &oldInodeInfo{}
	if err = json.Unmarshal(data, inodeInfoUnmarshal); err != nil {
		t.Errorf("unmarshal failed: %v", err)
		return
	}
	assert.Equal(t, inodeInfo.Inode, inodeInfoUnmarshal.Inode)
	assert.Equal(t, inodeInfo.Mode, inodeInfoUnmarshal.Mode)
	assert.Equal(t, inodeInfo.Size, inodeInfoUnmarshal.Size)
	assert.Equal(t, inodeInfo.Uid, inodeInfoUnmarshal.Uid)
	assert.Equal(t, inodeInfo.Gid, inodeInfoUnmarshal.Gid)
	assert.Equal(t, inodeInfo.Generation, inodeInfoUnmarshal.Generation)
	assert.Equal(t, *inodeInfo.Target, inodeInfoUnmarshal.Target)
	assert.Equal(t, inodeInfo.Nlink, inodeInfoUnmarshal.Nlink)
	assert.Equal(t, int64(inodeInfo.ModifyTime), inodeInfoUnmarshal.ModifyTime.Unix())
	assert.Equal(t, int64(inodeInfo.CreateTime), inodeInfoUnmarshal.CreateTime.Unix())
	assert.Equal(t, int64(inodeInfo.AccessTime), inodeInfoUnmarshal.AccessTime.Unix())
}
func innerTestCalcInodeAndDelInodeSize(t *testing.T, storeMode proto.StoreMode) {
	mp, err := mockMetaPartition(1, 1, storeMode, "./test_inode_size", ApplyMock)
	if mp == nil {
		t.Errorf("mock mp failed:%v", err)
		t.FailNow()
	}
	mp.config.TrashRemainingDays = 1
	mp.config.Cursor = mp.config.Start

	//create inode
	inodeCnt := uint64(100)
	inodeIDs := make([]uint64, 0, inodeCnt)
	for index := uint64(0); index < inodeCnt; index++ {
		var ino uint64
		ino, err = createInode(0, 0, 0, mp)
		if err != nil {
			t.Error(err)
			t.FailNow()
		}
		inodeIDs = append(inodeIDs, ino)
	}
	assert.Equal(t, uint64(0), mp.inodeTree.GetInodesTotalSize())
	assert.Equal(t, inodeCnt, uint64(len(inodeIDs)))

	t.Logf("create %v inode success", inodeCnt)

	//inset ek
	expectInodesTotalSize := uint64(0)
	for _, inodeID := range inodeIDs {
		expectInodesTotalSize += 420
		eks := []proto.ExtentKey{
			{FileOffset: 100, PartitionId: 2, ExtentId: 1, ExtentOffset: 0, Size: 100},
			{FileOffset: 300, PartitionId: 4, ExtentId: 1, ExtentOffset: 0, Size: 20},
			{FileOffset: 0, PartitionId: 1, ExtentId: 1, ExtentOffset: 0, Size: 100},
			{FileOffset: 200, PartitionId: 3, ExtentId: 1, ExtentOffset: 0, Size: 60},
			{FileOffset: 400, PartitionId: 4, ExtentId: 1, ExtentOffset: 0, Size: 20},
		}

		for _, ek := range eks {
			if err = extentInsert(inodeID, ek, mp); err != nil {
				t.Error(err)
				t.FailNow()
			}
		}
	}
	assert.Equal(t, expectInodesTotalSize, mp.inodeTree.GetInodesTotalSize())

	t.Logf("insert ek success")

	//ek truncate
	truncateInodeCnt := 0
	for _, inodeID := range inodeIDs {
		if inodeID%5 == 1 {
			truncateInodeCnt++
			oldSize := uint64(420)
			expectInodesTotalSize -= oldSize
			newSize := (inodeID * 10) % 3
			expectInodesTotalSize += newSize
			if err = extentTruncate(inodeID, oldSize, newSize, mp); err != nil {
				t.Error(err)
				t.FailNow()
			}
		}
	}
	assert.Equal(t, expectInodesTotalSize, mp.inodeTree.GetInodesTotalSize())

	t.Logf("truncate inode count: %v", truncateInodeCnt)

	//del
	delInodesID := make([]uint64, 0)
	expectDelInodeTotalSize := uint64(0)
	for _, inodeID := range inodeIDs {
		if inodeID%3 == 1 {
			delInodesID = append(delInodesID, inodeID)
			var retMsg *InodeResponse
			retMsg, err = mp.getInode(inodeID, false)
			if err != nil {
				t.Error(err)
				t.FailNow()
			}

			if retMsg.Status != proto.OpOk {
				t.Errorf("get inode %v failed", inodeID)
				t.FailNow()
			}

			inodeSize := retMsg.Msg.Size
			if err = unlinkInode(inodeID, mp, true); err != nil {
				t.Error(err)
				t.FailNow()
			}

			if err = evictInode(inodeID, mp, true); err != nil {
				t.Error(err)
				t.FailNow()
			}

			expectDelInodeTotalSize += inodeSize
			expectInodesTotalSize -= inodeSize
		}
	}
	assert.Equal(t, expectInodesTotalSize, mp.inodeTree.GetInodesTotalSize())
	assert.Equal(t, expectDelInodeTotalSize, mp.inodeDeletedTree.GetDelInodesTotalSize())

	t.Logf("del inode count: %v", len(delInodesID))

	//recover
	newDelInodeIDs := make([]uint64, 0)
	for _, delInodeID := range delInodesID {
		if delInodeID%2 == 0 {
			_, dino, status, _ := mp.getDeletedInode(delInodeID)
			if status != proto.OpOk {
				t.Error(status)
				t.FailNow()
			}
			if err = recoverDelInode(delInodeID, mp); err != nil {
				t.Error(err)
				t.FailNow()
			}
			expectInodesTotalSize += dino.Size
			expectDelInodeTotalSize -= dino.Size
		} else {
			newDelInodeIDs = append(newDelInodeIDs, delInodeID)
		}
	}
	assert.Equal(t, expectInodesTotalSize, mp.inodeTree.GetInodesTotalSize())
	assert.Equal(t, expectDelInodeTotalSize, mp.inodeDeletedTree.GetDelInodesTotalSize())

	t.Logf("recover inode success count: %v", len(delInodesID)-len(newDelInodeIDs))

	//clean del inode
	needCleanDelInodes := make([]*Inode, 0)
	for _, delInodeID := range newDelInodeIDs {
		if delInodeID%8 == 1 {
			needCleanDelInodes = append(needCleanDelInodes, NewInode(delInodeID, 0))
			_, dino, status, _ := mp.getDeletedInode(delInodeID)
			if status != proto.OpOk {
				t.Error(status)
				t.FailNow()
			}
			expectDelInodeTotalSize -= dino.Size
		}
	}

	bufSlice := make([]byte, 0, 8*len(needCleanDelInodes))
	for _, inode := range needCleanDelInodes {
		bufSlice = append(bufSlice, inode.MarshalKey()...)
	}
	if _, err = mp.submit(context.Background(), opFSMInternalCleanDeletedInode, "", bufSlice, nil); err != nil {
		t.Error(err)
		t.FailNow()
	}
	assert.Equal(t, expectInodesTotalSize, mp.inodeTree.GetInodesTotalSize())
	assert.Equal(t, expectDelInodeTotalSize, mp.inodeDeletedTree.GetDelInodesTotalSize())
	t.Logf("clean del inode success count: %v", len(needCleanDelInodes))

	err = mp.store(&storeMsg{
		command:    opFSMStoreTick,
		applyIndex: 10000,
		snap:       NewSnapshot(mp),
		reqTree:    mp.reqRecords.ReqBTreeSnap(),
	})
	if err != nil {
		t.Error(err)
		t.FailNow()
	}

	if mp.HasRocksDBStore() {
		err = mp.inodeTree.PersistBaseInfo()
		if err != nil {
			t.Error(err)
			t.FailNow()
		}
	}

	//stop partition
	close(mp.stopC)
	time.Sleep(time.Second)
	_ = mp.db.CloseDb()

	//reload partition
	mp, err = mockMetaPartitionReload(1, 1, storeMode, "./test_inode_size", ApplyMock)
	if err != nil {
		t.Errorf("mock mp reload failed:%v", err)
		t.FailNow()
	}

	assert.Equal(t, expectInodesTotalSize, mp.inodeTree.GetInodesTotalSize())
	assert.Equal(t, expectDelInodeTotalSize, mp.inodeDeletedTree.GetDelInodesTotalSize())
	releaseMetaPartition(mp)
}

func TestMetaPartition_CalcInodeAndDelInodeSize(t *testing.T) {
	innerTestCalcInodeAndDelInodeSize(t, proto.StoreModeMem)
	t.Logf("test mem mode finished")
	innerTestCalcInodeAndDelInodeSize(t, proto.StoreModeRocksDb)
	t.Logf("test rocksdb mode finished")
}
