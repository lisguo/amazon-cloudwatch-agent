// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package resolver

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	mapset "github.com/deckarep/golang-set/v2"
	"go.opentelemetry.io/collector/pdata/pcommon"
	semconv "go.opentelemetry.io/collector/semconv/v1.17.0"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	// kubeAllowedStringAlphaNums holds the characters allowed in replicaset names from as parent deployment
	// https://github.com/kubernetes/apimachinery/blob/master/pkg/util/rand/rand.go#L83
	kubeAllowedStringAlphaNums = "bcdfghjklmnpqrstvwxz2456789"

	// Deletion delay adjustment:
	// Previously, EKS resolver would instantly remove the IP to Service mapping when a pod was destroyed.
	// This posed a problem because:
	//   1. Metric data is aggregated and emitted every 1 minute.
	//   2. If this aggregated metric data, which contains the IP of the now-destroyed pod, arrives
	//      at the EKS resolver after the IP records have already been deleted, the metric can't be processed correctly.
	//
	// To mitigate this issue, we've introduced a 2-minute deletion delay. This ensures that any
	// metric data that arrives within those 2 minutes, containing the old IP, will still get mapped correctly to a service.
	deletionDelay = 2 * time.Minute
)

var DefaultHostedInAttributeMap = map[string]string{
	semconv.AttributeK8SNamespaceName: HostedInAttributeK8SNamespace,
}

var (
	// ReplicaSet name = Deployment name + "-" + 10 alphanumeric characters long string (see https://stackoverflow.com/questions/46204504/kubernetes-pod-naming-convention and https://stackoverflow.com/questions/71090356/how-does-random-string-in-kubernetes-pod-name-decided)
	// the alphanumeric characters in Kubernetes are restricted to exclude vowels to reduce the chances of "normal words" being formed. (see https://github.com/kubernetes/apimachinery/blob/master/pkg/util/rand/rand.go#L83)
	replicaSetWithDeploymentNamePattern = fmt.Sprintf(`^(.+)-[%s]{10}$`, kubeAllowedStringAlphaNums)
	deploymentFromReplicaSetPattern     = regexp.MustCompile(replicaSetWithDeploymentNamePattern)
	// if a pod is launched directly by a replicaSet (with a given name by users), its name has the following pattern:
	// Pod name = ReplicaSet name + 5 alphanumeric characters long string
	podWithReplicaSetNamePattern = fmt.Sprintf(`^(.+)-[%s]{5}$`, kubeAllowedStringAlphaNums)
	replicaSetFromPodPattern     = regexp.MustCompile(podWithReplicaSetNamePattern)
)

type eksResolver struct {
	logger                         *zap.Logger
	clientset                      kubernetes.Interface
	ipToPod                        *sync.Map
	podToWorkloadAndNamespace      *sync.Map
	ipToServiceAndNamespace        *sync.Map
	serviceAndNamespaceToSelectors *sync.Map
	workloadAndNamespaceToLabels   *sync.Map
	serviceToWorkload              *sync.Map // computed from serviceAndNamespaceToSelectors and workloadAndNamespaceToLabels every 1 min
	workloadPodCount               map[string]int
	safeStopCh                     *safeChannel // trace and metric processors share the same eksResolver and might close the same channel separately
}

// a safe channel which can be closed multiple times
type safeChannel struct {
	sync.Mutex

	ch     chan struct{}
	closed bool
}

func (sc *safeChannel) Close() {
	sc.Lock()
	defer sc.Unlock()

	if !sc.closed {
		close(sc.ch)
		sc.closed = true
	}
}

var (
	once     sync.Once
	instance *eksResolver
)

func jitterSleep(seconds int) {
	jitter := time.Duration(rand.Intn(seconds)) * time.Second // nolint:gosec
	time.Sleep(jitter)
}

