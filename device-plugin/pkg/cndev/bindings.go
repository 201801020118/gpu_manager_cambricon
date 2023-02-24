// Copyright 2020 Cambricon, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cndev

// #cgo LDFLAGS: -ldl -Wl,--unresolved-symbols=ignore-in-object-files
// #include "include/cndev.h"
import "C"

import (
	"errors"
	"fmt"
	"log"
	"time"
	"unsafe"
)

const version = 5

func errorString(ret C.cndevRet_t) error {
	if ret == C.CNDEV_SUCCESS {
		return nil
	}
	err := C.GoString(C.cndevGetErrorString(ret))
	return fmt.Errorf("cndev: %v", err)
}

func Init() error {
	r := dl.cndevInit()
	if r == C.CNDEV_ERROR_UNINITIALIZED {
		return errors.New("could not load CNDEV library")
	}
	return errorString(r)
}

func Release() error {
	r := dl.cndevRelease()
	return errorString(r)
}

func GetDeviceCount() (uint, error) { //返回硬件设备数量
	var cardInfos C.cndevCardInfo_t        //定义设备数量结构体,包含版本和无符号数量
	cardInfos.version = C.int(version)     //设置版本为5
	r := C.cndevGetDeviceCount(&cardInfos) //获取硬件设备数量,输出为cardInfos
	return uint(cardInfos.number), errorString(r)
}

func GetDeviceModel(idx uint) string {
	return C.GoString(C.getCardNameStringByDevId(C.int(idx)))
	//	根据设备号获取设备名称
}

func GetDeviceMemory(idx uint) (uint, error) {
	var cardMemInfo C.cndevMemoryInfo_t
	cardMemInfo.version = C.int(version)
	r := C.cndevGetMemoryUsage(&cardMemInfo, C.int(idx))
	return uint(cardMemInfo.physicalMemoryTotal), errorString(r)
}

func GetMLULinkGroups() ([][]uint, error) {
	num, err := GetDeviceCount()
	if err != nil {
		return nil, err
	}
	slots := map[string]uint{}
	for i := uint(0); i < num; i++ {
		uuid, _, _, _, err := getDeviceInfo(i)
		if err != nil {
			return nil, err
		}
		slots[uuid] = i
	}
	group := map[uint]bool{}
	queue := []uint{0}
	visited := map[uint]bool{}
	for len(queue) != 0 {
		slot := queue[0]
		queue = queue[1:]
		visited[slot] = true
		devs, err := getDeviceMLULinkDevs(slot)
		if err != nil {
			return nil, err
		}
		for dev := range devs {
			if _, ok := slots[dev]; !ok {
				continue
			}
			if !visited[slots[dev]] {
				queue = append(queue, slots[dev])
			}
		}
		group[slot] = true
	}
	// We assume there are at most 2 groups.
	group1 := []uint{}
	group2 := []uint{}
	for idx := range group {
		group1 = append(group1, idx)
	}
	for slot := uint(0); slot < num; slot++ {
		if !group[slot] {
			group2 = append(group2, slot)
		}
	}
	if len(group2) != 0 {
		return [][]uint{group1, group2}, nil
	}
	return [][]uint{group1}, nil
}

func getDeviceMLULinkDevs(idx uint) (map[string]int, error) {
	devs := make(map[string]int)
	portNum := C.cndevGetMLULinkPortNumber(C.int(idx))
	for i := 0; i < int(portNum); i++ {
		var status C.cndevMLULinkStatus_t
		status.version = C.int(version)
		r := C.cndevGetMLULinkStatus(&status, C.int(idx), C.int(i))
		err := errorString(r)
		if err != nil {
			return nil, err
		}
		if status.isActive == C.CNDEV_FEATURE_DISABLED {
			log.Printf("MLU %v port %v disabled", idx, i)
			continue
		}
		var remoteinfo C.cndevMLULinkRemoteInfo_t
		remoteinfo.version = C.int(version)
		r = C.cndevGetMLULinkRemoteInfo(&remoteinfo, C.int(idx), C.int(i))
		err = errorString(r)
		if err != nil {
			return nil, err
		}
		uuid := fmt.Sprintf("MLU-%s", C.GoString((*C.char)(unsafe.Pointer(&remoteinfo.uuid))))
		devs[uuid]++
	}
	return devs, nil
}

func getDeviceInfo(idx uint) (string, string, string, string, error) { //获取当前设备的具体信息
	var cardName C.cndevCardName_t
	var cardSN C.cndevCardSN_t
	var uuidInfo C.cndevUUID_t

	cardName.version = C.int(version)              //api版本为5
	r := C.cndevGetCardName(&cardName, C.int(idx)) //返回当前设备list中第i个设备的设备名
	err := errorString(r)
	if err != nil {
		return "", "", "", "", err
	}

	if cardName.id == C.MLU100 {
		log.Panicln("MLU100 detected, there is no way to be here.")
	}

	cardSN.version = C.int(version)
	r = C.cndevGetCardSN(&cardSN, C.int(idx)) //获取sn码
	err = errorString(r)
	if err != nil {
		return "", "", "", "", err
	}

	uuidInfo.version = C.int(version)
	r = C.cndevGetUUID(&uuidInfo, C.int(idx)) //获取uuid
	err = errorString(r)
	if err != nil {
		return "", "", "", "", err
	}
	uuid := C.GoString((*C.char)(unsafe.Pointer(&uuidInfo.uuid)))

	return fmt.Sprintf("MLU-%s", uuid), fmt.Sprintf("%x", int(cardSN.sn)), fmt.Sprintf("%x", int(cardSN.motherBoardSn)), fmt.Sprintf("/dev/cambricon_dev%d", idx), nil
	//将当前设备的地址存为/dev/cambricon_dev+index(也就是第i个)
}

func getDeviceHealthState(idx uint, delayTime int) (int, error) {
	var ret C.cndevRet_t
	var cardHealthState C.cndevCardHealthState_t
	var healthCode int
	cardHealthState.version = C.int(version)
	// sleep for some seconds
	time.Sleep(time.Duration(delayTime) * time.Second)
	ret = C.cndevGetCardHealthState(&cardHealthState, C.int(idx))
	healthCode = int(cardHealthState.health)
	return healthCode, errorString(ret)
}

func getDevicePCIeInfo(idx uint) (*pcie, error) {
	//相关知识:https://blog.csdn.net/weixin_42394088/article/details/114906656
	var pcieInfo C.cndevPCIeInfo_t                 //获取当前设备上PCIe总线上板卡的ID结构体。
	pcieInfo.version = C.int(version)              //获取当前的api版本信息,为5
	r := C.cndevGetPCIeInfo(&pcieInfo, C.int(idx)) //获取设备ID信息。需要设备支持PCIe标准
	if err := errorString(r); err != nil {
		return nil, err
	}
	return &pcie{
		domain:   int(pcieInfo.domain),   //一个片域
		bus:      int(pcieInfo.bus),      //PCIe设备的Bus ID(总线号)
		device:   int(pcieInfo.device),   //PCIe设备的device ID(设备号)
		function: int(pcieInfo.function), //PCIe设备的function ID(功能号)
		//,通过以上信息可以定位一个pcie设备
	}, nil
}
