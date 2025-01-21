#!/bin/bash

# Copyright 2018 The CubeFS Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
# implied. See the License for the specific language governing
# permissions and limitations under the License.

MntPoint=/cfs/mnt
EcMntPoint=/cfs/ecmnt
RocksDBMntPoint=/cfs/rocksdbmnt
mkdir -p /cfs/bin /cfs/log /cfs/mnt
src_path=/go/src/github.com/chubaofs/cfs
cli=/cfs/bin/cfs-cli
conf_path=/cfs/conf
cover_path=/cfs/coverage

Master1Addr="192.168.0.11:17010"
LeaderAddr=""
VolName=ltptest
RocksDBVolName=rocksdb-volume
Owner=ltptest
EcVolName=ltpectest
EcOwner=ltpectest
AccessKey=39bEF4RrAQgMj6RV
SecretKey=TRL6o3JL16YOqvZGIohBDFTHZDEcFsyd
AuthKey="0e20229116d5a9a4a9e876806b514a85"
EcAuthKey="65e85cb182cb2cdfb418c94f9a4ebba0"

init_cli() {
    cp ${cli} /usr/bin/
    cd ${conf_path}
    ${cli} completion
    echo 'source '${conf_path}'/cfs-cli.sh' >> ~/.bashrc
}
check_cluster() {
    echo -n "Checking cluster  ... "
    for i in $(seq 1 300) ; do
        ${cli} cluster info &> /tmp/cli_cluster_info
        LeaderAddr=`cat /tmp/cli_cluster_info | grep -i "master leader" | awk '{print$4}'`
        if [[ "x$LeaderAddr" != "x" ]] ; then
            echo -e "\033[32mdone\033[0m"
            return
        fi
        sleep 1
    done
    echo -e "\033[31mfail\033[0m"
    exit 1
}

ensure_node_writable() {
    node=$1
    echo -n "Checking $node ... "
    for i in $(seq 1 300) ; do
        ${cli} ${node} list &> /tmp/cli_${node}_list;
        res=`cat /tmp/cli_${node}_list | grep "Yes" | grep "Active" | wc -l`
        if [[ ${res} -eq $2 ]]; then
            echo -e "\033[32mdone\033[0m"
            return
        fi
        sleep 1
    done
    echo -e "\033[31mfail\033[0m"
    cat /tmp/cli_${node}_list
    exit 1
}

create_cluster_user() {
    echo -n "Creating user     ... "
    # check user exist
    ${cli} user info ${Owner} &> /dev/null
    if [[ $? -eq 0 ]] ; then
        echo -e "\033[32mdone\033[0m"
        return
    fi
    # try create user
    for i in $(seq 1 300) ; do
        curl http://master.chubao.io/user/create -H "Content-Type: application/json" -X POST \
         -d '{
          "id":"'${Owner}'",
          "pwd":"",
          "ak":"'${AccessKey}'",
          "sk":"'${SecretKey}'",
          "type":3,
          "description":""
        }' > /tmp/cli_user_create
        if [[ $? -eq 0 ]] ; then
            echo -e "\033[32mdone\033[0m"
            return
        fi
        sleep 1
    done
    echo -e "\033[31mfail\033[0m"
    exit 1
}

create_volume() {
    echo -n "Creating volume   ... "
    # check mem volume exist
    ${cli} volume info ${VolName} &> /dev/null
    if [[ $? -eq 0 ]]; then
        echo -e "\033[32mdone\033[0m"
    else

      create_volume_with_para ${VolName} ${Owner} 30 1 false > /dev/null
      if [[ $? -ne 0 ]]; then
        echo -e "\033[31mfail\033[0m"
        exit 1
      fi
    fi

    # check rocksdb volume exist
    ${cli} volume info ${RocksDBVolName} &> /dev/null
    if [[ $? -eq 0 ]]; then
        echo -e "\033[32mdone\033[0m"
    else
      create_volume_with_para ${RocksDBVolName} ${Owner} 30 2 false > /dev/null
      if [[ $? -ne 0 ]]; then
        echo -e "\033[31mfail\033[0m"
        exit 1
      fi
    fi
    echo -e "\033[32mdone\033[0m"
}

