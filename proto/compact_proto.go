package proto

type DataMigVolume struct {
	Name       string
	VolId      uint64
	Owner      string
	Status     uint8
	TotalSize  uint64
	UsedSize   uint64
	CreateTime int64
	ForceROW   bool
	CompactTag CompactTag
}

func (cv DataMigVolume) String() string {
	return cv.Name
}

type DataMigVolumeView struct {
	Cluster        string
	DataMigVolumes []*DataMigVolume
}

func NewDataMigVolumeView(cluster string, compactVolumes []*DataMigVolume) *DataMigVolumeView {
	return &DataMigVolumeView{
		Cluster:        cluster,
		DataMigVolumes: compactVolumes,
	}
}

type VolumeDataMigView struct {
	ClusterName   string   `json:"clusterName"`
	Name          string   `json:"volName"`
	State         uint32   `json:"state"`
	LastUpdate    int64    `json:"lastUpdate"`
	RunningMpCnt  int      `json:"runningMpCnt"`
	RunningMpIds  []uint64 `json:"runningMpIds"`
	RunningInodeCnt int    `json:"runningInodeCnt"`
	RunningInodes []uint64 `json:"runningInodes"`
}

type ClusterDataMigView struct {
	ClusterName string               `json:"clusterName"`
	Nodes       []string             `json:"nodes"`
	VolumeInfo  []*VolumeDataMigView `json:"volumeInfo"`
}

type DataMigWorkerViewInfo struct {
	Port     string                `json:"port"`
	Clusters []*ClusterDataMigView `json:"cluster"`
}

type QueryHTTPResult struct {
	Code int32  `json:"code"`
	Msg  string `json:"msg"`
}

type MergeEkType uint8

const (
	CompactMergeEk MergeEkType = iota
	FileMigMergeEk
	EcFileMigMergeEk
)