func attachNamespace(resourceName, namespace string) string {
	// character "@" is not allowed in kubernetes resource names: https://unofficial-kubernetes.readthedocs.io/en/latest/concepts/overview/working-with-objects/names/
	return resourceName + "@" + namespace
}

func getServiceAndNamespace(service *corev1.Service) string {
	return attachNamespace(service.Name, service.Namespace)
}

func extractResourceAndNamespace(serviceOrWorkloadAndNamespace string) (string, string) {
	// extract service name and namespace from serviceAndNamespace
	parts := strings.Split(serviceOrWorkloadAndNamespace, "@")
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func extractWorkloadNameFromRS(replicaSetName string) (string, error) {
	match := deploymentFromReplicaSetPattern.FindStringSubmatch(replicaSetName)
	if match != nil {
		return match[1], nil
	}

	return "", errors.New("failed to extract workload name from replicatSet name: " + replicaSetName)
}

func extractWorkloadNameFromPodName(podName string) (string, error) {
	match := replicaSetFromPodPattern.FindStringSubmatch(podName)
	if match != nil {
		return match[1], nil
	}

	return "", errors.New("failed to extract workload name from pod name: " + podName)
}

func getWorkloadAndNamespace(pod *corev1.Pod) string {
	var workloadAndNamespace string
	if pod.ObjectMeta.OwnerReferences != nil {
		for _, ownerRef := range pod.ObjectMeta.OwnerReferences {
			if workloadAndNamespace != "" {
				break
			}

			if ownerRef.Kind == "ReplicaSet" {
				if workloadName, err := extractWorkloadNameFromRS(ownerRef.Name); err == nil {
					// when the replicaSet is created by a deployment, use deployment name
					workloadAndNamespace = attachNamespace(workloadName, pod.Namespace)
				} else if workloadName, err := extractWorkloadNameFromPodName(pod.Name); err == nil {
					// when the replicaSet is not created by a deployment, use replicaSet name directly
					workloadAndNamespace = attachNamespace(workloadName, pod.Namespace)
				}
			} else if ownerRef.Kind == "StatefulSet" {
				workloadAndNamespace = attachNamespace(ownerRef.Name, pod.Namespace)
			} else if ownerRef.Kind == "DaemonSet" {
				workloadAndNamespace = attachNamespace(ownerRef.Name, pod.Namespace)
			}
		}
	}

	return workloadAndNamespace
}

// Deleter represents a type that can delete a key from a map after a certain delay.
type Deleter interface {
	DeleteWithDelay(m *sync.Map, key interface{})
}

// TimedDeleter deletes a key after a specified delay.
type TimedDeleter struct {
	Delay time.Duration
}

func (td *TimedDeleter) DeleteWithDelay(m *sync.Map, key interface{}) {
	go func() {
		time.Sleep(td.Delay)
		m.Delete(key)
	}()
}

func onAddOrUpdateService(obj interface{}, ipToServiceAndNamespace, serviceAndNamespaceToSelectors *sync.Map) {
	service := obj.(*corev1.Service)
	// service can also have an external IP (or ingress IP) that could be accessed
	// this field can be either an IP address (in some edge case) or a hostname (see "EXTERNAL-IP" column in "k get svc" output)
	// [ec2-user@ip-172-31-11-104 one-step]$ k get svc -A
	// NAMESPACE           NAME                          TYPE           CLUSTER-IP       EXTERNAL-IP                                                              PORT(S)                                     AGE
	// default             pet-clinic-frontend           ClusterIP      10.100.216.182   <none>                                                                   8080/TCP                                    108m
	// default             vets-service                  ClusterIP      10.100.62.167    <none>                                                                   8083/TCP                                    108m
	// default             visits-service                ClusterIP      10.100.96.5      <none>                                                                   8082/TCP                                    108m
	// ingress-nginx       default-http-backend          ClusterIP      10.100.11.231    <none>                                                                   80/TCP                                      108m
	// ingress-nginx       ingress-nginx                 LoadBalancer   10.100.154.5     aex7997ece08c435dbd2b912fd5aa5bd-5372117830.xxxxx.elb.amazonaws.com      80:32080/TCP,443:32081/TCP,9113:30410/TCP   108m
	// kube-system         kube-dns                      ClusterIP      10.100.0.10      <none>
	//
	// we ignore such case for now and may need to consider it in the future
	if service.Spec.ClusterIP != "" && service.Spec.ClusterIP != "None" {
		ipToServiceAndNamespace.Store(service.Spec.ClusterIP, getServiceAndNamespace(service))
	}
	labelSet := mapset.NewSet[string]()
	for key, value := range service.Spec.Selector {
		labelSet.Add(key + "=" + value)
	}
	if labelSet.Cardinality() > 0 {
		serviceAndNamespaceToSelectors.Store(getServiceAndNamespace(service), labelSet)
	}
}

func onDeleteService(obj interface{}, ipToServiceAndNamespace, serviceAndNamespaceToSelectors *sync.Map, deleter Deleter) {
	service := obj.(*corev1.Service)
	if service.Spec.ClusterIP != "" && service.Spec.ClusterIP != "None" {
		deleter.DeleteWithDelay(ipToServiceAndNamespace, service.Spec.ClusterIP)
	}
	deleter.DeleteWithDelay(serviceAndNamespaceToSelectors, getServiceAndNamespace(service))
}

func removeHostNetworkRecords(pod *corev1.Pod, ipToPod *sync.Map, deleter Deleter) {
	for _, port := range getHostNetworkPorts(pod) {
		deleter.DeleteWithDelay(ipToPod, pod.Status.HostIP+":"+port)
	}
}

func updateHostNetworkRecords(newPod *corev1.Pod, oldPod *corev1.Pod, ipToPod *sync.Map, deleter Deleter) {
	newHostIPPorts := make(map[string]bool)
	oldHostIPPorts := make(map[string]bool)

	for _, port := range getHostNetworkPorts(newPod) {
		newHostIPPorts[newPod.Status.HostIP+":"+port] = true
	}

	for _, port := range getHostNetworkPorts(oldPod) {
		oldHostIPPorts[oldPod.Status.HostIP+":"+port] = true
	}

	for oldHostIPPort := range oldHostIPPorts {
		if _, exist := newHostIPPorts[oldHostIPPort]; !exist {
			deleter.DeleteWithDelay(ipToPod, oldHostIPPort)
		}
	}

	for newHostIPPort := range newHostIPPorts {
		if _, exist := oldHostIPPorts[newHostIPPort]; !exist {
			ipToPod.Store(newHostIPPort, newPod.Name)
		}
	}
}

func handlePodAdd(pod *corev1.Pod, ipToPod *sync.Map) {
	if pod.Spec.HostNetwork {
		for _, port := range getHostNetworkPorts(pod) {
			ipToPod.Store(pod.Status.HostIP+":"+port, pod.Name)
		}
	} else if pod.Status.PodIP != "" {
		ipToPod.Store(pod.Status.PodIP, pod.Name)
	}
}

func handlePodUpdate(newPod *corev1.Pod, oldPod *corev1.Pod, ipToPod *sync.Map, deleter Deleter) {
	if oldPod.Spec.HostNetwork && newPod.Spec.HostNetwork {
		// Case 1: Both oldPod and newPod are using host network
		// Here we need to update the host network records accordingly
		updateHostNetworkRecords(newPod, oldPod, ipToPod, deleter)
	} else if oldPod.Spec.HostNetwork && !newPod.Spec.HostNetwork {
		// Case 2: The oldPod was using the host network, but the newPod is not
		// Here we remove the old host network records and add new PodIP record if it is not empty
		removeHostNetworkRecords(oldPod, ipToPod, deleter)
		if newPod.Status.PodIP != "" {
			ipToPod.Store(newPod.Status.PodIP, newPod.Name)
		}
	} else if !oldPod.Spec.HostNetwork && newPod.Spec.HostNetwork {
		// Case 3: The oldPod was not using the host network, but the newPod is
		// Here we remove the old PodIP record and add new host network records
		if oldPod.Status.PodIP != "" {
			deleter.DeleteWithDelay(ipToPod, oldPod.Status.PodIP)
		}
		for _, port := range getHostNetworkPorts(newPod) {
			ipToPod.Store(newPod.Status.HostIP+":"+port, newPod.Name)
		}
	} else if !oldPod.Spec.HostNetwork && !newPod.Spec.HostNetwork && oldPod.Status.PodIP != newPod.Status.PodIP {
		// Case 4: Both oldPod and newPod are not using the host network, but the Pod IPs are different
		// Here we replace the old PodIP record with the new one
		if oldPod.Status.PodIP != "" {
			deleter.DeleteWithDelay(ipToPod, oldPod.Status.PodIP)
		}
		if newPod.Status.PodIP != "" {
			ipToPod.Store(newPod.Status.PodIP, newPod.Name)
		}
	}
}

func onAddOrUpdatePod(newObj, oldObj interface{}, ipToPod, podToWorkloadAndNamespace, workloadAndNamespaceToLabels *sync.Map, workloadPodCount map[string]int, isAdd bool, logger *zap.Logger, deleter Deleter) {
	pod := newObj.(*corev1.Pod)

	if isAdd {
		handlePodAdd(pod, ipToPod)
	} else {
		oldPod := oldObj.(*corev1.Pod)
		handlePodUpdate(pod, oldPod, ipToPod, deleter)
	}

	workloadAndNamespace := getWorkloadAndNamespace(pod)

	if workloadAndNamespace != "" {
		podToWorkloadAndNamespace.Store(pod.Name, workloadAndNamespace)
		podLabels := mapset.NewSet[string]()
		for key, value := range pod.ObjectMeta.Labels {
			podLabels.Add(key + "=" + value)
		}
		if podLabels.Cardinality() > 0 {
			workloadAndNamespaceToLabels.Store(workloadAndNamespace, podLabels)
		}
		if isAdd {
			workloadPodCount[workloadAndNamespace]++
			logger.Debug("Added pod", zap.String("pod", pod.Name), zap.String("workload", workloadAndNamespace), zap.Int("count", workloadPodCount[workloadAndNamespace]))
		}
	}
}

func onDeletePod(obj interface{}, ipToPod, podToWorkloadAndNamespace, workloadAndNamespaceToLabels *sync.Map, workloadPodCount map[string]int, logger *zap.Logger, deleter Deleter) {
	pod := obj.(*corev1.Pod)
	if pod.Status.PodIP != "" {
		deleter.DeleteWithDelay(ipToPod, pod.Status.PodIP)
	} else if pod.Status.HostIP != "" {
		for _, port := range getHostNetworkPorts(pod) {
			deleter.DeleteWithDelay(ipToPod, pod.Status.HostIP+":"+port)
		}
	}

	if workloadKey, ok := podToWorkloadAndNamespace.Load(pod.Name); ok {
		workloadAndNamespace := workloadKey.(string)
		workloadPodCount[workloadAndNamespace]--
		logger.Debug("workload pod count", zap.String("workload", workloadAndNamespace), zap.Int("podCount", workloadPodCount[workloadAndNamespace]))
		if workloadPodCount[workloadAndNamespace] == 0 {
			deleter.DeleteWithDelay(workloadAndNamespaceToLabels, workloadAndNamespace)
		}
	}
	deleter.DeleteWithDelay(podToWorkloadAndNamespace, pod.Name)
}

type PodWatcher struct {
	ipToPod                      *sync.Map
	podToWorkloadAndNamespace    *sync.Map
	workloadAndNamespaceToLabels *sync.Map
	workloadPodCount             map[string]int
	logger                       *zap.Logger
	informer                     cache.SharedIndexInformer
	deleter                      Deleter
}

func NewPodWatcher(logger *zap.Logger, informer cache.SharedIndexInformer, deleter Deleter) *PodWatcher {
	return &PodWatcher{
		ipToPod:                      &sync.Map{},
		podToWorkloadAndNamespace:    &sync.Map{},
		workloadAndNamespaceToLabels: &sync.Map{},
		workloadPodCount:             make(map[string]int),
		logger:                       logger,
		informer:                     informer,
		deleter:                      deleter,
	}
}

func (p *PodWatcher) Run(stopCh chan struct{}) {
	p.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			p.logger.Debug("list and watch for pods: ADD")
			onAddOrUpdatePod(obj, nil, p.ipToPod, p.podToWorkloadAndNamespace, p.workloadAndNamespaceToLabels, p.workloadPodCount, true, p.logger, p.deleter)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			p.logger.Debug("list and watch for pods: UPDATE")
			onAddOrUpdatePod(newObj, oldObj, p.ipToPod, p.podToWorkloadAndNamespace, p.workloadAndNamespaceToLabels, p.workloadPodCount, false, p.logger, p.deleter)
		},
		DeleteFunc: func(obj interface{}) {
			p.logger.Debug("list and watch for pods: DELETE")
			onDeletePod(obj, p.ipToPod, p.podToWorkloadAndNamespace, p.workloadAndNamespaceToLabels, p.workloadPodCount, p.logger, p.deleter)
		},
	})

	go p.informer.Run(stopCh)

}