# $1:name
# $2:owner
# $3:capacity
# $4:store-mode [1:Mem, 2:Rocks]
# $5:follower-read  true/false
create_volume_with_para() {
  curl http://master.chubao.io/admin/createVol \
    -d name=$1 \
    -d owner=$2 \
    -d mpCount=3 \
    -d size=120 \
    -d capacity=$3 \
    -d ecDataNum=4 \
    -d ecParityNum=2 \
    -d ecEnable=false \
    -d followerRead=$5 \
    -d forceROW=false \
    -d writeCache=false \
    -d crossRegion=0 \
    -d autoRepair=false \
    -d replicaNum=3 \
    -d mpReplicaNum=3 \
    -d volWriteMutex=false \
    -d zoneName=default \
    -d trashRemainingDays=0 \
    -d storeMode=$4 \
    -d metaLayout="0,0" \
    -d smart=false \
    -d smartRules="" \
    -d compactTag=false \
    -d hostDelayInterval=0 \
    -d batchDelInodeCnt=0 \
    -d delInodeInterval=0 \
    -d enableBitMapAllocator=0 \
    -d metaOut=false
}

show_cluster_info() {
    tmp_file=/tmp/collect_cluster_info
    ${cli} cluster info &>> ${tmp_file}
    echo &>> ${tmp_file}
    ${cli} metanode list &>> ${tmp_file}
    echo &>> ${tmp_file}
    ${cli} datanode list &>> ${tmp_file}
    echo &>> ${tmp_file}
    ${cli} user info ${Owner} &>> ${tmp_file}
    echo &>> ${tmp_file}
    ${cli} volume info ${VolName} &>> ${tmp_file}
    echo &>> ${tmp_file}
    ${cli} volume info ${RocksDBVolName} &>> ${tmp_file}
    echo &>> ${tmp_file}
    cat /tmp/collect_cluster_info | grep -v "Master address"
}

add_data_partitions() {
    echo -n "Increasing DPs    ... "
    ${cli} vol add-dp ${VolName} 2 &> /dev/null
    if [[ $? -eq 0 ]] ; then
        echo -e "\033[32madd dp for mem volume done\033[0m"
    else
      echo -e "\033[31mfail\033[0m"
      exit 1
    fi

    curl -s "${LeaderAddr}/dataPartition/create?name=${RocksDBVolName}&count=2" | grep '"code":0' &> /dev/null
    if [[ $? -eq 0 ]] ; then
        echo -e "\033[32madd dp for rocksdb volume done\033[0m"
        return
    fi

    echo -e "\033[31mfail\033[0m"
    exit 1
}

create_idc() {
    echo -n "Checking idc  ... "
    curl -s "${LeaderAddr}/idc/get?name=huitian" | grep '"default":"hdd"' &> /dev/null
    if [[ $? -eq 0 ]] ; then
        echo -e "\033[32m done\033[0m"
        return
    fi
    echo -n "Create huitian idc   ... "
    curl -s "${LeaderAddr}/idc/create?name=huitian" | grep '"code":0' &> /dev/null
    if [[ $? -eq 0 ]] ; then
        echo -e "\033[32m done\033[0m"
    else
        echo -e "\033[31m fail\033[0m"
        exit 1
    fi
    echo -n "Set idc default zone mediumType hdd   ... "
    curl -s "${LeaderAddr}/zone/setIDC?zoneName=default&idcName=huitian&mediumType=hdd" | grep '"code":0' &> /dev/null
    if [[ $? -eq 0 ]] ; then
         echo -e "\033[32m done\033[0m"
    else
        echo -e "\033[31m fail\033[0m"
        exit 1
    fi
}

print_error_info() {
    echo "------ err ----"
    cat /cfs/log/cfs.out
    cat /cfs/log/ltptest/ltptest_info.log
    cat /cfs/log/ltptest/ltptest_error.log
    cat /cfs/log/ltptest/ltptest_warn.log
    curl -s "http://$LeaderAddr/admin/getCluster" | jq
    mount
    df -h
    stat $MntPoint
    stat $RocksDBMntPoint
    ls -l $MntPoint
    ls -l $RocksDBMntPoint
    ls -l $LTPTestDir
}

