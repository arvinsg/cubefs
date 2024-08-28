// Copyright 2018 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package cmd

import (
	"fmt"
	trash "github.com/cubefs/cubefs/cli/cmd/trash"
	"github.com/cubefs/cubefs/sdk/convert"
	"os"
	"path"
	"strings"

	"github.com/cubefs/cubefs/sdk/master"
	"github.com/cubefs/cubefs/util/log"
	"github.com/spf13/cobra"
)

const (
	cmdRootShort = "ChubaoFS Command Line Interface (CLI)"
)

type ChubaoFSCmd struct {
	CFSCmd *cobra.Command
}

func NewRootCmd(client *master.MasterClient, cc *convert.ConvertClient) *ChubaoFSCmd {
	var cmd = &ChubaoFSCmd{
		CFSCmd: &cobra.Command{
			Use:   path.Base(os.Args[0]),
			Short: cmdRootShort,
			Args:  cobra.MinimumNArgs(0),
		},
	}

	cmd.CFSCmd.AddCommand(
		cmd.newClusterCmd(client),
		newVolCmd(client),
		newInodeCmd(client),
		newUserCmd(client),
		newMetaNodeCmd(client),
		newDataNodeCmd(client),
		newCodEcnodeCmd(client),
		newEcNodeCmd(client),
		newEcPartitionCmd(client),
		newDataPartitionCmd(client),
		newMetaPartitionCmd(client),
		newConfigCmd(),
		newCompatibilityCmd(),
		newParseRaftLogCmd(),
		newRateLimitCmd(client),
		newExtentCmd(client),
		newZoneCmd(client),
		newRegionCmd(client),
		newReadWriteCheckTinyFileCmd(),
		trash.NewTrashCmd(client),
		newConvertNodeCmd(cc),
		newIdcCommand(client),
		newCompactCmd(client),
		newFlashGroupCommand(client),
		newFlashNodeCommand(client),
		newColdFileCmd(client),
	)
	return cmd
}

func stdout(format string, a ...interface{}) {
	_, _ = fmt.Fprintf(os.Stdout, format, a...)
}

func stdoutGreen(str string) {
	fmt.Printf("\033[1;40;32m%-8v\033[0m\n", str)
}

func stdoutRed(str string) {
	fmt.Printf("\033[1;40;31m%-8v\033[0m\n", str)
	stdoutGreen(strings.Repeat("_ ", partitionInfoTableHeaderLen/2+10) + "\n")
}

func errout(format string, a ...interface{}) {
	log.LogErrorf(format+"\n", a...)
	_, _ = fmt.Fprintf(os.Stderr, format, a...)
	OsExitWithLogFlush()
}

func OsExitWithLogFlush() {
	log.LogFlush()
	os.Exit(1)
}
