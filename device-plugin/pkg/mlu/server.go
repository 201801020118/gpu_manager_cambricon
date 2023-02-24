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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Cambricon/cambricon-k8s-device-plugin/device-plugin/pkg/allocator"
	"github.com/Cambricon/cambricon-k8s-device-plugin/device-plugin/pkg/cndev"
	"github.com/Cambricon/cambricon-k8s-device-plugin/device-plugin/pkg/common"
	"google.golang.org/grpc"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// CambriconDevicePlugin implements the Kubernetes device plugin API
// 实现的k8s设备插件API
type CambriconDevicePlugin struct {
	devs         []*pluginapi.Device
	devsInfo     map[string]*cndev.Device
	socket       string
	stop         chan interface{}
	health       chan *pluginapi.Device
	server       *grpc.Server
	deviceList   *deviceList
	allocator    allocator.Allocator
	nodeHostname string
	clientset    kubernetes.Interface
	options      Options
	sync.RWMutex
	containerIndex uint
}

// NewCambriconDevicePlugin returns an initialized CambriconDevicePlugin
func NewCambriconDevicePlugin(o Options) *CambriconDevicePlugin {
	devs, devsInfo := getDevices(o.Mode, int(o.VirtualizationNum)) //获取设备信息,包括已经虚拟化过的pcie卡的信息,VirtualizationNum是将一张gpu卡分成pcie卡的数量
	return &CambriconDevicePlugin{
		devs:         devs,
		devsInfo:     devsInfo,
		socket:       serverSock,
		stop:         make(chan interface{}),
		health:       make(chan *pluginapi.Device),
		deviceList:   newDeviceList(),
		nodeHostname: o.NodeName,
		options:      o,
	}
}

func (m *CambriconDevicePlugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{
		GetPreferredAllocationAvailable: m.options.Mode == topologyAware,
	}, nil
}

// dial establishes the gRPC communication with the registered device plugin.
func dial(unixSocketPath string, timeout time.Duration) (*grpc.ClientConn, error) {
	c, err := grpc.Dial(unixSocketPath, grpc.WithInsecure(), grpc.WithBlock(),
		grpc.WithTimeout(timeout),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, timeout)
		}),
	)

	if err != nil {
		return nil, err
	}

	return c, nil
}

// Start starts the gRPC server of the device plugin
// 在设备插件开启grpc服务
func (m *CambriconDevicePlugin) Start() error {
	err := m.cleanup() //删除device-plugin的socket文件夹
	if err != nil {
		return err
	}

	sock, err := net.Listen("unix", m.socket) //监听socket文件
	if err != nil {
		return err
	}

	m.server = grpc.NewServer([]grpc.ServerOption{}...) //创建一个空的grpc服务器
	pluginapi.RegisterDevicePluginServer(m.server, m)   //在这里调用了listandwatch
	//RegisterService将服务及其实现注册到gRPC服务器。它是从IDL生成的代码调用的。必须在调用Serve之前调用此函数。
	go m.server.Serve(sock) //启动grpc服务

	// Wait for server to start by launching a blocking connection
	conn, err := dial(m.socket, 5*time.Second) //创建一个阻塞
	if err != nil {
		return err
	}
	conn.Close() //结束阻塞

	if !m.options.DisableHealthCheck {
		go m.healthcheck() //监听是否健康
	}

	return nil
}

// Stop stops the gRPC server
func (m *CambriconDevicePlugin) Stop() error {
	if m.server == nil {
		return nil
	}

	m.server.Stop()
	m.server = nil
	close(m.stop)

	return m.cleanup()
}

// Register registers the device plugin for the given resourceName with Kubelet.
// Register向Kubelet注册给定resourceName的设备插件
func (m *CambriconDevicePlugin) Register(kubeletEndpoint, resourceName string) error {
	conn, err := dial(kubeletEndpoint, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := pluginapi.NewRegistrationClient(conn)
	reqt := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     path.Base(m.socket),
		ResourceName: resourceName,
		Options: &pluginapi.DevicePluginOptions{
			GetPreferredAllocationAvailable: m.options.Mode == topologyAware,
		},
	}

	_, err = client.Register(context.Background(), reqt)
	if err != nil {
		return err
	}
	return nil
}

// ListAndWatch lists devices and update that list according to the health status
func (m *CambriconDevicePlugin) ListAndWatch(e *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	s.Send(&pluginapi.ListAndWatchResponse{Devices: m.devs})

	for {
		select {
		case <-m.stop:
			return nil
		case d := <-m.health:
			for i, dev := range m.devs {
				if dev.ID == d.ID {
					m.devs[i].Health = d.Health
					break
				}
			}
			s.Send(&pluginapi.ListAndWatchResponse{Devices: m.devs})
		}
	}
}

