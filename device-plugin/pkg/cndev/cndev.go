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

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type pcie struct {
	domain   int
	bus      int
	device   int
	function int
}

type Device struct {
	Slot        uint
	UUID        string
	SN          string
	Path        string
	MotherBoard string
	pcie        *pcie
}

func NewDeviceLite(idx uint, pcieAware bool) (*Device, error) { //生成一个虚拟出的pcie轻量化设备信息
	var pcie *pcie

	uuid, sn, motherBoard, path, err := getDeviceInfo(idx) //获取当前设备list中指向为第i个的设备信息
	if err != nil {
		return nil, err
	}

	if pcieAware { //如果存在虚拟化方案的话
		pcie, err = getDevicePCIeInfo(idx) //获取当前设备的pcie设备信息,以及地址
		if err != nil {
			return nil, err
		}
	}

	return &Device{
		Slot:        idx, //device在deviceList中的位置信息
		UUID:        uuid,
		SN:          sn, //sn码
		Path:        path,
		MotherBoard: motherBoard,
		pcie:        pcie,
	}, nil
}

func (d *Device) GetGeviceHealthState(delayTime int) (int, error) {
	return getDeviceHealthState(d.Slot, delayTime)
}

func (d *Device) GetPCIeID() (string, error) { //根据一些pci信息生成唯一的pcie板卡ID
	if d.pcie == nil {
		return "", errors.New("device has no PCIe info")
	}
	domain := strconv.FormatInt(int64(d.pcie.domain), 16)
	domain = strings.Repeat("0", 4-len([]byte(domain))) + domain
	bus := strconv.FormatInt(int64(d.pcie.bus), 16)
	if d.pcie.bus < 16 {
		bus = "0" + bus
	}
	device := strconv.FormatInt(int64(d.pcie.device), 16)
	if d.pcie.device < 16 {
		device = "0" + device
	}
	function := strconv.FormatInt(int64(d.pcie.function), 16)
	return domain + ":" + bus + ":" + device + "." + function, nil
}

func (d *Device) EnableSriov(num int) error { //启用Sriov,num为mlu卡的虚拟化数量
	err := d.ValidateSriovNum(num) //
	if err != nil {
		return err
	}
	id, err := d.GetPCIeID() //根据一些pci信息生成唯一的pcie板卡ID
	if err != nil {
		return err
	}
	path := "/sys/bus/pci/devices/" + id + "/sriov_numvfs"
	vf, err := getNumFromFile(path) //查看最大虚拟设备数
	if err != nil {
		return err
	}
	if vf == num { //当设备的最大化虚拟设备数量与mlu卡的虚拟化数量相等时
		log.Println("sriov already enabled, pass")
		return nil
	}
	if vf != 0 {
		if err = setSriovNum(id, 0); err != nil {
			return fmt.Errorf("failed to set sriov num to 0, pcie: %s now: %d", id, vf)
		}
	}
	return setSriovNum(id, num) //将mlu卡的虚拟化数量写入到sriov_numvfs文件中
}

func (d *Device) ValidateSriovNum(num int) error { //
	id, err := d.GetPCIeID() //获取生成的PcieID
	if err != nil {
		return err
	}
	path := "/sys/bus/pci/devices/" + id + "/sriov_totalvfs" //定义path地址
	max, err := getNumFromFile(path)                         //查看最大虚拟设备数
	if err != nil {
		return err
	}
	if num < 1 || num > max {
		return fmt.Errorf("invalid sriov number %d, maximum: %d, minimum: 1", num, max)
	}
	return nil
}

func getNumFromFile(path string) (int, error) {
	output, err := ioutil.ReadFile(path)
	if err != nil {
		return 0, err
	}
	output = bytes.Trim(output, "\n")
	num, err := strconv.ParseInt(string(output), 10, 64)
	return int(num), err
}

func setSriovNum(id string, num int) error {
	path := "/sys/bus/pci/devices/" + id + "/sriov_numvfs"
	command := "echo " + strconv.Itoa(num) + " > " + path
	err := exec.Command("bash", "-c", command).Run()
	if err != nil {
		return fmt.Errorf("echo %d to file %s, err: %v", num, path, err)
	}
	time.Sleep(time.Second)
	got, err := getNumFromFile(path)
	if err != nil || got != num {
		return fmt.Errorf("the number of VFs is not expected. got: %d, err: %v, expected: %d", got, err, num)
	}
	return nil
}