start_client() {
    mkdir -p $RocksDBMntPoint
    echo -n "Starting client   ... "
    nohup /cfs/bin/cfs-client -test.coverprofile=client.cov -test.outputdir=${cover_path} -c /cfs/conf/client.json 2>&1 /cfs/log/cfs.out &
    nohup /cfs/bin/cfs-client -test.coverprofile=client_rocksdb.cov -test.outputdir=${cover_path} -c /cfs/conf/client_rocksdb.json 2>&1 /cfs/log/cfs_rocksdb.out &
    sleep 10
    res=$( mount | grep -q "$VolName on $MntPoint" ; echo $? )
    if [[ $res -ne 0 ]] ; then
        echo -e "\033[31mfail\033[0m"
        print_error_info
        exit $res
    fi
    sleep 1
    res=$( mount | grep -q "$RocksDBVolName on $RocksDBMntPoint" ; echo $? )
    if [[ $res -ne 0 ]] ; then
        echo -e "\033[31mfail\033[0m"
        print_error_info
        exit $res
    fi
    echo -e "\033[32mdone\033[0m"
}

wait_proc_done() {
    proc_name=$1
    pid=$( ps -ef | grep "$proc_name" | grep -v "grep" | awk '{print $2}' )
    logfile=$2
    logfile2=${logfile}-2
    logfile3=${logfile}-3
    maxtime=${3:-3000}
    checktime=${4:-60}
    retfile=${5:-"/tmp/ltpret"}
    timeout=1
    pout=0
    lastlog=""
    for i in $(seq 1 $maxtime) ; do
        if ! `ps -ef  | grep -v "grep" | grep -q "$proc_name" ` ; then
            echo "$proc_name run done"
            timeout=0
            break
        fi
        sleep 1
        ((pout+=1))
        if [ $(cat $logfile | wc -l) -gt 0  ] ; then
            pout=0
            cat $logfile > $logfile2 && cat $logfile2 >> $logfile3 && > $logfile
            cat $logfile2 && rm -f $logfile2
        fi
        if [[ $pout -ge $checktime ]] ; then
            echo -n "."
            pout=0
        fi
    done
    if [[ $timeout -eq 1 ]] ;then
        echo "$proc_name run timeout"
        print_error_info
        exit 1
    fi
    ret=$(cat /tmp/ltpret)
    if [[ "-$ret" != "-0" ]] ; then
        exit $ret
    fi
}

reload_client() {
    echo -n "run update libcfssdk.so libcfsc.so test    ... "
    curl "http://127.0.0.1:17410/set/clientUpgrade?version=test"
    sleep 5
    res=$( stat $MntPoint | grep -q "Transport endpoint is not connected" ; echo $? )
    if [[ $res -eq 0 ]] ; then
        echo -e "\033[31mfail\033[0m"
        print_error_info
        exit $res
    fi
    res=$( stat $RocksDBMntPoint | grep -q "Transport endpoint is not connected" ; echo $? )
    if [[ $res -eq 0 ]] ; then
        echo -e "\033[31mfail\033[0m"
        print_error_info
        exit $res
    fi
    echo ""
}

