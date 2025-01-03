package proto

import (
	"fmt"
	"strconv"

	"github.com/cubefs/cubefs/util/auth"
	"github.com/cubefs/cubefs/util/config"
	"github.com/cubefs/cubefs/util/exporter"
)

// For client
const (
	// Mandatory
	MountPoint int = iota
	VolName
	Owner
	Master
	// Optional
	Modulename
	LogDir
	WarnLogDir
	LogLevel
	ProfPort
	ExporterPort
	IcacheTimeout
	LookupValid
	AttrValid
	ReadRate
	WriteRate
	EnSyncWrite
	AutoInvalData
	Rdonly
	WriteCache
	KeepCache
	FollowerRead
	Authenticate
	ClientKey
	TicketHost
	EnableHTTPS
	CertFile
	TokenKey
	AccessKey
	SecretKey
	DisableDcache
	SubDir
	FsyncOnClose
	MaxCPUs
	EnableXattr
	NearRead
	// AlignSize
	// MaxExtentNumPerAlignArea
	// ForceAlignMerge
	EnablePosixACL
	AutoMakeSubDir
	ExtentSize
	AutoFlush
	DeleteProcessAbsoPath
	NoBatchGetInodeOnReaddir
	ReadAheadSize
	UmpCollectWay
	PidFile
	EnableReadDirPlus
	PrefetchThread
	StreamerSegCount
	MaxBackground
	CongestionThresh
	Profile
	UpdateExtentsOnRead
	NotCacheNode
	UseLastExtent
	SeqWriteCluster
	ReadAheadMemMB
	ReadAheadWindowMB
	EnableMonitor

	MaxMountOption
)

// For server
const (
	MasterAddr        = "masterAddr"
	ListenPort        = "listen"
	HttpPort          = "prof"
	ObjectNodeDomain  = "objectNodeDomain"
	MaxReadAhead      = 512 * 1024
	ProfileAiPrefetch = "ai_prefetch"
	Profile9N         = "9N"
	MasterLBDomain    = "masterLBDomain"
	MasterDomain      = "masterDomain"
	MasterDefaultPort = "8868"
)

type MountOption struct {
	keyword      string
	description  string
	cmdlineValue string
	value        interface{}
	hasConfig    bool // is this option in config file
}

func NewMountOption(keyword string, description string, value interface{}) MountOption {
	return MountOption{keyword, description, "", value, false}
}

func (opt MountOption) String() string {
	return fmt.Sprintf("[%v] %T: %v", opt.keyword, opt.value, opt.value)
}

func NewMountOptions() []MountOption {
	opts := make([]MountOption, MaxMountOption)
	return opts
}

