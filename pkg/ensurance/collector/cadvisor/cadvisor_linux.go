//go:build linux
// +build linux

package cadvisor

import (
	"net/http"
	"strconv"
	"time"

	cmemory "github.com/google/cadvisor/cache/memory"
	cadvisorcontainer "github.com/google/cadvisor/container"
	info "github.com/google/cadvisor/info/v1"
	cadvisorapiv2 "github.com/google/cadvisor/info/v2"
	cmanager "github.com/google/cadvisor/manager"
	csysfs "github.com/google/cadvisor/utils/sysfs"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/gocrane/crane/pkg/common"
	"github.com/gocrane/crane/pkg/ensurance/collector/types"
	"github.com/gocrane/crane/pkg/utils"
)

var cadvisorMetrics = []types.MetricName{
	types.MetricNameContainerCpuTotalUsage,
	types.MetricNameContainerSchedRunQueueTime,
	types.MetricNameContainerCpuLimit,
	types.MetricNameContainerCpuQuota,
	types.MetricNameContainerCpuPeriod,
}

type ContainerState struct {
	stat      cadvisorapiv2.ContainerInfo
	timestamp time.Time
}

//CadvisorCollector is the collector to collect container state
type CadvisorCollector struct {
	Manager   Manager
	podLister corelisters.PodLister

	latestContainersStates map[string]ContainerState
}

type CadvisorManager struct {
	cmanager.Manager
}

var _ Manager = new(CadvisorManager)

func NewCadvisorCollector(podLister corelisters.PodLister, manager Manager) *CadvisorCollector {
	c := CadvisorCollector{
		Manager:   manager,
		podLister: podLister,
	}
	return &c
}

func NewCadvisorManager() Manager {
	var includedMetrics = cadvisorcontainer.MetricSet{
		cadvisorcontainer.CpuUsageMetrics:         struct{}{},
		cadvisorcontainer.ProcessSchedulerMetrics: struct{}{},
	}

	allowDynamic := true
	maxHousekeepingInterval := 10 * time.Second
	memCache := cmemory.New(10*time.Minute, nil)
	sysfs := csysfs.NewRealSysFs()
	maxHousekeepingConfig := cmanager.HouskeepingConfig{Interval: &maxHousekeepingInterval, AllowDynamic: &allowDynamic}

	m, err := cmanager.New(memCache, sysfs, maxHousekeepingConfig, includedMetrics, http.DefaultClient, []string{utils.CgroupKubePods}, "")
	if err != nil {
		klog.Errorf("Failed to create cadvisor manager start: %v", err)
		return nil
	}

	if err := m.Start(); err != nil {
		klog.Errorf("Failed to start cadvisor manager: %v", err)
		return nil
	}

	return &CadvisorManager{
		m,
	}
}

// Stop cadvisor and clear existing factory
func (c *CadvisorCollector) Stop() error {
	return nil
}

func (c *CadvisorCollector) GetType() types.CollectType {
	return types.CadvisorCollectorType
}