func (p *PodWatcher) WaitForCacheSync(stopCh chan struct{}) {
	if !cache.WaitForNamedCacheSync("podWatcher", stopCh, p.informer.HasSynced) {
		p.logger.Fatal("timed out waiting for kubernetes pod watcher caches to sync")
	}

	p.logger.Info("PodWatcher: Cache synced")
}

type ServiceWatcher struct {
	ipToServiceAndNamespace        *sync.Map
	serviceAndNamespaceToSelectors *sync.Map
	logger                         *zap.Logger
	informer                       cache.SharedIndexInformer
	deleter                        Deleter
}

func NewServiceWatcher(logger *zap.Logger, informer cache.SharedIndexInformer, deleter Deleter) *ServiceWatcher {
	return &ServiceWatcher{
		ipToServiceAndNamespace:        &sync.Map{},
		serviceAndNamespaceToSelectors: &sync.Map{},
		logger:                         logger,
		informer:                       informer,
		deleter:                        deleter,
	}
}

func (s *ServiceWatcher) Run(stopCh chan struct{}) {
	s.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			s.logger.Debug("list and watch for services: ADD")
			onAddOrUpdateService(obj, s.ipToServiceAndNamespace, s.serviceAndNamespaceToSelectors)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			s.logger.Debug("list and watch for services: UPDATE")
			onAddOrUpdateService(newObj, s.ipToServiceAndNamespace, s.serviceAndNamespaceToSelectors)
		},
		DeleteFunc: func(obj interface{}) {
			s.logger.Debug("list and watch for services: DELETE")
			onDeleteService(obj, s.ipToServiceAndNamespace, s.serviceAndNamespaceToSelectors, s.deleter)
		},
	})
	go s.informer.Run(stopCh)
}

