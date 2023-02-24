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

package mlu

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
)

func (m *CambriconDevicePlugin) getCandidatePods(ctx context.Context) ([]*v1.Pod, error) { //获取候选的pod且将其进行优先级排序
	candidatePods := []*v1.Pod{}                //Pod是可以在主机上运行的容器集合。此资源由客户端创建并安排到主机上。
	allPods, err := m.getPendingPodsInNode(ctx) //获取状态为pending(在准备的)pod
	if err != nil {
		return candidatePods, err
	}
	for _, pod := range allPods {
		current := pod
		if isMLUMemoryAssumedPod(&current) { //判断此pod是否可被列入候选pod中
			candidatePods = append(candidatePods, &current) //返回获选podlist
		}
	}
	sort.Slice(candidatePods, func(i, j int) bool { //slice:对传入的数组做一个规则为fun的排序
		return getAssumeTimeFromPodAnnotation(candidatePods[i]) < getAssumeTimeFromPodAnnotation(candidatePods[j])
		//如果在数组中i位置的pod的假定分配时间戳小于在数组总j位置的假定分配时间戳
	})
	return candidatePods, nil //返回排序后的pod
}

func (m *CambriconDevicePlugin) getPendingPodsInNode(ctx context.Context) ([]v1.Pod, error) {
	pods := []v1.Pod{}
	podMap := make(map[types.UID]bool) //UID是此对象的唯一时间和空间值。它通常由服务器成功创建资源，不允许在PUT上更改操作。

	selector := fields.SelectorFromSet(fields.Set{"spec.nodeName": m.nodeHostname, "status.phase": "Pending"}) //返回与给定Set完全匹配的Selector
	podList, err := m.clientset.CoreV1().Pods(v1.NamespaceAll).List(ctx, metav1.ListOptions{
		//在所有的命名空间的pod中筛选状态为准备,所在节点的主机名为当前主机名的pod
		FieldSelector: selector.String(), //FieldSelector是通过字段限制返回对象列表的选择器
	})
	for i := 0; i < retries && err != nil; i++ { //重复五次这种选择过程
		log.Printf("list pods error %v, retried %d times", err, i)
		time.Sleep(100 * time.Second)
		podList, err = m.clientset.CoreV1().Pods(v1.NamespaceAll).List(ctx, metav1.ListOptions{
			FieldSelector: selector.String(),
		})
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get pending pods assigned to node %v", m.nodeHostname)
	}

	for _, pod := range podList.Items {
		if _, ok := podMap[pod.UID]; !ok {
			pods = append(pods, pod)
			podMap[pod.UID] = true
		}
	}
	return pods, nil //返回可选择的pod集
}

func isMLUMemoryAssumedPod(pod *v1.Pod) bool { //查看当前pod是否拥有所需求的资源,是否可以被列在候选pod里
	if !requestsMLUMemory(pod) { //请求当前pod的容器list的最大限制内存,如果当前资源不允许,返回false
		return false
	}

	if _, ok := pod.ObjectMeta.Annotations[mluMemResourceAssumeTime]; !ok { //这里这个time是什么意思?
		//也许是测试这个pod是否是正常运行的
		return false
	}

	if assigned, ok := pod.ObjectMeta.Annotations[mluMemResourceAssigned]; ok && assigned == "false" {
		//如果内存资源在pod中存在且此pod未被分配,返回可以假定选择这个pod
		return true
	}

	return false
}

func requestsMLUMemory(pod *v1.Pod) bool { //请求mlu内存
	r := false
	for _, c := range pod.Spec.Containers { //pod.spec.Containers是当前pod中的容器列表.且pod必须存在容器
		if _, ok := c.Resources.Limits[v1.ResourceName(mluMemResourceName)]; ok {
			//获取当前容器内的最大显示内存
			r = true
			break
		}
	}
	return r
}

func getAssumeTimeFromPodAnnotation(pod *v1.Pod) (assumeTime uint64) { //获取假定分配时间
	if assumeTimeStr, ok := pod.ObjectMeta.Annotations[mluMemResourceAssumeTime]; ok { //获取资源分配假定时间
		u64, err := strconv.ParseUint(assumeTimeStr, 10, 64)
		if err != nil {
			log.Printf("Failed to parse assume Timestamp %s due to %v", assumeTimeStr, err)
		} else {
			assumeTime = u64
		}
	}
	return assumeTime
}

func getIndexFromAnnotation(pod *v1.Pod) (uint, error) { //返回拥有内存划分指数对象的pod
	value, found := pod.ObjectMeta.Annotations[mluMemSplitIndex] //获取拥有内存划分指数对象的pod
	if !found {
		return 0, fmt.Errorf("pod annotation %s not found", mluMemSplitIndex)
	}
	index, err := strconv.Atoi(value) //将value转化为int类型
	if err != nil {
		return 0, fmt.Errorf("strconv value %v, %v", value, err)
	}
	if index < 0 {
		return 0, fmt.Errorf("index %d less than 0", index)
	}
	return uint(index), nil
}

func podContainerCountWithMlu(pod *v1.Pod) uint { //返回pod内满足内存资源需求的容器和临时容器数量
	count := 0
	for _, c := range pod.Spec.InitContainers { //pod内的containers
		if _, ok := c.Resources.Limits[v1.ResourceName(mluMemResourceName)]; ok { //如果容器的内存资源足够
			count++ //计数加一
			log.Printf("namespace %s pod %s init container %s uses mlu-mem, just allocate the mlu and ignore memory limit", pod.Namespace, pod.Name, c.Name)
		}
	}
	for _, c := range pod.Spec.Containers { //此pod中运行的临时容器列表
		if _, ok := c.Resources.Limits[v1.ResourceName(mluMemResourceName)]; ok { //如果临时容器的内存资源足够
			count++ //计数加一
		}
	}
	return uint(count)
}

func (m *CambriconDevicePlugin) releaseNodeLock() error {
	node, err := m.clientset.CoreV1().Nodes().Get(context.TODO(), m.nodeHostname, metav1.GetOptions{})

	if err != nil {
		return err
	}
	newNode := node.DeepCopy()
	//复制一个新节点

	if newNode.Annotations != nil {
		if time, ok := newNode.Annotations[mluMemLock]; ok {
			log.Printf("node lock timestamp %s", time)
			delete(newNode.Annotations, mluMemLock)
		} else {
			log.Println("Lock is released, No Need to update node")
			return nil
		}
	}
	_, err = m.clientset.CoreV1().Nodes().Update(context.TODO(), newNode, metav1.UpdateOptions{})

	if err != nil {
		log.Printf("Failed to release node lock %s, err %v", mluMemLock, err)
	} else {
		log.Printf("release node lock %s successfully.", mluMemLock)
	}
	return err
}

func (m *CambriconDevicePlugin) patchMLUCount(count int) error {

	patchAnnotations := map[string]interface{}{
		"metadata": map[string]map[string]string{"annotations": {
			mluResourceCount: fmt.Sprintf("%d", count),
		}}}

	b, err := json.Marshal(patchAnnotations)
	if err != nil {
		return err
	}

	_, err = m.clientset.CoreV1().Nodes().Patch(context.TODO(), m.nodeHostname, types.StrategicMergePatchType, b, metav1.PatchOptions{})
	if err != nil {
		log.Printf("Failed to update Capacity %s.", mluResourceCount)
	} else {
		log.Printf("Updated Capacity %s to %d successfully.", mluResourceCount, count)
	}
	return err
}