run_unit_test() {
    echo "Running unit test"
    echo "************************";
    echo "       unit test       ";
    echo "************************";
    export GO111MODULE="off"
    pushd /go/src/github.com/cubefs/cubefs > /dev/null
    packages=$(GO111MODULE="off" go list \
            ./master/... \
            ./datanode/... \
            ./metanode/... \
            ./objectnode/... \
            ./schedulenode/intramig/... \
            ./schedulenode/migcore/... \
            ./schedulenode/smart/... \
            ./schedulenode/worker/... \
            ./codecnode/... \
            ./ecnode/... \
            ./storage/... \
            ./client/fs/... \
            ./client/cache/... \
            ./sdk/data/... \
            ./sdk/meta/... \
            ./sdk/master/... \
            ./sdk/s3/... \
            ./repl/... \
            ./raftstore/rafttest/... \
            ./util/... \
            ./vendor/github.com/tiglabs/raft/...)
    echo "Following packages will be tested and record code coverage:"
    for package in `echo ${packages}`; do
        echo "  * "${package};
    done

    test_output_file=${cover_path}/unittest.out
    echo "Running unit tests ..."
    go test -v -covermode=atomic -timeout 20m -coverprofile=${cover_path}/unittest.cov ${packages} > ${test_output_file}
    ret=$?
    popd > /dev/null
    pass_num=`grep "PASS:" ${test_output_file} | wc -l`
    fail_num=`grep "FAIL:" ${test_output_file} | wc -l`
    skip_num=`grep "SKIP:" ${test_output_file} | wc -l`
    total_num=`expr ${pass_num} + ${fail_num}`
    echo "Unit test complete returns ${ret}: PASS ${pass_num}, FAIL ${fail_num}, SKIP ${skip_num}, TOTAL ${total_num}"
    if [[ $skip_num -ne 0 ]]; then
      grep "SKIP:" ${test_output_file}
    fi
    if [[ $fail_num -ne 0 ]]; then
      grep "FAIL:" ${test_output_file}
    fi
    if [[ $ret -ne 0 ]]; then
        echo -e "Unit test: \033[32mFAIL\033[0m"
        exit $ret
    fi
    echo -e "Unit test: \033[32mPASS\033[0m"
}

