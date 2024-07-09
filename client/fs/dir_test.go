package fs

import (
	"context"
	"os"
	"strconv"
	"testing"

	"bazil.org/fuse"
	"github.com/cubefs/cubefs/proto"
)

type createInfo struct {
	name string
	ino  uint64
	mode uint32
}

func Test_ReaddirPlus(t *testing.T) {
	opt := &proto.MountOptions{Modulename: "fuseclient", Volname: ltptestVolume, Owner: ltptestOwner, Master: ltptestMasterStr}
	s, err := NewSuper(opt, true, nil, nil, nil)
	if err != nil {
		t.Fatalf("Test_ReaddirPlus: new super err(%v)", err)
		return
	}
	Sup = s
	d := &Node{}
	ctx := context.Background()
	// create test dir
	var dInfo *proto.InodeInfo
	if dInfo, err = s.mw.Create_ll(ctx, 1, "Test_ReaddirPlus", uint32(os.ModeDir), 0, 0, nil); err != nil {
		t.Fatalf("Test_ReaddirPlus: create dir err(%v)", err)
		return
	}
	d.inode = dInfo.Inode
	// create file/dir under the folder
	createInfos := make([]*createInfo, 0)
	for i := int64(0); i < 3; i++ {
		info := &createInfo{name: strconv.FormatInt(i, 10), mode: 0644}
		if i == 3 {
			info.mode = uint32(os.ModeDir)
		}
		var inoInfo *proto.InodeInfo
		if inoInfo, err = s.mw.Create_ll(ctx, d.inode, info.name, info.mode, 0, 0, nil); err != nil {
			t.Fatalf("Test_ReaddirPlus: create inode err(%v)", err)
			return
		}
		info.ino = inoInfo.Inode
		createInfos = append(createInfos, info)
	}
	// exec and check result of ReadDirPlusAll
	resp := &fuse.ReadDirPlusResponse{}
	res, err := d.ReadDirPlusAll(ctx, resp)
	if err != nil {
		t.Fatalf("Test_ReaddirPlus: readdirplus err(%v)", err)
		return
	}
	if len(res) != len(createInfos) {
		t.Fatalf("Test_ReaddirPlus: readdirplus result count expect(%v) but(%v)", len(createInfos), len(res))
		return
	}
	for i, dirent := range res {
		info := createInfos[i]
		if dirent.Dirent.Name != info.name || dirent.Dirent.Inode != info.ino || dirent.Dirent.Type != ParseType(info.mode) || dirent.Node == nil {
			t.Errorf("Test_ReaddirPlus: inconsistent dirent(%v) info(%v) or Node(%v) is nil", dirent.Dirent, info, dirent.Node)
		}
	}
}