func (m *CambriconDevicePlugin) PrepareResponse(uuids []string) pluginapi.ContainerAllocateResponse {

	resp := pluginapi.ContainerAllocateResponse{}
	//containerAllocateReponse包含
	//1:在容器中设置以访问一个或多个设备的环境变量列表
	//2:容器的装载
	//3:容器内的设备??????????
	//4:传递到容器运行时的容器注释

	resp.Mounts = []*pluginapi.Mount{ //指定要装载在容器上的主机卷,设备库或工具在主机或容器上的安装位置
		{
			ContainerPath: mluRPMsgDir, //容器上装载的路径
			HostPath:      mluRPMsgDir, //主机上装载的路劲
		},
	}

	if m.options.CnmonPath != "" { //如果寒武纪硬件监控工具的安装地址不为空,将其装载进容器
		resp.Mounts = append(resp.Mounts, &pluginapi.Mount{
			ContainerPath: m.options.CnmonPath,
			HostPath:      m.options.CnmonPath,
			ReadOnly:      true,
		})
	}

	if m.options.Mode == mluShare { //如果模式为mlu_share,将smlu_container的地址装载进容器
		resp.Mounts = append(resp.Mounts, &pluginapi.Mount{
			ContainerPath: mluMemBinaryPath,
			HostPath:      mluMemBinaryPath,
			ReadOnly:      true,
		})
		if m.deviceList.hasSplitDev { //如果设备具有切分模块时?(不确定)
			addDevice(&resp, mluSplitDeviceName, mluSplitDeviceName) //将切分模块挂载在容器中
		}
	}

	devpaths := m.uuidToPath(uuids) //通过uuid获取当前设备的基础信息,并返回该设备的路径

	if m.deviceList.hasCtrlDev { //如果设备具有监视模块时?(不确定)
		addDevice(&resp, mluMonitorDeviceName, mluMonitorDeviceName) //将监控模块挂载在容器中
	}

	for id, devpath := range devpaths {
		if m.options.Mode == sriov { //当虚拟化方式为sriov时
			vfid := strings.Split(devpath, mluDeviceName)[1] //获取当前设备的vfid为设备列表中的第几个设备
			if m.deviceList.hasCommuDev {                    //如果设备具有(通信)模块时?
				addDevice(&resp, mluCommuDeviceName+vfid, mluCommuDeviceName+strconv.Itoa(id)) //将通信模块挂载在容器中.应该是通信模块在本地的名字就会有一些添加
			}
			addDevice(&resp, devpath, mluDeviceName+strconv.Itoa(id))
			continue
		}

		var index int
		_, err := fmt.Sscanf(devpath, mluDeviceName+"%d", &index)
		if err != nil {
			log.Printf("Failed to get device index for device path %v", err)
			continue
		}
		if m.deviceList.hasMsgqDev {
			addDevice(&resp, fmt.Sprintf(mluMsgqDeviceName+":%d", index), fmt.Sprintf(mluMsgqDeviceName+":%d", id))
		}
		if m.deviceList.hasRPCDev {
			addDevice(&resp, fmt.Sprintf(mluRPCDeviceName+":%d", index), fmt.Sprintf(mluRPCDeviceName+":%d", id))
		}
		if m.deviceList.hasCmsgDev {
			addDevice(&resp, fmt.Sprintf(mluCmsgDeviceName+"%d", index), fmt.Sprintf(mluCmsgDeviceName+"%d", id))
		}
		if m.deviceList.hasCommuDev {
			addDevice(&resp, fmt.Sprintf(mluCommuDeviceName+"%d", index), fmt.Sprintf(mluCommuDeviceName+"%d", id))
		}
		if m.deviceList.hasIpcmDev {
			addDevice(&resp, fmt.Sprintf(mluIpcmDeviceName+"%d", index), fmt.Sprintf(mluIpcmDeviceName+"%d", id))
		}
		if m.deviceList.hasUARTConsoleDev && m.options.EnableConsole {
			addDevice(&resp, fmt.Sprintf(mluUARTConsoleDeviceName+"%d", index), fmt.Sprintf(mluUARTConsoleDeviceName+"%d", id))
		}
		addDevice(&resp, devpath, mluDeviceName+strconv.Itoa(id)) //将本地第i个设备挂载到容器中,就是已经分号了gpu,把本地分好,存好的很多个gpu,选择性的挂载到相应的gpu中
	}
	return resp
}