func (c *CadvisorCollector) Collect() (map[string][]common.TimeSeries, error) {
	var containerStates = make(map[string]ContainerState)

	allPods, err := c.podLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list all pods: %v", err)
		return nil, err
	}
	var extResCpuUse float64 = 0

	var stateMap = make(map[string][]common.TimeSeries)
	for _, pod := range allPods {
		var now = time.Now()
		containers, err := c.Manager.GetContainerInfoV2(types.GetCgroupPath(pod), cadvisorapiv2.RequestOptions{
			IdType:    cadvisorapiv2.TypeName,
			Count:     1,
			Recursive: true,
		})
		if err != nil {
			klog.Errorf("GetContainerInfoV2 failed: %v", err)
			continue
		}
		for key, v := range containers {
			containerId := utils.GetContainerIdFromKey(key)
			containerName := utils.GetContainerNameFromPod(pod, containerId)
			// Filter the sandbox container
			if (containerId != "") && (containerName == "") {
				continue
			}

			_, hasExtRes := utils.GetContainerExtCpuResFromPod(pod, containerName)

			// In the GetContainerInfoV2 not collect the cpu quota and period
			// We used GetContainerInfo instead
			// issue https://github.com/google/cadvisor/issues/3040
			var query = info.ContainerInfoRequest{}
			containerInfoV1, err := c.Manager.GetContainerInfo(key, &query)
			if err != nil {
				klog.Errorf("ContainerInfoRequest failed: %v", err)
				continue
			}

			if state, ok := c.latestContainersStates[key]; ok {
				var containerLabels = GetContainerLabels(pod, containerId, containerName, hasExtRes)

				cpuUsageSample, schedRunqueueTime := caculateCPUUsage(&v, &state)
				if cpuUsageSample == 0 && schedRunqueueTime == 0 {
					continue
				}
				if hasExtRes {
					extResCpuUse += cpuUsageSample
				}
				addSampleToStateMap(types.MetricNameContainerCpuTotalUsage, composeSample(containerLabels, cpuUsageSample, now), stateMap)
				addSampleToStateMap(types.MetricNameContainerSchedRunQueueTime, composeSample(containerLabels, schedRunqueueTime, now), stateMap)
				addSampleToStateMap(types.MetricNameContainerCpuLimit, composeSample(containerLabels, float64(state.stat.Spec.Cpu.Limit), now), stateMap)
				addSampleToStateMap(types.MetricNameContainerCpuQuota, composeSample(containerLabels, float64(containerInfoV1.Spec.Cpu.Quota), now), stateMap)
				addSampleToStateMap(types.MetricNameContainerCpuPeriod, composeSample(containerLabels, float64(containerInfoV1.Spec.Cpu.Period), now), stateMap)

				klog.V(10).Infof("Pod: %s, containerName: %s, key %s, scheduler run queue time %.2f", klog.KObj(pod), containerName, key, schedRunqueueTime)
			}
			containerStates[key] = ContainerState{stat: v, timestamp: now}
		}
	}
	addSampleToStateMap(types.MetricNameExtResContainerCpuTotalUsage, composeSample(make([]common.Label, 0), extResCpuUse, time.Now()), stateMap)

	c.latestContainersStates = containerStates

	return stateMap, nil
}

func composeSample(labels []common.Label, cpuUsageSample float64, sampleTime time.Time) common.TimeSeries {
	return common.TimeSeries{
		Labels: labels,
		Samples: []common.Sample{
			{
				Value:     cpuUsageSample,
				Timestamp: sampleTime.Unix(),
			},
		},
	}
}

func addSampleToStateMap(metricsName types.MetricName, usage common.TimeSeries, storeMap map[string][]common.TimeSeries) {
	key := string(metricsName)
	if _, exists := storeMap[key]; !exists {
		storeMap[key] = []common.TimeSeries{usage}
	} else {
		storeMap[key] = append(storeMap[key], usage)
	}
}

func caculateCPUUsage(info *cadvisorapiv2.ContainerInfo, state *ContainerState) (float64, float64) {
	if info == nil ||
		state == nil ||
		len(info.Stats) == 0 {
		return 0, 0
	}
	cpuUsageIncrease := info.Stats[0].Cpu.Usage.Total - state.stat.Stats[0].Cpu.Usage.Total
	schedRunqueueTimeIncrease := info.Stats[0].Cpu.Schedstat.RunqueueTime - state.stat.Stats[0].Cpu.Schedstat.RunqueueTime
	timeIncrease := info.Stats[0].Timestamp.UnixNano() - state.stat.Stats[0].Timestamp.UnixNano()
	cpuUsageSample := float64(cpuUsageIncrease) / float64(timeIncrease)
	schedRunqueueTime := float64(schedRunqueueTimeIncrease) * 1000 * 1000 / float64(timeIncrease)
	return cpuUsageSample, schedRunqueueTime
}

func GetContainerLabels(pod *v1.Pod, containerId, containerName string, hasExtRes bool) []common.Label {
	return []common.Label{
		{Name: common.LabelNamePodName, Value: pod.Name},
		{Name: common.LabelNamePodNamespace, Value: pod.Namespace},
		{Name: common.LabelNamePodUid, Value: string(pod.UID)},
		{Name: common.LabelNameContainerName, Value: containerName},
		{Name: common.LabelNameContainerId, Value: containerId},
		{Name: common.LabelNameHasExtRes, Value: strconv.FormatBool(hasExtRes)},
	}
}

func CheckMetricNameExist(name string) bool {
	for _, vv := range cadvisorMetrics {
		if string(vv) == name {
			return true
		}
	}
	return false
}
