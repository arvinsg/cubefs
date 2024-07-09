package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"plugin"
	"runtime"
	"time"
	_ "unsafe"

	"github.com/jacobsa/daemonize"
)

var (
	configVersion = flag.Bool("v", false, "Show client version")

	startClient func(string, *os.File, []byte) error
	stopClient  func() []byte
	getFuseFd   func() *os.File
	getVersion  func() string
)

const (
	CheckUpdateInterval = 5 * time.Second
)

func loadSym(handle *plugin.Plugin) {
	sym, _ := handle.Lookup("StartClient")
	startClient = sym.(func(string, *os.File, []byte) error)

	sym, _ = handle.Lookup("StopClient")
	stopClient = sym.(func() []byte)

	sym, _ = handle.Lookup("GetFuseFd")
	getFuseFd = sym.(func() *os.File)

	sym, _ = handle.Lookup("GetVersion")
	getVersion = sym.(func() string)
}

//go:linkname initSig runtime.libpreinit
func initSig()

func init() {
	initSig()
}

func main() {
	flag.Parse()

	if !*configVersion && *configFile == "" {
		fmt.Printf("Usage: %s -c {configFile}\n", os.Args[0])
		os.Exit(1)
	}
	var (
		err          error
		masterAddr   string
		downloadAddr string
		tarName      string
	)

	if *configUseVersion != "" {
		if *configFile == "" {
			fmt.Printf("Must given -c {configFile}\n")
			os.Exit(1)
		}

		masterAddr, err = parseMasterAddr(*configFile)
		if err != nil {
			fmt.Printf("parseMasterAddr err: %v\n", err)
			os.Exit(1)
		}
		downloadAddr, err = getClientDownloadAddr(masterAddr)
		if err != nil {
			fmt.Printf("get downloadAddr from master err: %v\n", err)
			os.Exit(1)
		}

		if runtime.GOARCH == AMD64 {
			tarName = fmt.Sprintf("%s_%s.tar.gz", VersionTarPre, *configUseVersion)
		} else if runtime.GOARCH == ARM64 {
			tarName = fmt.Sprintf("%s_%s_%s.tar.gz", VersionTarPre, ARM64, *configUseVersion)
		}
		if !prepareLibs(downloadAddr, tarName) {
			os.Exit(1)
		}
	}

	if *configVersion {
		*configForeground = true
	}
	if !*configForeground {
		if err := startDaemon(); err != nil {
			fmt.Printf("%s\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	handle, err := plugin.Open(ClientLib)
	if err != nil {
		fmt.Printf("open plugin %s error: %s", ClientLib, err.Error())
		_ = daemonize.SignalOutcome(err)
		os.Exit(1)
	}
	loadSym(handle)
	if *configVersion {
		fmt.Println(getVersion())
		os.Exit(0)
	}
	err = startClient(*configFile, nil, nil)
	if err != nil {
		fmt.Printf("\nStart fuse client failed: %v\n", err.Error())
		_ = daemonize.SignalOutcome(err)
		os.Exit(1)
	} else {
		_ = daemonize.SignalOutcome(nil)
	}
	fd := getFuseFd()
	for {
		time.Sleep(CheckUpdateInterval)
		reload := os.Getenv("RELOAD_CLIENT")
		if reload != "1" && reload != "test" {
			continue
		}

		clientState := stopClient()
		plugin.Close(ClientLib)
		if reload == "test" {
			runtime.GC()
			time.Sleep(CheckUpdateInterval)
		}

		handle, err = plugin.Open(ClientLib)
		if err != nil {
			fmt.Printf("open plugin %s error: %s", ClientLib, err.Error())
			os.Exit(1)
		}
		loadSym(handle)
		err = startClient(*configFile, fd, clientState)
		if err != nil {
			fmt.Printf(err.Error())
			os.Exit(1)
		}
	}
}

func startDaemon() error {
	cmdPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("start ChubaoFS client failed: cannot get absolute command path, err(%v)", err)
	}

	args := []string{"-f"}
	args = append(args, os.Args[1:]...)

	if *configFile != "" {
		configPath, err := filepath.Abs(*configFile)
		if err != nil {
			return fmt.Errorf("start ChubaoFS client failed: cannot get absolute command path of config file(%v) , err(%v)", *configFile, err)
		}
		for i := 0; i < len(args); i++ {
			if args[i] == "-c" {
				// Since *configFile is not "", the (i+1)th argument must be the config file path
				args[i+1] = configPath
				break
			}
		}
	}

	env := os.Environ()
	buf := new(bytes.Buffer)
	err = daemonize.Run(cmdPath, args, env, buf)
	if err != nil {
		if buf.Len() > 0 {
			fmt.Println(buf.String())
		}
		return fmt.Errorf("start ChubaoFS client failed.\n%v\n", err)
	}
	return nil
}