func (s *ServiceWatcher) WaitForCacheSync(stopCh chan struct{}) {
	if !cache.WaitForNamedCacheSync("serviceWatcher", stopCh, s.informer.HasSynced) {
		s.logger.Fatal("timed out waiting for kubernetes service watcher caches to sync")
	}

	s.logger.Info("ServiceWatcher: Cache synced")
}

type ServiceToWorkloadMapper struct {
	serviceAndNamespaceToSelectors *sync.Map
	workloadAndNamespaceToLabels   *sync.Map
	serviceToWorkload              *sync.Map
	logger                         *zap.Logger
	deleter                        Deleter
}

func NewServiceToWorkloadMapper(serviceAndNamespaceToSelectors, workloadAndNamespaceToLabels, serviceToWorkload *sync.Map, logger *zap.Logger, deleter Deleter) *ServiceToWorkloadMapper {
	return &ServiceToWorkloadMapper{
		serviceAndNamespaceToSelectors: serviceAndNamespaceToSelectors,
		workloadAndNamespaceToLabels:   workloadAndNamespaceToLabels,
		serviceToWorkload:              serviceToWorkload,
		logger:                         logger,
		deleter:                        deleter,
	}
}

func (m *ServiceToWorkloadMapper) MapServiceToWorkload() {
	m.logger.Debug("Map service to workload at:", zap.Time("time", time.Now()))

	m.serviceAndNamespaceToSelectors.Range(func(key, value interface{}) bool {
		var workloads []string
		serviceAndNamespace := key.(string)
		_, serviceNamespace := extractResourceAndNamespace(serviceAndNamespace)
		serviceLabels := value.(mapset.Set[string])

		m.workloadAndNamespaceToLabels.Range(func(workloadKey, labelsValue interface{}) bool {
			labels := labelsValue.(mapset.Set[string])
			workloadAndNamespace := workloadKey.(string)
			_, workloadNamespace := extractResourceAndNamespace(workloadAndNamespace)
			if workloadNamespace == serviceNamespace && workloadNamespace != "" && serviceLabels.IsSubset(labels) {
				m.logger.Debug("Found workload for service", zap.String("service", serviceAndNamespace), zap.String("workload", workloadAndNamespace))
				workloads = append(workloads, workloadAndNamespace)
			}

			return true
		})

		if len(workloads) > 1 {
			m.logger.Info("Multiple workloads found for service. You will get unexpected results.", zap.String("service", serviceAndNamespace), zap.Strings("workloads", workloads))
		} else if len(workloads) == 1 {
			m.serviceToWorkload.Store(serviceAndNamespace, workloads[0])
		} else {
			m.logger.Debug("No workload found for service", zap.String("service", serviceAndNamespace))
			m.deleter.DeleteWithDelay(m.serviceToWorkload, serviceAndNamespace)
		}
		return true
	})
}