func (m *CambriconDevicePlugin) GetDeviceUUIDByIndex(index uint) (uuid string, found bool) {
	//通过index获取当前设备的UUid
	for uuid, info := range m.devsInfo {
		if info.Slot == index {
			return uuid, true
		}
	}
	return "", false
}

func (m *CambriconDevicePlugin) allocateMLUShare(ctx context.Context, reqs *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {

	m.Lock()
	defer m.Unlock()

	pods, err := m.getCandidatePods(ctx) //返回候选的podlist且进行优先级排序
	if err != nil {
		log.Printf("Failed to get candidate pods, err %v", err)
		m.containerIndex = 0
		return nil, fmt.Errorf("getCandidatePods %v", err)
	}

	var assumePod *v1.Pod //创建pod对象
	if len(pods) != 1 {   //如果候选podlist的长度不为1,输出有多少哥候选pod(无意义,日志罢了)
		m.containerIndex = 0
		log.Printf("Number of candidate Pods %d", len(pods))
		return nil, fmt.Errorf("Number of candidate Pods %d", len(pods))
	}

	assumePod = pods[0]                           //假定pod为候选pod里的第一个pod
	counts := podContainerCountWithMlu(assumePod) //获取当前pod内满足内存资源需求的容器以及临时容器数量

	index, err := getIndexFromAnnotation(assumePod) //获取拥有内存划分指数对象的pod的index标识(不确定)
	if err != nil {
		m.containerIndex = 0
		log.Printf("Failed to get index from annotation, err %v", err)
		return nil, fmt.Errorf("getIndexFromAnnotation %v", err)
	}
	uuid, ok := m.GetDeviceUUIDByIndex(index) //通过index获取当前设备的UUid
	if !ok {
		m.containerIndex = 0
		log.Printf("Failed to get uuid by index %d", index)
		return nil, fmt.Errorf("failed GetDeviceUUIDByIndex %d", index)
	}

	responses := pluginapi.AllocateResponse{}    //初始化allocateResponse
	for _, req := range reqs.ContainerRequests { //初始化containerRequest
		reqMem := len(req.DevicesIDs)             //获取当前设备的DevicesIds
		resp := m.PrepareResponse([]string{uuid}) //将一些信息装载到容器中
		resp.Envs = map[string]string{
			mluMemSplitEnable: "1",                       //内存拆分启用标志为已启用
			mluMemSplitIndex:  fmt.Sprintf("%d", index),  //拆分指数定义为当前pod的标志
			mluMemSplitLimit:  fmt.Sprintf("%d", reqMem), //拆分限制为当前设备的数量
		}
		responses.ContainerResponses = append(responses.ContainerResponses, &resp) //将相关的信息封装到ContainerResponses中
	}

	if m.containerIndex < counts-1 {
		m.containerIndex++
		log.Printf("Pod %s has %d containers, creating %d container", assumePod.Name, counts, m.containerIndex)
	} else {
		log.Printf("Creating last container in pod %s", assumePod.Name)
		m.containerIndex = 0
		err = m.releaseNodeLock()
		for i := 0; i < retries && err != nil; i++ {
			log.Printf("Failed to release node lock, err %v, retried %d times", err, i)
			time.Sleep(100 * time.Millisecond)
			err = m.releaseNodeLock()
		}
		if err != nil {
			log.Printf("releaseNodeLock exceeds retry count %d", retries)
		}

		patchedAnnotation, err := json.Marshal(
			map[string]interface{}{
				"metadata": map[string]map[string]string{"annotations": {
					mluMemResourceAssigned: "true",
				}}})
		if err != nil {
			log.Printf("Failed to patch pod annotation. err: %v", err)
			return nil, fmt.Errorf("patchPodAnnotation %v", err)
		}
		_, err = m.clientset.CoreV1().Pods(assumePod.Namespace).Patch(ctx, assumePod.Name, types.StrategicMergePatchType, patchedAnnotation, metav1.PatchOptions{})
		for i := 0; i < retries && err != nil; i++ {
			log.Printf("patchPodAnnotation err: %v, retried times: %d", err, i)
			time.Sleep(100 * time.Millisecond)
			_, err = m.clientset.CoreV1().Pods(assumePod.Namespace).Patch(ctx, assumePod.Name, types.StrategicMergePatchType, patchedAnnotation, metav1.PatchOptions{})
		}
		if err != nil {
			return nil, fmt.Errorf("patchPodAnnotation exceeds retry count %d", retries)
		}
	}

	return &responses, nil
}

// Allocate which return list of devices.
// 返回设备列表
func (m *CambriconDevicePlugin) Allocate(ctx context.Context, reqs *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {

	if m.options.Mode == mluShare {
		return m.allocateMLUShare(ctx, reqs) //如果虚拟化模式是mlu_share
	}

	responses := pluginapi.AllocateResponse{}
	for _, req := range reqs.ContainerRequests {
		for _, id := range req.DevicesIDs {
			if !deviceExists(m.devs, id) {
				return nil, fmt.Errorf("invalid allocation request: unknown device: %s", id)
			}
		}
		car := m.PrepareResponse(req.DevicesIDs)
		responses.ContainerResponses = append(responses.ContainerResponses, &car)
	}
	return &responses, nil
}

func (m *CambriconDevicePlugin) uuidToPath(uuids []string) []string {
	//将uuid传入该设备的基础信息中,并通过该uuid返回该设备的路径
	var paths []string
	for _, uuid := range uuids {
		dev := m.devsInfo[uuid]         //通过uuid获取到当前设备的信息,也就是Device结构体中的内容
		paths = append(paths, dev.Path) //通过uuid获取该设备的地址
	}
	return paths //返回该设备地址
}

func (m *CambriconDevicePlugin) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

func (m *CambriconDevicePlugin) cleanup() error { //删除指定socket的文件夹
	if err := os.Remove(m.socket); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (m *CambriconDevicePlugin) healthcheck() {
	ctx, cancel := context.WithCancel(context.Background())
	health := make(chan *pluginapi.Device)

	go watchUnhealthy(ctx, m.devsInfo, health)

	for {
		select {
		case <-m.stop:
			cancel()
			return
		case dev := <-health:
			m.health <- dev
		}
	}
}

// Serve starts the gRPC server and register the device plugin to Kubelet
// 开启grpc客户端并向k8s注册device-plugin
func (m *CambriconDevicePlugin) Serve() error {
	if m.options.CnmonPath != "" && !path.IsAbs(m.options.CnmonPath) {
		log.Panicf("invalid cnmon path: %s", m.options.CnmonPath)
	}

	if m.options.Mode == topologyAware { //如果虚拟化模式是topology-aware模式
		m.allocator = allocator.New(m.options.MLULinkPolicy, m.devsInfo) //传入MLULink的分配策略,和设备信息,然后再根据卡的型号来判断是建造那种allocate结构
		m.clientset = initClientSet()                                    //初始化Clientset,Clientset 是调用 Kubernetes 资源对象最常用的客户端，可以操作所有的资源对象

		if m.options.MLULinkPolicy != common.BestEffort { //分配规则不为最大努力时
			if err := m.updateNodeMLULinkAnnotation(0); err != nil { //更新节点关于分配规则的注释
				return err
			}
		}
	}

	if m.options.Mode == mluShare { //如果虚拟化模式是mlu-share模式
		m.clientset = initClientSet()
		if num, err := cndev.GetDeviceCount(); err != nil { //获取设备数量
			return err
		} else if err = m.patchMLUCount(int(num)); err != nil {
			return err
		}
		if err := m.releaseNodeLock(); err != nil { //释放节点
			return err
		}
	}

	if err := m.Start(); err != nil { //开启grpc连接
		return fmt.Errorf("start device plugin err: %v", err)
	}

	log.Printf("Starting to serve on socket %v", m.socket)
	resourceName := "cambricon.com/mlu" //定义资源名称
	if m.options.EnableDeviceType {
		model := cndev.GetDeviceModel(uint(0)) //获取设备名称
		if model == "" {
			m.Stop() //停止grpc连接
			return errors.New("device type enabled, but got empty device model from cndev")
		}
		if strings.EqualFold(model, "MLU270-X5K") { //设备名称是否为"MLU270-X5K"
			resourceName = "cambricon.com/" + strings.ToLower(model) //定义资源名称
		} else {
			resourceName = "cambricon.com/" + strings.Split(strings.ToLower(model), "-")[0]
		}
	}
	if m.options.Mode == mluShare { //如果虚拟化模式为:mlu-share
		resourceName = mluMemResourceName //定义资源名称
	}
	if err := m.Register(pluginapi.KubeletSocket, resourceName); err != nil {
		//Register向Kubelet注册给定resourceName的设备插件,到这一步才注册了设备插件!
		m.Stop()
		return fmt.Errorf("register resource %s err: %v", resourceName, err)
	}
	log.Printf("Registered resource %s", resourceName)
	return nil
}

func (m *CambriconDevicePlugin) GetPreferredAllocation(ctx context.Context, r *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	response := &pluginapi.PreferredAllocationResponse{}
	for _, req := range r.ContainerRequests {
		available := m.getSlots(req.AvailableDeviceIDs)
		required := m.getSlots(req.MustIncludeDeviceIDs)
		allocated, err := m.GetPreferredAllocatedDeviceUUIDs(available, required, int(req.AllocationSize))
		if err != nil {
			log.Printf("failed to get preferred allocated devices, available: %v, size: %d, err: %v \n", available, req.AllocationSize, err)
			return response, err
		}
		resp := &pluginapi.ContainerPreferredAllocationResponse{
			DeviceIDs: allocated,
		}
		response.ContainerResponses = append(response.ContainerResponses, resp)
	}
	return response, nil
}

func (m *CambriconDevicePlugin) GetPreferredAllocatedDeviceUUIDs(available []uint, required []uint, size int) ([]string, error) {

	// todo: consider required list for init containers and numa. ignore it for now.
	if len(required) != 0 {
		log.Printf("required device slice not empty, ignore it. %v \n", required)
	}

	log.Println("=== Start GetPreferredAllocatedDeviceUUIDs ===")
	log.Printf("available devs: %v, size %d", available, size)

	devs, err := m.allocator.Allocate(available, required, size)
	if err != nil {
		if e := m.updateNodeMLULinkAnnotation(size); e != nil {
			log.Printf("updateNodeMLULinkAnnotation err: %v", e)
		}
		return nil, err
	}

	log.Printf("preferred devices %v", devs)

	uuids := []string{}
	for _, dev := range devs {
		uuid, found := m.GetDeviceUUIDByIndex(dev)
		if !found {
			return nil, fmt.Errorf("uuid not found for dev %d", dev)
		}
		uuids = append(uuids, uuid)
	}

	log.Println("=== Finish GetPreferredAllocatedDeviceUUIDs ===")
	return uuids, nil
}

func (m *CambriconDevicePlugin) createAnnotationWithTimestamp(size int) error {
	node, err := m.clientset.CoreV1().Nodes().Get(context.TODO(), m.nodeHostname, metav1.GetOptions{}) //update node
	if err != nil {
		return fmt.Errorf("get node err %v", err)
	}
	if size == 0 {
		delete(node.Annotations, mluLinkPolicyUnsatisfied) //删除带有mluLinkPolicyUnsatisfied的node.annotations元素
	} else {
		timeStamp := strconv.FormatInt(time.Now().Unix(), 10) //从10到36.用诸如abcd的字母来表示时间戳
		if len(node.Annotations) == 0 {
			node.Annotations = make(map[string]string)
		}
		node.Annotations[mluLinkPolicyUnsatisfied] = fmt.Sprintf("%d-%s-%s", size, m.options.MLULinkPolicy, timeStamp)
		//对于不满足分配政策的节点,赋值为"大小-分配规则-时间戳"
	}
	_, err = m.clientset.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update node err: %v", err)
	}
	return nil
}

func (m *CambriconDevicePlugin) updateNodeMLULinkAnnotation(size int) error { //更新节点的关于分配规则的注释
	err := m.createAnnotationWithTimestamp(size) //对于不满足分配规则的节点赋予注释
	for i := 0; i < retries && err != nil; i++ {
		log.Printf("createAnnotationWithTimestamp err: %v, retried times: %d", err, i+1)
		time.Sleep(100 * time.Millisecond)
		err = m.createAnnotationWithTimestamp(size)
	}
	return err
}

func (m *CambriconDevicePlugin) getSlots(ids []string) []uint {
	slots := []uint{}
	for _, id := range ids {
		mlu := m.devsInfo[id]
		slots = append(slots, mlu.Slot)
	}
	return slots
}

func addDevice(car *pluginapi.ContainerAllocateResponse, hostPath string, containerPath string) {
	dev := new(pluginapi.DeviceSpec)
	//指定一个主机设备挂载到容器,定义容器中设备的路径,主机中设备的路径,cgroups的权限控制
	dev.HostPath = hostPath
	dev.ContainerPath = containerPath
	dev.Permissions = "rw"
	car.Devices = append(car.Devices, dev) //添加到Devices中
}

func initClientSet() kubernetes.Interface { //初始化客户端,与k8s进行通信
	config, err := rest.InClusterConfig() //看不懂,https://blog.csdn.net/qq_24433609/article/details/127192779
	if err != nil {
		log.Printf("Failed to get in cluser config, err: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config) //通过*rest.Config参数和NewForConfig方法来获取clientset对象，clientset是多个client的集合，每个client可能包含不同版本的方法调用,
	//NewForConfig函数就是初始化clientset中的每个client
	if err != nil {
		log.Printf("Failed to init clientset, err: %v", err)
	}
	return clientset
}
