package proto

import "time"

const (
	FileID = iota
	Size
	Crc
	ModifyTime
)

type DataPartitions struct {
	Partitions       []*DataPartition `json:"partitions"`
	PartitionCount   int              `json:"partitionCount"`
	RiskCount        int              `json:"riskCount"`
	RiskFixerRunning bool             `json:"riskFixerRunning"`
}
type DataPartition struct {
	ID       uint64   `json:"id"`
	Size     int      `json:"size"`
	Used     int      `json:"used"`
	Status   int      `json:"status"`
	Path     string   `json:"path"`
	Replicas []string `json:"replicas"`
}

type DNDataPartitionRiskFixerStatus struct {
	Fragments []struct {
		ExtentID uint64
		Offset   uint64
		Size     uint64
	}
	Count   int
	Running bool
}

type DNDataPartitionInfo struct {
	VolName              string            `json:"volName"`
	ID                   uint64            `json:"id"`
	Size                 int               `json:"size"`
	Used                 int               `json:"used"`
	Status               int               `json:"status"`
	Path                 string            `json:"path"`
	Files                []ExtentInfoBlock `json:"extents"`
	FileCount            int               `json:"fileCount"`
	Replicas             []string          `json:"replicas"`
	TinyDeleteRecordSize int64             `json:"tinyDeleteRecordSize"`
	RaftStatus           *Status           `json:"raftStatus"`
	Peers                []Peer            `json:"peers"`
	Learners             []Learner         `json:"learners"`
	IsFinishLoad         bool              `json:"isFinishLoad"`
	IsRecover            bool              `json:"isRecover"`
	BaseExtentID         uint64            `json:"baseExtentID"`

	RiskFixerStatus DNDataPartitionRiskFixerStatus `json:"riskFixerStatus"`
}

type BlockCrc struct {
	BlockNo int
	Crc     uint32
}

type ExtentInfoBlock [4]uint64

const (
	ExtentInfoFileID = iota
	ExtentInfoSize
	ExtentInfoCrc
	ExtentInfoModifyTime
)

type DNDataPartitionInfoDbbak struct {
	VolName   string                 `json:"volname"`
	ID        uint64                 `json:"id"`
	Size      int                    `json:"size"`
	Used      int                    `json:"used"`
	Status    int                    `json:"status"`
	Path      string                 `json:"path"`
	Files     []ExtentFileInfoDbBack `json:"files"`
	FileCount int                    `json:"fileCount"`
	Replicas  []string               `json:"replicas"`
}
type DNDataPartitionInfoOldVersion struct {
	VolName              string       `json:"volName"`
	ID                   uint64       `json:"id"`
	Size                 int          `json:"size"`
	Used                 int          `json:"used"`
	Status               int          `json:"status"`
	Path                 string       `json:"path"`
	Files                []ExtentInfo `json:"extents"`
	FileCount            int          `json:"fileCount"`
	Replicas             []string     `json:"replicas"`
	TinyDeleteRecordSize int64        `json:"tinyDeleteRecordSize"`
	RaftStatus           *Status      `json:"raftStatus"`
	Peers                []Peer       `json:"peers"`
	Learners             []Learner    `json:"learners"`
}

type TinyExtentHole struct {
	Offset     uint64
	Size       uint64
	PreAllSize uint64
}

type DNTinyExtentInfo struct {
	Holes           []*TinyExtentHole `json:"holes"`
	ExtentAvaliSize uint64            `json:"extentAvaliSize"`
	BlocksNum       uint64            `json:"blockNum"`
}

type DataNodeDiskReport struct {
	Disks []*DataNodeDiskInfo `json:"disks"`
	Zone  string              `json:"zone"`
}

type DataNodeDiskInfo struct {
	Path        string `json:"path"`
	Total       uint64 `json:"total"`
	Used        uint64 `json:"used"`
	Available   uint64 `json:"available"`
	Unallocated uint64 `json:"unallocated"`
	Allocated   uint64 `json:"allocated"`
	Status      int    `json:"status"`
	RestSize    uint64 `json:"restSize"`
	Partitions  int    `json:"partitions"`

	// Limits
	RepairTaskLimit              uint64 `json:"repair_task_limit"`
	ExecutingRepairTask          uint64 `json:"executing_repair_task"`
	FixTinyDeleteRecordLimit     uint64 `json:"fix_tiny_delete_record_limit"`
	ExecutingFixTinyDeleteRecord uint64 `json:"executing_fix_tiny_delete_record"`
}