func (m *ServiceToWorkloadMapper) Start(stopCh chan struct{}) {
	// do the first mapping immediately
	m.MapServiceToWorkload()
	m.logger.Debug("First-time map service to workload at:", zap.Time("time", time.Now()))

	go func() {
		for {
			select {
			case <-stopCh:
				return
			case <-time.After(time.Minute + 30*time.Second):
				m.MapServiceToWorkload()
				m.logger.Debug("Map service to workload at:", zap.Time("time", time.Now()))
			}
		}
	}()
}

func getEksResolver(logger *zap.Logger) subResolver {
	once.Do(func() {
		config, err := clientcmd.BuildConfigFromFlags("", "")
		if err != nil {
			logger.Fatal("Failed to create config", zap.Error(err))
		}

		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			logger.Fatal("Failed to create eks client", zap.Error(err))
		}

		// add a time jitter of 10 seconds
		jitterSleep(10)

		sharedInformerFactory := informers.NewSharedInformerFactory(clientset, 0)
		podInformer := sharedInformerFactory.Core().V1().Pods().Informer()
		serviceInformer := sharedInformerFactory.Core().V1().Services().Informer()

		timedDeleter := &TimedDeleter{Delay: deletionDelay}
		podWatcher := NewPodWatcher(logger, podInformer, timedDeleter)
		serviceWatcher := NewServiceWatcher(logger, serviceInformer, timedDeleter)

		safeStopCh := &safeChannel{ch: make(chan struct{}), closed: false}
		// initialize the pod and service watchers for the cluster
		podWatcher.Run(safeStopCh.ch)
		serviceWatcher.Run(safeStopCh.ch)
		// wait for caches to sync (for once) so that clients knows about the pods and services in the cluster
		podWatcher.WaitForCacheSync(safeStopCh.ch)
		serviceWatcher.WaitForCacheSync(safeStopCh.ch)

		serviceToWorkload := &sync.Map{}
		serviceToWorkloadMapper := NewServiceToWorkloadMapper(serviceWatcher.serviceAndNamespaceToSelectors, podWatcher.workloadAndNamespaceToLabels, serviceToWorkload, logger, timedDeleter)
		serviceToWorkloadMapper.Start(safeStopCh.ch)

		instance = &eksResolver{
			logger:                         logger,
			clientset:                      clientset,
			ipToServiceAndNamespace:        serviceWatcher.ipToServiceAndNamespace,
			serviceAndNamespaceToSelectors: serviceWatcher.serviceAndNamespaceToSelectors,
			ipToPod:                        podWatcher.ipToPod,
			podToWorkloadAndNamespace:      podWatcher.podToWorkloadAndNamespace,
			workloadAndNamespaceToLabels:   podWatcher.workloadAndNamespaceToLabels,
			serviceToWorkload:              serviceToWorkload,
			workloadPodCount:               podWatcher.workloadPodCount,
			safeStopCh:                     safeStopCh,
		}

		go instance.debugPrint()
	})

	return instance
}