run_ltptest() {
    echo "Running LTP test"
    echo "************************";
    echo "        LTP test        ";
    echo "************************";

    LTPMemTestDir=$MntPoint/ltptest
    LTPRocksDBTestDir=$RocksDBMntPoint/ltptest
    LtpMemLog=/tmp/ltp.log
    LtpRocksDBLog=/tmp/ltp_rocksdb.log
    mkdir -p $LTPMemTestDir
    mkdir -p $LTPRocksDBTestDir
    nohup /bin/sh -c " /opt/ltp/runltp  -f fs -d $LTPMemTestDir > $LtpMemLog 2>&1; echo $? > /tmp/ltpret " &
    nohup /bin/sh -c " /opt/ltp/runltp  -f fs -d $LTPRocksDBTestDir > $LtpRocksDBLog 2>&1; echo $? > /tmp/ltpret_rocksdb " &
    wait_proc_done "runltp" $LtpMemLog
    echo "------------------------";
    echo "Failed LTP Test Cases:"
    cat /opt/ltp/output/*
    if [ $(cat /opt/ltp/output/* | wc -l) -ne 0  ] ; then
      echo -e "\033[31m ltp test fail\033[0m"
      exit 1
    fi
    echo "------------------------";
}

stop_client() {
    echo -n "Stopping client   ... "
    umount ${MntPoint}
    umount ${RocksDBMntPoint}
    echo -e "\033[32mdone\033[0m" || { echo -e "\033[31mfail\033[0m"; exit 1; }
}

delete_volume() {
    echo -n "Deleting volume   ... "
    curl -s "${LeaderAddr}/vol/delete?name=${VolName}&authKey=${AuthKey}" | grep '"code":0' &> /dev/null
    if [[ $? -eq 0 ]]; then
        echo -e "\033[32mdelete mem volume done\033[0m"
    else
      echo -e "\033[31mfail\033[0m"
      exit 1
    fi

    curl -s "${LeaderAddr}/vol/delete?name=${RocksDBVolName}&authKey=${AuthKey}" | grep '"code":0' &> /dev/null
    if [[ $? -eq 0 ]]; then
        echo -e "\033[32mdelete rocksdb volume done\033[0m"
        return
    fi
    echo -e "\033[31mfail\033[0m"
    exit 1
}

run_s3_test() {
    work_path=/opt/s3tests;
    echo "Running S3 compatibility tests"
    echo "******************************";
    echo "    S3 compatibility tests    ";
    echo "******************************";

    python3 -m unittest2 discover ${work_path} "*.py" -v
    if [[ $? -ne 0 ]]; then
        exit 1
    fi
}

set_trash_days() {
   echo -n "set trash days... "
   curl -s "${LeaderAddr}/vol/update?name=${VolName}&authKey=${AuthKey}&trashRemainingDays=2" | grep '"code":0' &> /dev/null
   if [[ $? -ne 0 ]]; then
        echo -e "\033[31mfail\033[0m"
        exit 1
   fi
      curl -s "${LeaderAddr}/vol/update?name=${RocksDBVolName}&authKey=${AuthKey}&trashRemainingDays=2" | grep '"code":0' &> /dev/null
   if [[ $? -ne 0 ]]; then
        echo -e "\033[31mfail\033[0m"
        exit 1
   fi
   echo -e "\033[32mdone\033[0m"
}

run_trash_test() {
   echo -n "run trash test... "
   ${cli} trash test --vol ${VolName} > /dev/null
   if [[ $? -ne 0 ]]; then
        echo -e "\033[31mfail\033[0m"
	      cp -r /tmp/cfs/cli /cfs/log/
        exit 1
   fi
   ${cli} trash test --vol ${RocksDBVolName} > /dev/null
   if [[ $? -ne 0 ]]; then
        echo -e "\033[31mfail\033[0m"
	      cp -r /tmp/cfs/cli /cfs/log/
        exit 1
   fi
   cp -r /tmp/cfs/cli /cfs/log/
   echo -e "\033[32mdone\033[0m"
}

run_ectest() {
    export GO111MODULE="off"
    base_path=/go/src/github.com/cubefs/cubefs
    echo "Running EC Consistency Test"
    echo "************************";
    echo "   EC Consistency Test  ";
    echo "************************";
    pre_ec_consistency_test
    ret=$?
    if [[ $ret -ne 0 ]]; then
        exit $ret
    fi

    echo "Ec Consistency Test start"
    ec_consistency_test
    ret=$?
    if [[ $ret -ne 0 ]]; then
        exit $ret
    fi
    echo "Ec Consistency Test end"

    after_ec_consistency_test
    ret=$?
    if [[ $ret -ne 0 ]]; then
        exit $ret
    fi
}

pre_ec_consistency_test() {
  mkdir -p /cfs/ecmnt
  echo -n "Creating EcVolume   ... "
  ${cli} volume create ${EcVolName} ${EcOwner} --capacity=30 --ecEnable=true -y > /dev/null
  if [[ $? -ne 0 ]]; then
      echo -e "\033[31mfail\033[0m"
      return 1
  fi

  ${cli} volume ec-set ${EcVolName} --ecRetryWait 1 > /dev/null
  if [[ $? -ne 0 ]]; then
      echo -e "set ecRetryWait \033[31mfail\033[0m"
      return 1
  fi

  ret=`${cli} volume info ${EcVolName} | grep EcEnable | awk '{print $3}'`
  if [[ "$ret" == "false" ]]; then
      echo -e "\033[31mfail\033[0m"
      return 1
  fi
  echo -e "\033[32mdone\033[0m"

  echo -n "Starting EcClient   ... "
  nohup /cfs/bin/cfs-client -c /cfs/conf/client_ec.json >/cfs/log/cfs.out 2>&1 &
  sleep 10
  res=$( mount | grep -q "$EcVolName on $EcMntPoint" ; echo $? )
  if [[ $res -ne 0 ]] ; then
      echo -e "\033[31mfail\033[0m"
      return $res
  fi
  echo -e "\033[32mdone\033[0m"
  return 0
}

ec_consistency_test() {
  test_files=(100M 1M 111K 10K)
  for file in ${test_files[@]};do
    echo -n "write to $EcMntPoint/$file   ... "
    timeout 180 dd if=/dev/zero of=$EcMntPoint/$file bs=$file count=1 &>/dev/null
    if [[ $? -ne 0 ]];then
      echo -e "\033[31mfail\033[0m"
    fi
    echo -e "\033[32mdone\033[0m"
  done

  dps=(`${cli} volume info ${EcVolName} --data-partition | awk '{print $1}' | sed  -e 's/[a-z|A-Z|:]//g' -e '/^$/d'`)

  echo "origin file md5 info:"
  for ((idx=0; idx<${#test_files[@]}; idx++));do
    md5_origin_files[$idx]=`timeout 180 md5sum $EcMntPoint/${test_files[$idx]} | awk '{print $1}'`
    printf "%-5s %-10s\n" ${test_files[$idx]} ${md5_origin_files[$idx]}
  done
  sleep 120
  needmigration=1
  time=0
  while ((1));do
    echo -e "\rMigration Start  timeout:(300)s curtime:($time)s  ... \c"
    for((idx=0; idx<${#dps[@]}; idx++));do
      ret=`${cli} datapartition info ${dps[$idx]} | grep USED -A 3 | awk 'NR==2{print $2}'`
      if [[ $ret == 0 ]]; then
          migration_status[$idx]=1
          continue
      fi

      if [[ needmigration -eq 1 ]];then
        curl -s "${LeaderAddr}/dataPartition/ecmigreate?id=${dps[$idx]}&test=true" &> /dev/null
        migration_status[$idx]=0
      fi

      ret=`${cli} ecpartition info ${dps[$idx]} | grep EcMigrateStatus | awk '{print $3}'`
      if [[ "$ret" == "FinishEc" ]];then
        migration_status[$idx]=1
      fi
    done
    migration_fin=1
    if [[ needmigration -eq 1 ]];then needmigration=0; fi
    for status in ${migration_status[@]};do
      if [[ $status -eq 0 ]]; then migration_fin=0; break; fi
    done
    if [[ $migration_fin -eq 1 ]]; then
      echo -e "\033[32mdone\033[0m"
      break
    fi
    if [[ $time -ge 300 ]]; then
      echo -e "\033[31mfail\033[0m"
      echo "migration timeout"
      return 1
    fi
    time=$((time+10))
    sleep 10
  done
  sleep 60
  for ((idx=0; idx<${#test_files[@]}; idx++));do
    ecmd5=`timeout 180 md5sum $EcMntPoint/${test_files[$idx]} | awk '{print $1}'`
    echo -n "${test_files[$idx]} origin:${md5_origin_files[$idx]} ec:$ecmd5   ... "
    if [[ "$ecmd5" == "${md5_origin_files[$idx]}" ]];then
      echo -e "\033[32mdone\033[0m"
      continue
    fi
    echo -e "\033[31mfail\033[0m"
  done

  return 0
}

after_ec_consistency_test() {
  echo -n "Stopping EcClient   ... "
  umount ${EcMntPoint}
  echo -e "\033[32mdone\033[0m" || { echo -e "\033[31mfail\033[0m"; exit 1; }

  echo -n "Deleting EcVolume   ... "
  curl -s "${LeaderAddr}/vol/delete?name=${EcVolName}&authKey=${EcAuthKey}" | grep '"code":0' &> /dev/null
  if [[ $? -ne 0 ]]; then
      echo -e "\033[31mfail\033[0m"
      return 1
  fi
  echo -e "\033[32mdone\033[0m"
  return 0
}

run_bypass_client_test() {
    echo "run bypass client test..."
    LD_PRELOAD=/usr/lib64/libcfsclient.so CFS_CONFIG_PATH=/usr/lib64/bypass.ini MOUNT_POINT=/cfs/mnt /cfs/bin/test-bypass
    if [[ $? -ne 0 ]]; then
      echo -e "\033[31mfail\033[0m"
      exit 1
    fi
}

init_cli
check_cluster
create_cluster_user
ensure_node_writable "metanode" 5
ensure_node_writable "datanode" 4
create_volume ; sleep 2
create_idc
add_data_partitions ; sleep 3
show_cluster_info
start_client ; sleep 2
run_unit_test
reload_client
run_ltptest
run_s3_test
if [[ $ECENABLE -eq 1 ]];then
  run_ectest
fi
set_trash_days; sleep 310
run_trash_test; sleep 2
stop_client ; sleep 20
run_bypass_client_test ; sleep 10
delete_volume
