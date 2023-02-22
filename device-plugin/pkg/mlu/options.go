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

package mlu

import (
	"log"
	"os"
	"strings"

	flags "github.com/jessevdk/go-flags"
)

type Options struct {
	Mode string `long:"mode" description:"device plugin mode" default:"default" choice:"default" choice:"sriov" choice:"env-share" choice:"topology-aware" choice:"mlu-share"`
	//设备插件模式,一些虚拟化的模式
	MLULinkPolicy string `long:"mlulink-policy" description:"MLULink topology policy" default:"best-effort" choice:"best-effort" choice:"restricted" choice:"guaranteed"`
	//MLULink拓扑策略
	VirtualizationNum uint `long:"virtualization-num" description:"the virtualization number for each MLU, used only in sriov mode or env-share mode" default:"1" env:"VIRTUALIZATION_NUM"`
	//每个MLU的虚拟化编号，仅在sriov模式或env共享模式下使用
	DisableHealthCheck bool `long:"disable-health-check" description:"disable MLU health check"`
	//禁用MLU健康检查
	NodeName string `long:"node-name" description:"host node name" env:"NODE_NAME"`
	//主机node节点名
	EnableConsole bool `long:"enable-console" description:"enable UART console device(/dev/ttyMS) in container"`
	//在容器中启用UART控制台设备（/dev/ttyMS）
	EnableDeviceType bool `long:"enable-device-type" description:"enable device registration with type info"`
	//使用类型信息启用设备注册
	CnmonPath string `long:"cnmon-path" description:"host cnmon path"`
}

func ParseFlags() Options {
	for index, arg := range os.Args {
		if strings.HasPrefix(arg, "-mode") {
			os.Args[index] = strings.Replace(arg, "-mode", "--mode", 1)
			break
		}
	}
	if os.Getenv("DP_DISABLE_HEALTHCHECKS") == "all" {
		os.Args = append(os.Args, "--disable-health-check")
	}
	options := Options{}
	parser := flags.NewParser(&options, flags.Default)
	if _, err := parser.Parse(); err != nil {
		code := 1
		if fe, ok := err.(*flags.Error); ok {
			if fe.Type == flags.ErrHelp {
				code = 0
			}
		}
		os.Exit(code)
	}
	log.Printf("Parsed options: %v\n", options)
	return options
}