func (e *eksResolver) Stop(_ context.Context) error {
	e.safeStopCh.Close()
	return nil
}

// add a method to eksResolver
func (e *eksResolver) GetWorkloadAndNamespaceByIP(ip string) (string, string, error) {
	var workload, namespace string
	if podKey, ok := e.ipToPod.Load(ip); ok {
		pod := podKey.(string)
		if workloadKey, ok := e.podToWorkloadAndNamespace.Load(pod); ok {
			workload, namespace = extractResourceAndNamespace(workloadKey.(string))
			return workload, namespace, nil
		}
	}

	if serviceKey, ok := e.ipToServiceAndNamespace.Load(ip); ok {
		serviceAndNamespace := serviceKey.(string)
		if workloadKey, ok := e.serviceToWorkload.Load(serviceAndNamespace); ok {
			workload, namespace = extractResourceAndNamespace(workloadKey.(string))
			return workload, namespace, nil
		}
	}

	return "", "", errors.New("no EKS workload found for ip: " + ip)
}

func printSyncMap(name string, m *sync.Map, logger *zap.Logger) {
	logger.Debug("", zap.String("MapName", name))
	m.Range(func(key, value interface{}) bool {
		logger.Debug("", zap.Any("key", key), zap.Any("value", value))
		return true
	})
	logger.Debug("DEBUG ====================")
}