func InitMountOptions(opts []MountOption) {
	opts[MountPoint] = NewMountOption("mountPoint", "Mount Point", "")
	opts[Modulename] = NewMountOption("modulename", "module name", "")
	opts[VolName] = NewMountOption("volName", "Volume Name", "")
	opts[Owner] = NewMountOption("owner", "Owner", "")
	opts[Master] = NewMountOption(MasterAddr, "Master Address", "")
	opts[LogDir] = NewMountOption("logDir", "Log Path", "")
	opts[WarnLogDir] = NewMountOption("warnLogDir", "Warn Log Path", "")
	opts[LogLevel] = NewMountOption("logLevel", "Log Level", "")
	opts[ProfPort] = NewMountOption("profPort", "PProf Port", "")
	opts[ExporterPort] = NewMountOption("exporterPort", "Exporter Port", "")
	opts[IcacheTimeout] = NewMountOption("icacheTimeout", "Inode Cache Expiration Time", int64(-1))
	opts[LookupValid] = NewMountOption("lookupValid", "Lookup Valid Duration", int64(-1))
	opts[AttrValid] = NewMountOption("attrValid", "Attr Valid Duration", int64(-1))
	opts[ReadRate] = NewMountOption("readRate", "Read Rate Limit", int64(-1))
	opts[WriteRate] = NewMountOption("writeRate", "Write Rate Limit", int64(-1))
	opts[EnSyncWrite] = NewMountOption("enSyncWrite", "Enable Sync Write", int64(-1))
	opts[AutoInvalData] = NewMountOption("autoInvalData", "Auto Invalidate Data", int64(-1))
	opts[Rdonly] = NewMountOption("rdonly", "Mount as readonly", false)
	opts[WriteCache] = NewMountOption("writecache", "Enable FUSE writecache feature", false)
	opts[KeepCache] = NewMountOption("keepcache", "Enable FUSE keepcache feature", false)
	opts[FollowerRead] = NewMountOption("followerRead", "Enable read from follower", false)
	opts[NearRead] = NewMountOption("nearRead", "Enable read from nearest node", false)
	opts[ReadAheadSize] = NewMountOption("readAheadSize", "Set the size of kernel read-ahead", int64(MaxReadAhead))
	opts[MaxBackground] = NewMountOption("maxBackground", "Set the count of kernel background requests", int64(0))
	opts[CongestionThresh] = NewMountOption("congestionThresh", "Set the congestion threshold of kernel background requests", int64(0))

	opts[Authenticate] = NewMountOption("authenticate", "Enable Authenticate", false)
	opts[ClientKey] = NewMountOption("clientKey", "Client Key", "")
	opts[TicketHost] = NewMountOption("ticketHost", "Ticket Host", "")
	opts[EnableHTTPS] = NewMountOption("enableHTTPS", "Enable HTTPS", false)
	opts[CertFile] = NewMountOption("certFile", "Cert File", "")

	opts[TokenKey] = NewMountOption("token", "Token Key", "")
	opts[AccessKey] = NewMountOption("accessKey", "Access Key", "")
	opts[SecretKey] = NewMountOption("secretKey", "Secret Key", "")

	opts[DisableDcache] = NewMountOption("disableDcache", "Disable Dentry Cache", false)
	opts[SubDir] = NewMountOption("subdir", "Mount sub directory", "")
	opts[AutoMakeSubDir] = NewMountOption("autoMakeSubdir", "Auto make non-existent subdir", false)
	opts[FsyncOnClose] = NewMountOption("fsyncOnClose", "Perform fsync upon file close", true)
	opts[MaxCPUs] = NewMountOption("maxcpus", "The maximum number of CPUs that can be executing", int64(-1))
	opts[EnableXattr] = NewMountOption("enableXattr", "Enable xattr support", false)
	opts[EnablePosixACL] = NewMountOption("enablePosixACL", "enable posix ACL support", false)
	opts[NoBatchGetInodeOnReaddir] = NewMountOption("noBatchGetInodeOnReaddir", "Not batch get inode info when readdir", false)
	opts[ExtentSize] = NewMountOption("extentSize", "set extentSize for client", int64(0))
	opts[AutoFlush] = NewMountOption("autoFlush", "set autoFlush for client", true)
	opts[DeleteProcessAbsoPath] = NewMountOption("delProcessAbsoPath", "the absolute path of the process which is allowed to delete files", "")
	opts[UmpCollectWay] = NewMountOption("umpCollectWay", "1: by file, 2: by jmtp client", int64(exporter.UMPCollectMethodFile))
	opts[PidFile] = NewMountOption("pidFile", "pidFile absolute path", "")
	opts[EnableReadDirPlus] = NewMountOption("readDirPlus", "readdir and get inode info to accelerate any future lookups in the same directory", false)
	opts[PrefetchThread] = NewMountOption("prefetchThread", "start multiple threads to prefetch files", int64(0))
	opts[StreamerSegCount] = NewMountOption("streamerSegCount", "The number of streamer segment map", int64(0))
	opts[Profile] = NewMountOption("profile", "config group for different situations", "")
	opts[UpdateExtentsOnRead] = NewMountOption("updateExtentsOnRead", "update extents cache when requested offset exceeds extents length", true)
	opts[NotCacheNode] = NewMountOption("notCacheNode", "not cache node in libfuse", false)
	opts[UseLastExtent] = NewMountOption("useLastExtent", "use last extent when writing", true)
	opts[SeqWriteCluster] = NewMountOption("seqWriteCluster", "is sequential write cluster", false)
	opts[ReadAheadMemMB] = NewMountOption("readAheadMemMB", "total memory usage size(MB) of read ahead", int64(0))
	opts[ReadAheadWindowMB] = NewMountOption("readAheadWindowMB", "window size(MB) of read ahead", int64(0))
	opts[EnableMonitor] = NewMountOption("enableMonitor", "client i/o operation report to monitor", false)
}