type DataNodeStats struct {
	Total               uint64
	Used                uint64
	Available           uint64
	TotalPartitionSize  uint64
	RemainingCapacity   uint64
	CreatedPartitionCnt uint64
	MaxCapacity         uint64
	HttpPort            string
	ZoneName            string
	PartitionReports    []*PartitionReport
	PartitionInfo       []*PartitionReport // for dbBack
	Status              int
	Result              string
	BadDisks            []string
	DiskInfos           map[string]*DiskInfo
	Version             string
}

type StatInfo struct {
	Type             string       `json:"type"`
	Zone             string       `json:"zone"`
	VersionInfo      VersionValue `json:"versionInfo"`
	StartTime        string       `json:"startTime"`
	CPUUsageList     []float64    `json:"cpuUsageList"`
	MaxCPUUsage      float64      `json:"maxCPUUsage"`
	CPUCoreNumber    int          `json:"cpuCoreNumber"`
	MemoryUsedGBList []float64    `json:"memoryUsedGBList"`
	MaxMemoryUsedGB  float64      `json:"maxMemoryUsedGB"`
	MaxMemoryUsage   float64      `json:"maxMemoryUsage"`
	DiskInfo         []struct {
		Path            string  `json:"path"`
		TotalTB         float64 `json:"totalTB"`
		UsedGB          float64 `json:"usedGB"`
		UsedRatio       float64 `json:"usedRatio"`
		ReservedSpaceGB uint    `json:"reservedSpaceGB"`
	} `json:"diskInfo"`
}

type ReplProtocalBufferDetail struct {
	Addr     string
	Cnt      int64
	UseBytes int64
	ReplID   int64
}

type ExtentCrc struct {
	ExtentId uint64
	CRC      uint32
}

// HardState is the repl state,must persist to the storage.
type HardState struct {
	Term   uint64
	Commit uint64
	Vote   uint64
}

type LockStatus uint8

const (
	Lock LockStatus = iota
	Unlock
)

type ExtentLockInfo struct {
	ExtentKeys []ExtentKey
	LockStatus LockStatus
	LockTime   int64
}

type ExtentIdLockInfo struct {
	ExtentId   uint64
	ExpireTime int64
	TTL        int64
}

const (
	DataNodeDiskReservedMinRatio = 0.01
	DataNodeDiskReservedMaxRatio = 0.1
)

type FileInfo struct {
	FileId  uint64    `json:"fileId"`
	Inode   uint64    `json:"ino"`
	Size    uint64    `json:"size"`
	Crc     uint32    `json:"crc"`
	Deleted bool      `json:"deleted"`
	ModTime time.Time `json:"modTime"`
	Source  string    `json:"src"`
}

type DataPartitionViewDbBack struct {
	VolName   string      `json:"volname"`
	ID        uint32      `json:"id"`
	Files     []*FileInfo `json:"files"`
	FileCount int         `json:"fileCount"`
}

type DbbackPartitionReport struct {
	PartitionID          uint64
	PartitionStatus      int
	Total                uint64
	Used                 uint64
	DiskPath             string
	ExtentCount          int
	NeedCompare          bool
	AvaliTinyExtentCnt   int
	UnavaliTinyExtentCnt int
	VolName              string
}

type DbbackDataNodeHeartBeatResponse struct {
	Total                           uint64
	Used                            uint64
	Available                       uint64
	CreatedPartitionWeights         uint64 //volCnt*volsize
	RemainWeightsForCreatePartition uint64 //all-usedvolsWieghts
	CreatedPartitionCnt             uint32
	MaxWeightsForCreatePartition    uint64
	RackName                        string
	PartitionInfo                   []*DbbackPartitionReport
	Status                          uint8
	Result                          string
}