func (e *eksResolver) debugPrint() {
	// call some logic every 5 minutes for ever
	for {
		select {
		case <-time.After(5 * time.Minute):
			e.debug()
		case <-e.safeStopCh.ch:
			return
		}
	}
}

func (e *eksResolver) debug() {
	e.logger.Debug("start debug print")
	// print ipToServiceAndNamespace
	printSyncMap("ipToServiceAndNamespace", e.ipToServiceAndNamespace, e.logger)

	// print serviceAndNamespaceToSelectors
	printSyncMap("serviceAndNamespaceToSelectors", e.serviceAndNamespaceToSelectors, e.logger)

	// print ipToPod
	printSyncMap("ipToPod", e.ipToPod, e.logger)

	// print podToWorkloadAndNamespace
	printSyncMap("podToWorkloadAndNamespace", e.podToWorkloadAndNamespace, e.logger)

	// print workloadAndNamespaceToLabels
	printSyncMap("workloadAndNamespaceToLabels", e.workloadAndNamespaceToLabels, e.logger)

	// print serviceToWorkload
	e.logger.Debug("workload pod count", zap.Any("workloadPodCount", e.workloadPodCount))
	printSyncMap("serviceToWorkload", e.serviceToWorkload, e.logger)
	e.logger.Debug("end debug print")
}

