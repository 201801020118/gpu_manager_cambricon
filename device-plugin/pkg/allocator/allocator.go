// Copyright 2021 Cambricon, Inc.
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

package allocator

import (
	"strings"

	"github.com/Cambricon/cambricon-k8s-device-plugin/device-plugin/pkg/cndev"
)

type Allocator interface {
	Allocate(available []uint, required []uint, size int) ([]uint, error)
}

func New(policy string, devs map[string]*cndev.Device) Allocator {
	model := cndev.GetDeviceModel(uint(0))                         //根据设备号获取设备名称
	if strings.Contains(model, "MLU290") || model == "MLU370-M8" { //model是否包含"MLU290"或model为"MLU370-M8"
		return NewSpiderAllocator(policy, devs) //创建一个spider分配块,传入了分配政策和设备信息
	}
	if model == "MLU370-X8" {
		return NewBoardAllocator(policy, devs) //创建一个board分配块,比起spider多了获取cpu组
	}
	return NewDefaultAllocator(policy, devs) //如果都不是就返回default分配块,块内对象和spider一致
}

func contains(set []uint, dev uint) bool {
	for i := range set {
		if set[i] == dev {
			return true
		}
	}
	return false
}

func containsAll(set []uint, devs []uint) bool {
	for _, dev := range devs {
		if !contains(set, dev) {
			return false
		}
	}
	return true
}