func ParseMountOptions(opts []MountOption, cfg *config.Config) {
	for i := 0; i < MaxMountOption; i++ {
		switch v := opts[i].value.(type) {
		case string:
			if opts[i].cmdlineValue != "" {
				opts[i].value = opts[i].cmdlineValue
			} else {
				if value, present := cfg.CheckAndGetString(opts[i].keyword); present {
					opts[i].value = value
					opts[i].hasConfig = true
				} else {
					opts[i].value = v
				}
			}
			fmt.Println(fmt.Sprintf("keyword[%v] value[%v] type[%T]", opts[i].keyword, opts[i].value, v))

		case int64:
			if opts[i].cmdlineValue != "" {
				opts[i].value = parseInt64(opts[i].cmdlineValue)
			} else {
				if value, present := cfg.CheckAndGetInt64(opts[i].keyword); present {
					opts[i].value = value
					opts[i].hasConfig = true
				} else {
					opts[i].value = v
				}
			}
			fmt.Println(fmt.Sprintf("keyword[%v] value[%v] type[%T]", opts[i].keyword, opts[i].value, v))

		case bool:
			if opts[i].cmdlineValue != "" {
				opts[i].value = parseBool(opts[i].cmdlineValue)
			} else {
				if value, present := cfg.CheckAndGetBool(opts[i].keyword); present {
					opts[i].value = value
					opts[i].hasConfig = true
				} else {
					opts[i].value = v
				}
			}
			fmt.Println(fmt.Sprintf("keyword[%v] value[%v] type[%T]", opts[i].keyword, opts[i].value, v))

		default:
			fmt.Println(fmt.Sprintf("keyword[%v] unknown type[%T]", opts[i].keyword, v))
		}
	}
}

func parseInt64(s string) int64 {
	var ret int64 = -1

	if s != "" {
		val, err := strconv.Atoi(s)
		if err == nil {
			ret = int64(val)
		}
	}
	return ret
}

func parseBool(s string) bool {
	var ret = false

	if s == "true" {
		ret = true
	}
	return ret
}

func (opt *MountOption) GetString() string {
	val, ok := opt.value.(string)
	if !ok {
		return ""
	}
	return val
}

func (opt *MountOption) GetBool() bool {
	val, ok := opt.value.(bool)
	if !ok {
		return false
	}
	return val
}

func (opt *MountOption) GetInt64() int64 {
	val, ok := opt.value.(int64)
	if !ok {
		return int64(-1)
	}
	return val
}

func (opt *MountOption) HasConfig() bool {
	return opt.hasConfig
}

type MountOptions struct {
	Config                   *config.Config
	MountPoint               string
	Modulename               string
	Volname                  string
	Owner                    string
	Master                   string
	Logpath                  string
	Loglvl                   string
	Profport                 string
	IcacheTimeout            int64
	LookupValid              int64
	AttrValid                int64
	ReadRate                 int64
	WriteRate                int64
	EnSyncWrite              int64
	AutoInvalData            int64
	UmpDatadir               string
	Rdonly                   bool
	WriteCache               bool
	KeepCache                bool
	FollowerRead             bool
	Authenticate             bool
	TicketMess               auth.TicketMess
	TokenKey                 string
	AccessKey                string
	SecretKey                string
	DisableDcache            bool
	SubDir                   string
	AutoMakeSubDir           bool
	FsyncOnClose             bool
	MaxCPUs                  int64
	EnableXattr              bool
	NearRead                 bool
	EnablePosixACL           bool
	ExtentSize               int64
	AutoFlush                bool
	DelProcessPath           string
	NoBatchGetInodeOnReaddir bool
	ReadAheadSize            int64
	UmpCollectWay            int64
	PidFile                  string
	EnableReadDirPlus        bool
	PrefetchThread           int64
	StreamerSegCount         int64
	MaxBackground            int64
	CongestionThresh         int64
	Profile                  string
	UpdateExtentsOnRead      bool
	NotCacheNode             bool
	UseLastExtent            bool
	SeqWriteCluster          bool
	ReadAheadMemMB           int64
	ReadAheadWindowMB        int64
	EnableMonitor            bool
}