func (e *eksResolver) Process(attributes, resourceAttributes pcommon.Map) error {
	if value, ok := attributes.Get(AttributeRemoteService); ok {
		valueStr := value.AsString()
		ipStr := ""
		if ip, _, ok := extractIPPort(valueStr); ok {
			if workload, namespace, err := e.GetWorkloadAndNamespaceByIP(valueStr); err == nil {
				attributes.PutStr(AttributeRemoteService, workload)
				attributes.PutStr(AttributeRemoteNamespace, namespace)
			} else {
				ipStr = ip
			}
		} else if isIP(valueStr) {
			ipStr = valueStr
		}

		if ipStr != "" {
			if workload, namespace, err := e.GetWorkloadAndNamespaceByIP(ipStr); err == nil {
				attributes.PutStr(AttributeRemoteService, workload)
				attributes.PutStr(AttributeRemoteNamespace, namespace)
			} else {
				e.logger.Debug("failed to Process ip", zap.String("ip", ipStr), zap.Error(err))
				attributes.PutStr(AttributeRemoteService, "UnknownRemoteService")
			}
		}
	}

	return nil
}

func isIP(ipString string) bool {
	ip := net.ParseIP(ipString)
	return ip != nil
}

const IP_PORT_PATTERN = `^(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}):(\d+)$`

var ipPortRegex = regexp.MustCompile(IP_PORT_PATTERN)

func extractIPPort(ipPort string) (string, string, bool) {
	match := ipPortRegex.MatchString(ipPort)

	if !match {
		return "", "", false
	}

	result := ipPortRegex.FindStringSubmatch(ipPort)
	if len(result) != 3 {
		return "", "", false
	}

	ip := result[1]
	port := result[2]

	return ip, port, true
}

func getHostNetworkPorts(pod *corev1.Pod) []string {
	var ports []string
	if !pod.Spec.HostNetwork {
		return ports
	}
	for _, container := range pod.Spec.Containers {
		for _, port := range container.Ports {
			if port.HostPort != 0 {
				ports = append(ports, strconv.Itoa(int(port.HostPort)))
			}
		}
	}
	return ports
}

type eksHostedInAttributeResolver struct {
	clusterName  string
	attributeMap map[string]string
}

func newEKSHostedInAttributeResolver() *eksHostedInAttributeResolver {
	return &eksHostedInAttributeResolver{
		attributeMap: map[string]string{
			semconv.AttributeK8SNamespaceName: HostedInAttributeK8SNamespace,
		},
	}
}
func (h *eksHostedInAttributeResolver) Process(attributes, resourceAttributes pcommon.Map) error {
	for attrKey, mappingKey := range h.attributeMap {
		if val, ok := resourceAttributes.Get(attrKey); ok {
			attributes.PutStr(mappingKey, val.AsString())
		}
	}

	if h.clusterName != "" {
		attributes.PutStr(HostedInAttributeClusterName, h.clusterName)
	} else {
		platform, _ := resourceAttributes.Get(semconv.AttributeCloudProvider)
		if platform.AsString() == semconv.AttributeCloudProviderAWS {
			// iterate resource attributes to find the cluster name
			resourceAttributes.Range(func(key string, value pcommon.Value) bool {
				if strings.HasPrefix(key, "ec2.tag.kubernetes.io/cluster/") && value.Type() == pcommon.ValueTypeStr && value.AsString() == "owned" {
					h.clusterName = strings.TrimPrefix(key, "ec2.tag.kubernetes.io/cluster/")
					attributes.PutStr(HostedInAttributeClusterName, h.clusterName)
					return false
				}
				return true
			})
		}
	}

	return nil
}

func (h *eksHostedInAttributeResolver) Stop(ctx context.Context) error {
	return nil
}