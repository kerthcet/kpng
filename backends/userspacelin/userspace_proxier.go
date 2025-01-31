/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package userspacelin

import (
	"fmt"
	"net"
	"reflect"

	localv1 "sigs.k8s.io/kpng/api/localv1"

	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	k8snet "k8s.io/apimachinery/pkg/util/net"

	// libcontaineruserns "github.com/opencontainers/runc/libcontainer/userns"

	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"

	// utilfeature "k8s.io/apiserver/pkg/util/feature"

	klog "k8s.io/klog/v2"

	// kubefeatures "k8s.io/kubernetes/pkg/features"
	// "k8s.io/kubernetes/pkg/proxy/config"
	"sigs.k8s.io/kpng/backends/iptables"
	iptablesutil "sigs.k8s.io/kpng/backends/iptables/util"

	utilexec "k8s.io/utils/exec"
	netutils "k8s.io/utils/net"
)

type portal struct {
	ip         net.IP
	port       int
	isExternal bool
}

// ServiceInfo contains information and state for a particular proxied service
type ServiceInfo struct {
	// Timeout is the read/write timeout (used for UDP connections)
	Timeout time.Duration
	// ActiveClients is the cache of active UDP clients being proxied by this proxy for this service
	ActiveClients *ClientCache

	isAliveAtomic           int32 // Only access this with atomic ops
	portal                  portal
	protocol                localv1.Protocol
	proxyPort               int
	socket                  ProxySocket
	nodePort                int
	loadBalancerIPs         []string
	sessionClientIPAffinity *localv1.ClientIPAffinity
	stickyMaxAgeSeconds     int
	// Deprecated, but required for back-compat (including e2e)
	externalIPs []string

	// isStartedAtomic is set to non-zero when the service's socket begins
	// accepting requests. Used in testcases. Only access this with atomic ops.
	isStartedAtomic int32
	// isFinishedAtomic is set to non-zero when the service's socket shuts
	// down. Used in testcases. Only access this with atomic ops.
	isFinishedAtomic int32
}

func (info *ServiceInfo) setStarted() {
	atomic.StoreInt32(&info.isStartedAtomic, 1)
}

func (info *ServiceInfo) IsStarted() bool {
	return atomic.LoadInt32(&info.isStartedAtomic) != 0
}

func (info *ServiceInfo) setFinished() {
	atomic.StoreInt32(&info.isFinishedAtomic, 1)
}

func (info *ServiceInfo) IsFinished() bool {
	return atomic.LoadInt32(&info.isFinishedAtomic) != 0
}

func (info *ServiceInfo) setAlive(b bool) {
	var i int32
	if b {
		i = 1
	}
	atomic.StoreInt32(&info.isAliveAtomic, i)
}

func (info *ServiceInfo) IsAlive() bool {
	return atomic.LoadInt32(&info.isAliveAtomic) != 0
}

func logTimeout(err error) bool {
	if e, ok := err.(net.Error); ok {
		if e.Timeout() {
			klog.V(3).InfoS("Connection to endpoint closed due to inactivity")
			return true
		}
	}
	return false
}

// ProxySocketFunc is a function which constructs a ProxySocket from a protocol, ip, and port
type ProxySocketFunc func(protocol localv1.Protocol, ip net.IP, port int) (ProxySocket, error)

const numBurstSyncs int = 2

// Interface for async runner; abstracted for testing
type asyncRunnerInterface interface {
	Run()
	Loop(<-chan struct{})
}

// Proxier is a simple proxy for TCP connections between a localhost:lport
// and services that provide the actual implementations.
type UserspaceLinux struct {
	// EndpointSlice support has not been added for this proxier yet.
	// onfig.NoopEndpointSliceHandler
	// TODO(imroc): implement node handler for userspace proxier.
	// config.NoopNodeHandler

	loadBalancer    LoadBalancer
	mu              sync.Mutex // protects serviceMap
	serviceMap      map[iptables.ServicePortName]*ServiceInfo
	syncPeriod      time.Duration
	minSyncPeriod   time.Duration
	udpIdleTimeout  time.Duration
	portMapMutex    sync.Mutex
	portMap         map[portMapKey]*portMapValue
	listenIP        net.IP
	iptables        iptablesutil.Interface
	hostIP          net.IP
	localAddrs      netutils.IPSet
	proxyPorts      PortAllocator
	makeProxySocket ProxySocketFunc
	exec            utilexec.Interface
	// endpointsSynced and servicesSynced are set to 1 when the corresponding
	// objects are synced after startup. This is used to avoid updating iptables
	// with some partial data after kube-proxy restart.
	endpointsSynced int32
	servicesSynced  int32
	initialized     int32
	// protects serviceChanges
	serviceChangesLock sync.Mutex
	serviceChanges     map[types.NamespacedName]*UserspaceServiceChangeTracker // map of service changes, this is the entire state-space of all services in k8s.
	syncRunner         asyncRunnerInterface                                    // governs calls to syncProxyRules

	stopChan chan struct{}
}

// A key for the portMap.  The ip has to be a string because slices can't be map
// keys.
type portMapKey struct {
	ip       string
	port     int
	protocol localv1.Protocol
}

func (k *portMapKey) String() string {
	return fmt.Sprintf("%s/%s", net.JoinHostPort(k.ip, strconv.Itoa(k.port)), k.protocol)
}

// A value for the portMap
type portMapValue struct {
	owner  iptables.ServicePortName
	socket interface {
		Close() error
	}
}

var (
	// ErrProxyOnLocalhost is returned by NewProxier if the user requests a proxier on
	// the loopback address. May be checked for by callers of NewProxier to know whether
	// the caller provided invalid input.
	ErrProxyOnLocalhost = fmt.Errorf("cannot proxy on localhost")
)

// NewProxier returns a new Proxier given a LoadBalancer and an address on
// which to listen.  Because of the iptables logic, It is assumed that there
// is only a single Proxier active on a machine. An error will be returned if
// the proxier cannot be started due to an invalid ListenIP (loopback) or
// if iptables fails to update or acquire the initial lock. Once a proxier is
// created, it will keep iptables up to date in the background and will not
// terminate if a particular iptables call fails.

func NewUserspaceLinux(loadBalancer LoadBalancer, listenIP net.IP, iptables iptablesutil.Interface, exec utilexec.Interface, pr utilnet.PortRange, syncPeriod, minSyncPeriod, udpIdleTimeout time.Duration) (*UserspaceLinux, error) {
	return NewCustomProxier(loadBalancer, listenIP, iptables, exec, pr, syncPeriod, minSyncPeriod, udpIdleTimeout, newProxySocket)
}

// NewCustomProxier functions similarly to NewProxier, returning a new Proxier
// for the given LoadBalancer and address.  The new proxier is constructed using
// the ProxySocket constructor provided, however, instead of constructing the
// default ProxySockets.
func NewCustomProxier(loadBalancer LoadBalancer, listenIP net.IP, iptables iptablesutil.Interface, exec utilexec.Interface, pr utilnet.PortRange, syncPeriod, minSyncPeriod, udpIdleTimeout time.Duration, makeProxySocket ProxySocketFunc) (*UserspaceLinux, error) {

	// If listenIP is given, assume that is the intended host IP.  Otherwise
	// try to find a suitable host IP address from network interfaces.
	var err error
	hostIP, err := utilnet.ChooseHostInterface()

	err = setRLimit(64 * 1000)
	if err != nil {

		// TODO @jayunit100 enable this once we bump to 1.22
		/**
		if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.KubeletInUserNamespace) && libcontaineruserns.RunningInUserNS() {
		} else {
			return nil, fmt.Errorf("failed to set open file handler limit to 64000: %w", err)
		}
		*/
		klog.V(2).InfoS("Failed to set open file handler limit to 64000 (running in UserNS, ignoring)", "err", err)
	}

	// make a dummy port range, newPortAllocator will make one for us w/ defaults
	proxyPorts := newPortAllocator(k8snet.PortRange{})

	klog.V(2).InfoS("Setting proxy IP and initializing iptables", "ip", hostIP)

	// ... finish implementing these functions ...
	return createProxier(loadBalancer, hostIP, iptables, exec, hostIP, proxyPorts, syncPeriod, minSyncPeriod, udpIdleTimeout, makeProxySocket)
}

// createProxier makes a userspace proxier.  It does some iptables actions but it doesn't actually run iptables AS the proxy.
func createProxier(loadBalancer LoadBalancer, listenIP net.IP, iptablesInterfaceImpl iptablesutil.Interface, exec utilexec.Interface, hostIP net.IP, proxyPorts PortAllocator, syncPeriod, minSyncPeriod, udpIdleTimeout time.Duration, makeProxySocket ProxySocketFunc) (*UserspaceLinux, error) {
	// Hack: since the userspace proxy is old, we don't expect people to need to replace this loadbalancer. so we hardcode it to round_robin.go.

	// convenient to pass nil for tests..
	if proxyPorts == nil {
		proxyPorts = newPortAllocator(utilnet.PortRange{})
	}
	// Set up the iptables foundations we need.
	if err := iptablesInit(iptablesInterfaceImpl); err != nil {
		return nil, fmt.Errorf("failed to initialize iptables: %v", err)
	}
	// Flush old iptables rules (since the bound ports will be invalid after a restart).
	// When OnUpdate() is first called, the rules will be recreated.
	if err := iptablesFlush(iptablesInterfaceImpl); err != nil {
		return nil, fmt.Errorf("failed to flush iptables: %v", err)
	}
	proxier := &UserspaceLinux{
		loadBalancer:    loadBalancer, // <----
		serviceMap:      make(map[iptables.ServicePortName]*ServiceInfo),
		serviceChanges:  make(map[types.NamespacedName]*UserspaceServiceChangeTracker),
		portMap:         make(map[portMapKey]*portMapValue),
		syncPeriod:      syncPeriod,
		minSyncPeriod:   minSyncPeriod,
		udpIdleTimeout:  udpIdleTimeout,
		listenIP:        listenIP,
		iptables:        iptablesInterfaceImpl,
		hostIP:          hostIP,
		proxyPorts:      proxyPorts,
		makeProxySocket: makeProxySocket,
		exec:            exec,
		stopChan:        make(chan struct{}),
	}
	klog.V(3).InfoS("Record sync param", "minSyncPeriod", minSyncPeriod, "syncPeriod", syncPeriod, "burstSyncs", numBurstSyncs)
	proxier.syncRunner = newBoundedFrequencyRunner("userspace-proxy-sync-runner", proxier.syncProxyRules, minSyncPeriod, syncPeriod, numBurstSyncs)
	return proxier, nil
}

// CleanupLeftovers removes all iptables rules and chains created by the Proxier
// It returns true if an error was encountered. Errors are logged.
func CleanupLeftovers(ipt iptablesutil.Interface) (encounteredError bool) {
	// NOTE: Warning, this needs to be kept in sync with the userspace Proxier,
	// we want to ensure we remove all of the iptables rules it creates.
	// Currently they are all in iptablesInit()
	// Delete Rules first, then Flush and Delete Chains
	args := []string{"-m", "comment", "--comment", "handle ClusterIPs; NOTE: this must be before the NodePort rules"}
	if err := ipt.DeleteRule(iptablesutil.TableNAT, iptablesutil.ChainOutput, append(args, "-j", string(iptablesHostPortalChain))...); err != nil {
		if !iptablesutil.IsNotFoundError(err) {
			klog.ErrorS(err, "Error removing userspace rule")
			encounteredError = true
		}
	}
	if err := ipt.DeleteRule(iptablesutil.TableNAT, iptablesutil.ChainPrerouting, append(args, "-j", string(iptablesContainerPortalChain))...); err != nil {
		if !iptablesutil.IsNotFoundError(err) {
			klog.ErrorS(err, "Error removing userspace rule")
			encounteredError = true
		}
	}
	args = []string{"-m", "addrtype", "--dst-type", "LOCAL"}
	args = append(args, "-m", "comment", "--comment", "handle service NodePorts; NOTE: this must be the last rule in the chain")
	if err := ipt.DeleteRule(iptablesutil.TableNAT, iptablesutil.ChainOutput, append(args, "-j", string(iptablesHostNodePortChain))...); err != nil {
		if !iptablesutil.IsNotFoundError(err) {
			klog.ErrorS(err, "Error removing userspace rule")
			encounteredError = true
		}
	}
	if err := ipt.DeleteRule(iptablesutil.TableNAT, iptablesutil.ChainPrerouting, append(args, "-j", string(iptablesContainerNodePortChain))...); err != nil {
		if !iptablesutil.IsNotFoundError(err) {
			klog.ErrorS(err, "Error removing userspace rule")
			encounteredError = true
		}
	}
	args = []string{"-m", "comment", "--comment", "Ensure that non-local NodePort traffic can flow"}
	if err := ipt.DeleteRule(iptablesutil.TableFilter, iptablesutil.ChainInput, append(args, "-j", string(iptablesNonLocalNodePortChain))...); err != nil {
		if !iptablesutil.IsNotFoundError(err) {
			klog.ErrorS(err, "Error removing userspace rule")
			encounteredError = true
		}
	}

	// flush and delete chains.
	tableChains := map[iptablesutil.Table][]iptablesutil.Chain{
		iptablesutil.TableNAT:    {iptablesContainerPortalChain, iptablesHostPortalChain, iptablesHostNodePortChain, iptablesContainerNodePortChain},
		iptablesutil.TableFilter: {iptablesNonLocalNodePortChain},
	}
	for table, chains := range tableChains {
		for _, c := range chains {
			// flush chain, then if successful delete, delete will fail if flush fails.
			if err := ipt.FlushChain(table, c); err != nil {
				if !iptablesutil.IsNotFoundError(err) {
					klog.ErrorS(err, "Error flushing userspace chain")
					encounteredError = true
				}
			} else {
				if err = ipt.DeleteChain(table, c); err != nil {
					if !iptablesutil.IsNotFoundError(err) {
						klog.ErrorS(err, "Error deleting userspace chain")
						encounteredError = true
					}
				}
			}
		}
	}
	return encounteredError
}

// shutdown closes all service port proxies and returns from the proxy's
// sync loop. Used from testcases.
func (proxier *UserspaceLinux) shutdown() {
	proxier.mu.Lock()
	defer proxier.mu.Unlock()

	for serviceName, info := range proxier.serviceMap {
		proxier.stopProxy(serviceName, info)
	}
	proxier.cleanupStaleStickySessions()
	close(proxier.stopChan)
}

func (proxier *UserspaceLinux) isInitialized() bool {
	return atomic.LoadInt32(&proxier.initialized) > 0
}

// Sync is called to synchronize the proxier state to iptables as soon as possible.
func (proxier *UserspaceLinux) Sync() {
	proxier.syncRunner.Run()
}

// this is called sync() in iptables, omg vivek
func (proxier *UserspaceLinux) syncProxyRules() {
	start := time.Now()
	defer func() {
		klog.V(4).InfoS("Userspace syncProxyRules complete", "elapsed", time.Since(start))
	}()

	// don't sync rules till we've received services and endpoints
	if !proxier.isInitialized() {
		klog.V(2).InfoS("Not syncing userspace proxy until Services and Endpoints have been received from master")
		return
	}

	if err := iptablesInit(proxier.iptables); err != nil {
		klog.ErrorS(err, "Failed to ensure iptables")
	}

	// ... we can remove these locks bc kpng runs synchronous streams to update things ...
	proxier.serviceChangesLock.Lock()
	oldChanges := proxier.serviceChanges

	// make the "current" service changes a new map and rebuild it...
	proxier.serviceChanges = make(map[types.NamespacedName]*UserspaceServiceChangeTracker)
	proxier.serviceChangesLock.Unlock()

	proxier.mu.Lock()
	defer proxier.mu.Unlock()

	klog.V(4).InfoS("userspace proxy: processing service events", "count", len(oldChanges))
	for _, oldChange := range oldChanges {
		for _, svcChange := range oldChange.items {
			existingPorts := proxier.mergeService(svcChange.current)
			proxier.unmergeService(svcChange.previous, existingPorts)
		}
	}

	proxier.localAddrs = GetLocalAddrSet()

	proxier.ensurePortals()
	proxier.cleanupStaleStickySessions()
}

// SyncLoop runs periodic work.  This is expected to run as a goroutine or as the main loop of the app.  It does not return.
func (proxier *UserspaceLinux) SyncLoop() {
	proxier.syncRunner.Loop(proxier.stopChan)
}

// Ensure that portals exist for all services.
func (proxier *UserspaceLinux) ensurePortals() {
	// NB: This does not remove rules that should not be present.
	for name, info := range proxier.serviceMap {
		err := proxier.openPortal(name, info)
		if err != nil {
			klog.ErrorS(err, "Failed to ensure portal", "servicePortName", name)
		}
	}
}

// clean up any stale sticky session records in the hash map.
func (proxier *UserspaceLinux) cleanupStaleStickySessions() {
	for name := range proxier.serviceMap {
		proxier.loadBalancer.CleanupStaleStickySessions(name)
	}
}

func (proxier *UserspaceLinux) stopProxy(service iptables.ServicePortName, info *ServiceInfo) error {
	delete(proxier.serviceMap, service)
	info.setAlive(false)
	err := info.socket.Close()
	port := info.socket.ListenPort()
	proxier.proxyPorts.Release(port)
	return err
}

func (proxier *UserspaceLinux) getServiceInfo(service iptables.ServicePortName) (*ServiceInfo, bool) {
	proxier.mu.Lock()
	defer proxier.mu.Unlock()
	info, ok := proxier.serviceMap[service]
	return info, ok
}

// addServiceOnPortInternal starts listening for a new service, returning the ServiceInfo.
// Pass proxyPort=0 to allocate a random port. The timeout only applies to UDP
// connections, for now.
func (proxier *UserspaceLinux) addServiceOnPortInternal(service iptables.ServicePortName, protocol localv1.Protocol, proxyPort int, timeout time.Duration) (*ServiceInfo, error) {
	sock, err := proxier.makeProxySocket(protocol, proxier.listenIP, proxyPort)
	if err != nil {
		return nil, err
	}
	_, portStr, err := net.SplitHostPort(sock.Addr().String())
	if err != nil {
		sock.Close()
		return nil, err
	}
	portNum, err := strconv.Atoi(portStr)
	if err != nil {
		sock.Close()
		return nil, err
	}
	si := &ServiceInfo{
		Timeout:                 timeout,
		ActiveClients:           newClientCache(),
		isAliveAtomic:           1,
		proxyPort:               portNum,
		protocol:                protocol,
		socket:                  sock,
		sessionClientIPAffinity: nil, // default
	}
	proxier.serviceMap[service] = si

	klog.V(2).InfoS("Proxying for service", "service", service, "protocol", protocol, "portNum", portNum)
	go func() {
		defer runtime.HandleCrash()
		sock.ProxyLoop(service, si, proxier.loadBalancer)
	}()

	return si, nil
}

func (proxier *UserspaceLinux) cleanupPortalAndProxy(serviceName iptables.ServicePortName, info *ServiceInfo) error {
	if err := proxier.closePortal(serviceName, info); err != nil {
		return fmt.Errorf("Failed to close portal for %q: %v", serviceName, err)
	}
	if err := proxier.stopProxy(serviceName, info); err != nil {
		return fmt.Errorf("Failed to stop service %q: %v", serviceName, err)
	}
	return nil
}

func (proxier *UserspaceLinux) mergeService(service *localv1.Service) sets.String {
	if service == nil {
		return nil
	}
	if ShouldSkipService(service) {
		return nil
	}
	existingPorts := sets.NewString()
	svcName := types.NamespacedName{Namespace: service.Namespace, Name: service.Name}
	for i := range service.Ports {
		//TODO print Ports
		servicePort := &service.Ports[i]
		//TODO print servicePort
		serviceName := iptables.ServicePortName{NamespacedName: svcName, Port: (*servicePort).Name}
		existingPorts.Insert((*servicePort).Name)
		info, exists := proxier.serviceMap[serviceName]
		// TODO: check health of the socket? What if ProxyLoop exited?
		if exists && sameConfig(info, service, *servicePort) {
			// Nothing changed.
			continue
		}
		if exists {
			klog.V(4).InfoS("Something changed for service: stopping it", "serviceName", serviceName)
			if err := proxier.cleanupPortalAndProxy(serviceName, info); err != nil {
				klog.ErrorS(err, "Failed to cleanup portal and proxy")
			}
			info.setFinished()
		}
		proxyPort, err := proxier.proxyPorts.AllocateNext()
		if err != nil {
			klog.ErrorS(err, "Failed to allocate proxy port", "serviceName", serviceName)
			continue
		}

		serviceIP := net.ParseIP(service.IPs.ClusterIPs.V4[0])
		klog.V(0).InfoS("Adding new service", "serviceName", serviceName, "addr", net.JoinHostPort(serviceIP.String(), strconv.Itoa(int((*servicePort).Port))), "protocol", (*servicePort).Protocol)
		info, err = proxier.addServiceOnPortInternal(serviceName, (*servicePort).Protocol, proxyPort, proxier.udpIdleTimeout)
		if err != nil {
			klog.ErrorS(err, "Failed to start proxy", "serviceName", serviceName)
			continue
		}
		info.portal.ip = serviceIP
		info.portal.port = int((*servicePort).Port)
		info.externalIPs = service.GetIPs().ExternalIPs.GetV4()
		info.loadBalancerIPs = service.GetIPs().LoadBalancerIPs.GetV4()
		info.nodePort = int((*servicePort).GetNodePort())
		// info.affinityClientIP = service.GetClientIP()
		// Deep-copy in case the service instance changes
		/**
			ClusterIPs  *IPSet `protobuf:"bytes,1,opt,name=ClusterIPs,proto3" json:"ClusterIPs,omitempty"`
			ExternalIPs *IPSet `protobuf:"bytes,2,opt,name=ExternalIPs,proto3" json:"ExternalIPs,omitempty"`
			Headless    bool   `protobuf:"varint,3,opt,name=Headless,proto3" json:"Headless,omitempty"`
		}

				// TODO sessionAffinity
				info.sessionAffinityType = service.SessionAffinity
				// Kube-apiserver side guarantees SessionAffinityConfig won't be nil when session affinity type is ClientIP
				if service.SessionAffinity == v1.ServiceAffinityClientIP {
					info.stickyMaxAgeSeconds = int(*service.SessionAffinityConfig.ClientIP.TimeoutSeconds)
				}
		**/
		if service.SessionAffinity != nil {
			info.stickyMaxAgeSeconds = int(service.GetClientIP().TimeoutSeconds)
		}
		klog.V(0).InfoS("Record serviceInfo", "serviceInfo", info)

		if err := proxier.openPortal(serviceName, info); err != nil {
			klog.ErrorS(err, "Failed to open portal", "serviceName", serviceName)
		}
		proxier.loadBalancer.NewService(serviceName, service.GetClientIP(), info.stickyMaxAgeSeconds)

		info.setStarted()
	}

	return existingPorts
}

func (proxier *UserspaceLinux) unmergeService(service *localv1.Service, existingPorts sets.String) {
	if service == nil {
		return
	}

	if ShouldSkipService(service) {
		return
	}
	staleUDPServices := sets.NewString()
	svcName := types.NamespacedName{Namespace: service.Namespace, Name: service.Name}
	for i := range service.Ports {
		servicePort := &service.Ports[i]
		if existingPorts.Has((*servicePort).Name) {
			continue
		}
		serviceName := iptables.ServicePortName{NamespacedName: svcName, Port: (*servicePort).Name}

		klog.V(1).InfoS("Stopping service", "serviceName", serviceName)
		info, exists := proxier.serviceMap[serviceName]
		if !exists {
			klog.ErrorS(nil, "Service is being removed but doesn't exist", "serviceName", serviceName)
			continue
		}

		if proxier.serviceMap[serviceName].protocol == localv1.Protocol_UDP {
			staleUDPServices.Insert(proxier.serviceMap[serviceName].portal.ip.String())
		}

		if err := proxier.cleanupPortalAndProxy(serviceName, info); err != nil {
			klog.ErrorS(err, "Clean up portal and proxy")
		}
		proxier.loadBalancer.DeleteService(serviceName)
		info.setFinished()
	}
	// for _, svcIP := range staleUDPServices.UnsortedList() {
	// 	if err := conntrack.ClearEntriesForIP(proxier.exec, svcIP, kpng.ProtocolUDP); err != nil {
	// 		klog.ErrorS(err, "Failed to delete stale service IP connections", "ip", svcIP)
	// 	}
	// }
}

func (proxier *UserspaceLinux) serviceChange(previous, current *localv1.Service, detail string) {
	var svcName types.NamespacedName
	if current != nil {
		svcName = types.NamespacedName{Namespace: current.Namespace, Name: current.Name}
	} else {
		svcName = types.NamespacedName{Namespace: previous.Namespace, Name: previous.Name}
	}
	klog.V(0).InfoS("Record service change", "action", detail, "svcName", svcName)

	proxier.serviceChangesLock.Lock()
	defer proxier.serviceChangesLock.Unlock()

	change, exists := proxier.serviceChanges[svcName]
	if !exists {
		// change.previous is only set for new changes. We must keep
		// the oldest service info (or nil) because correct unmerging
		// depends on the next update/del after a merge, not subsequent
		// updates.
		change = &UserspaceServiceChangeTracker{items: map[types.NamespacedName]*userspaceServiceChange{svcName: &userspaceServiceChange{previous: previous}}}
		proxier.serviceChanges[svcName] = change
	}

	// Always use the most current service (or nil) as change.current
	change.items[svcName].current = current

	if reflect.DeepEqual(change.items[svcName].previous, change.items[svcName].current) {
		// collapsed change had no effect
		delete(proxier.serviceChanges, svcName)
	} else if proxier.isInitialized() {
		// change will have an effect, ask the proxy to sync
		proxier.syncRunner.Run()
	}
}

// OnServiceAdd is called whenever creation of new service object
// is observed.
func (proxier *UserspaceLinux) OnServiceAdd(service *localv1.Service) {
	atomic.StoreInt32(&proxier.servicesSynced, 1)
	if atomic.LoadInt32(&proxier.endpointsSynced) > 0 {
		atomic.StoreInt32(&proxier.initialized, 1)
	}
	proxier.serviceChange(nil, service, "OnServiceAdd")
	//_ = proxier.mergeService(service)
}

// OnServiceUpdate is called whenever modification of an existing
// service object is observed.
func (proxier *UserspaceLinux) OnServiceUpdate(oldService, service *localv1.Service) {
	proxier.serviceChange(oldService, service, "OnServiceUpdate")
	//existingPorts := proxier.mergeService(service)
	//proxier.unmergeService(oldService, existingPorts)
}

// OnServiceDelete is called whenever deletion of an existing service
// object is observed.
func (proxier *UserspaceLinux) OnServiceDelete(service *localv1.Service) {
	proxier.serviceChange(service, nil, "OnServiceDelete")
	//proxier.unmergeService(service, sets.NewString())
}

// OnServiceSynced is called once all the initial event handlers were
// called and the state is fully propagated to local cache.
func (proxier *UserspaceLinux) OnServiceSynced() {
	klog.V(2).InfoS("Userspace OnServiceSynced")

	// Mark services as initialized and (if endpoints are already
	// initialized) the entire proxy as initialized
	atomic.StoreInt32(&proxier.servicesSynced, 1)
	if atomic.LoadInt32(&proxier.endpointsSynced) > 0 {
		atomic.StoreInt32(&proxier.initialized, 1)
	}

	// Must sync from a goroutine to avoid blocking the
	// service event handler on startup with large numbers
	// of initial objects
	go proxier.syncProxyRules()
}

// OnEndpointsAdd is called whenever creation of new endpoints object
// is observed.
func (proxier *UserspaceLinux) OnEndpointsAdd(ep *localv1.Endpoint, svc *localv1.Service) {
	atomic.StoreInt32(&proxier.endpointsSynced, 1)
	if atomic.LoadInt32(&proxier.servicesSynced) > 0 {
		atomic.StoreInt32(&proxier.initialized, 1)
	}

	proxier.loadBalancer.OnEndpointsAdd(ep, svc)
}

// OnEndpointsUpdate is called whenever modification of an existing
// endpoints object is observed.
func (proxier *UserspaceLinux) OnEndpointsUpdate(oldEndpoints, endpoints *localv1.Endpoint) {
	//	proxier.loadBalancer.OnEndpointsUpdate(oldEndpoints, endpoints)
}

// OnEndpointsDelete is called whenever deletion of an existing endpoints
// object is observed.
func (proxier *UserspaceLinux) OnEndpointsDelete(ep *localv1.Endpoint, svc *localv1.Service) {
	proxier.loadBalancer.OnEndpointsDelete(ep, svc)
}

// OnEndpointsSynced is called once all the initial event handlers were
// called and the state is fully propagated to local cache.
func (proxier *UserspaceLinux) OnEndpointsSynced() {
	klog.V(2).InfoS("Userspace OnEndpointsSynced")
	proxier.loadBalancer.OnEndpointsSynced()

	// Mark endpoints as initialized and (if services are already
	// initialized) the entire proxy as initialized
	atomic.StoreInt32(&proxier.endpointsSynced, 1)
	if atomic.LoadInt32(&proxier.servicesSynced) > 0 {
		atomic.StoreInt32(&proxier.initialized, 1)
	}

	// Must sync from a goroutine to avoid blocking the
	// service event handler on startup with large numbers
	// of initial objects
	go proxier.syncProxyRules()
}

// TODO do we need portmapping?
func sameConfig(info *ServiceInfo, service *localv1.Service, port *localv1.PortMapping) bool {
	pr := localv1.Protocol(info.protocol)

	if pr != localv1.Protocol(port.Protocol) || info.portal.port != int(port.Port) || info.nodePort != int(port.NodePort) {
		return false
	}
	if !info.portal.ip.Equal(net.ParseIP(service.IPs.ClusterIPs.V4[0])) {
		return false
	}
	if !ipsEqual(info.externalIPs, service.IPs.ExternalIPs.V4) {
		return false
	}

	// TODO. build this loadBalancerStatus up properly.
	// loadBalancerStatus := v1.LoadBalancerStatus{}
	// if !servicehelper.LoadBalancerStatusEqual(&info.loadBalancerStatus, &loadBalancerStatus) {
	// 	return false
	// }

	// TODO add Session AFfinity to KPNG
	// if info.sessionAffinityType != service.Spec.SessionAffinity {
	//	return false
	//}
	return true
}

func ipsEqual(lhs, rhs []string) bool {
	if len(lhs) != len(rhs) {
		return false
	}
	for i := range lhs {
		if lhs[i] != rhs[i] {
			return false
		}
	}
	return true
}

func (proxier *UserspaceLinux) openPortal(service iptables.ServicePortName, info *ServiceInfo) error {
	err := proxier.openOnePortal(info.portal, info.protocol, proxier.listenIP, info.proxyPort, service)
	if err != nil {
		return err
	}
	for _, publicIP := range info.externalIPs {
		err = proxier.openOnePortal(portal{net.ParseIP(publicIP), info.portal.port, true}, info.protocol, proxier.listenIP, info.proxyPort, service)
		if err != nil {
			return err
		}
	}
	for _, ingress := range info.loadBalancerIPs {
		if ingress != "" {
			err = proxier.openOnePortal(portal{net.ParseIP(ingress), info.portal.port, false}, info.protocol, proxier.listenIP, info.proxyPort, service)
			if err != nil {
				return err
			}
		}
	}
	if info.nodePort != 0 {
		//TODO Add log here
		err = proxier.openNodePort(info.nodePort, info.protocol, proxier.listenIP, info.proxyPort, service)
		if err != nil {
			return err
		}
	}
	return nil
}

func (proxier *UserspaceLinux) openOnePortal(portal portal, protocol localv1.Protocol, proxyIP net.IP, proxyPort int, name iptables.ServicePortName) error {
	if proxier.localAddrs.Has(portal.ip) {
		err := proxier.claimNodePort(portal.ip, portal.port, protocol, name)
		if err != nil {
			return err
		}
	}

	// Handle traffic from containers.
	args := proxier.iptablesContainerPortalArgs(portal.ip, portal.isExternal, false, portal.port, protocol, proxyIP, proxyPort, name)
	portalAddress := net.JoinHostPort(portal.ip.String(), strconv.Itoa(portal.port))
	existed, err := proxier.iptables.EnsureRule(iptablesutil.Append, iptablesutil.TableNAT, iptablesContainerPortalChain, args...)
	if err != nil {
		klog.ErrorS(err, "Failed to install iptables rule for service", "chain", iptablesContainerPortalChain, "servicePortName", name, "args", args)
		return err
	}
	if !existed {
		klog.V(3).InfoS("Opened iptables from-containers portal for service", "servicePortName", name, "protocol", protocol, "portalAddress", portalAddress)
	}
	if portal.isExternal {
		args := proxier.iptablesContainerPortalArgs(portal.ip, false, true, portal.port, protocol, proxyIP, proxyPort, name)
		existed, err := proxier.iptables.EnsureRule(iptablesutil.Append, iptablesutil.TableNAT, iptablesContainerPortalChain, args...)
		if err != nil {
			klog.ErrorS(err, "Failed to install iptables rule that opens service for local traffic", "chain", iptablesContainerPortalChain, "servicePortName", name, "args", args)
			return err
		}
		if !existed {
			klog.V(3).InfoS("Opened iptables from-containers portal for service for local traffic", "servicePortName", name, "protocol", protocol, "portalAddress", portalAddress)
		}

		args = proxier.iptablesHostPortalArgs(portal.ip, true, portal.port, protocol, proxyIP, proxyPort, name)
		existed, err = proxier.iptables.EnsureRule(iptablesutil.Append, iptablesutil.TableNAT, iptablesHostPortalChain, args...)
		if err != nil {
			klog.ErrorS(err, "Failed to install iptables rule for service for dst-local traffic", "chain", iptablesHostPortalChain, "servicePortName", name)
			return err
		}
		if !existed {
			klog.V(3).InfoS("Opened iptables from-host portal for service for dst-local traffic", "servicePortName", name, "protocol", protocol, "portalAddress", portalAddress)
		}
		return nil
	}

	// Handle traffic from the host.
	args = proxier.iptablesHostPortalArgs(portal.ip, false, portal.port, protocol, proxyIP, proxyPort, name)
	existed, err = proxier.iptables.EnsureRule(iptablesutil.Append, iptablesutil.TableNAT, iptablesHostPortalChain, args...)
	if err != nil {
		klog.ErrorS(err, "Failed to install iptables rule for service", "chain", iptablesHostPortalChain, "servicePortName", name)
		return err
	}
	if !existed {
		klog.V(3).InfoS("Opened iptables from-host portal for service", "servicePortName", name, "protocol", protocol, "portalAddress", portalAddress)
	}
	return nil
}

// Marks a port as being owned by a particular service, or returns error if already claimed.
// Idempotent: reclaiming with the same owner is not an error
func (proxier *UserspaceLinux) claimNodePort(ip net.IP, port int, protocol localv1.Protocol, owner iptables.ServicePortName) error {
	proxier.portMapMutex.Lock()
	defer proxier.portMapMutex.Unlock()

	// TODO: We could pre-populate some reserved ports into portMap and/or blacklist some well-known ports

	key := portMapKey{ip: ip.String(), port: port, protocol: protocol}
	existing, found := proxier.portMap[key]
	if !found {
		// Hold the actual port open, even though we use iptables to redirect
		// it.  This ensures that a) it's safe to take and b) that stays true.
		// NOTE: We should not need to have a real listen()ing socket - bind()
		// should be enough, but I can't figure out a way to e2e test without
		// it.  Tools like 'ss' and 'netstat' do not show sockets that are
		// bind()ed but not listen()ed, and at least the default debian netcat
		// has no way to avoid about 10 seconds of retries.
		socket, err := proxier.makeProxySocket(protocol, ip, port)
		if err != nil {
			return fmt.Errorf("can't open node port for %s: %v", key.String(), err)
		}
		proxier.portMap[key] = &portMapValue{owner: owner, socket: socket}
		klog.V(2).InfoS("Claimed local port", "port", key.String())
		return nil
	}
	if existing.owner == owner {
		// We are idempotent
		return nil
	}
	return fmt.Errorf("Port conflict detected on port %s.  %v vs %v", key.String(), owner, existing)
}

// Release a claim on a port.  Returns an error if the owner does not match the claim.
// Tolerates release on an unclaimed port, to simplify .
func (proxier *UserspaceLinux) releaseNodePort(ip net.IP, port int, protocol localv1.Protocol, owner iptables.ServicePortName) error {
	proxier.portMapMutex.Lock()
	defer proxier.portMapMutex.Unlock()

	key := portMapKey{ip: ip.String(), port: port, protocol: protocol}
	existing, found := proxier.portMap[key]
	if !found {
		// We tolerate this, it happens if we are cleaning up a failed allocation
		klog.InfoS("Ignoring release on unowned port", "port", key)
		return nil
	}
	if existing.owner != owner {
		return fmt.Errorf("Port conflict detected on port %v (unowned unlock).  %v vs %v", key, owner, existing)
	}
	delete(proxier.portMap, key)
	existing.socket.Close()
	return nil
}

func (proxier *UserspaceLinux) openNodePort(nodePort int, protocol localv1.Protocol, proxyIP net.IP, proxyPort int, name iptables.ServicePortName) error {
	// TODO: Do we want to allow containers to access public services?  Probably yes.
	// TODO: We could refactor this to be the same code as portal, but with IP == nil

	err := proxier.claimNodePort(nil, nodePort, protocol, name)
	if err != nil {
		return err
	}

	// Handle traffic from containers.
	args := proxier.iptablesContainerPortalArgs(nil, false, false, nodePort, protocol, proxyIP, proxyPort, name)
	existed, err := proxier.iptables.EnsureRule(iptablesutil.Append, iptablesutil.TableNAT, iptablesContainerNodePortChain, args...)
	if err != nil {
		klog.ErrorS(err, "Failed to install iptables rule for service", "chain", iptablesContainerNodePortChain, "servicePortName", name)
		return err
	}
	if !existed {
		klog.InfoS("Opened iptables from-containers public port for service", "servicePortName", name, "protocol", protocol, "nodePort", nodePort)
	}

	// Handle traffic from the host.
	args = proxier.iptablesHostNodePortArgs(nodePort, protocol, proxyIP, proxyPort, name)
	existed, err = proxier.iptables.EnsureRule(iptablesutil.Append, iptablesutil.TableNAT, iptablesHostNodePortChain, args...)
	if err != nil {
		klog.ErrorS(err, "Failed to install iptables rule for service", "chain", iptablesHostNodePortChain, "servicePortName", name)
		return err
	}
	if !existed {
		klog.InfoS("Opened iptables from-host public port for service", "servicePortName", name, "protocol", protocol, "nodePort", nodePort)
	}

	args = proxier.iptablesNonLocalNodePortArgs(nodePort, protocol, proxyIP, proxyPort, name)
	existed, err = proxier.iptables.EnsureRule(iptablesutil.Append, iptablesutil.TableFilter, iptablesNonLocalNodePortChain, args...)
	if err != nil {
		klog.ErrorS(err, "Failed to install iptables rule for service", "chain", iptablesNonLocalNodePortChain, "servicePortName", name)
		return err
	}
	if !existed {
		klog.InfoS("Opened iptables from-non-local public port for service", "servicePortName", name, "protocol", protocol, "nodePort", nodePort)
	}

	return nil
}

func (proxier *UserspaceLinux) closePortal(service iptables.ServicePortName, info *ServiceInfo) error {
	// Collect errors and report them all at the end.
	el := proxier.closeOnePortal(info.portal, info.protocol, proxier.listenIP, info.proxyPort, service)
	for _, publicIP := range info.externalIPs {
		el = append(el, proxier.closeOnePortal(portal{net.ParseIP(publicIP), info.portal.port, true}, info.protocol, proxier.listenIP, info.proxyPort, service)...)
	}
	for _, ingress := range info.loadBalancerIPs {
		if ingress != "" {
			el = append(el, proxier.closeOnePortal(portal{net.ParseIP(ingress), info.portal.port, false}, info.protocol, proxier.listenIP, info.proxyPort, service)...)
		}
	}
	if info.nodePort != 0 {
		el = append(el, proxier.closeNodePort(info.nodePort, info.protocol, proxier.listenIP, info.proxyPort, service)...)
	}
	if len(el) == 0 {
		klog.V(3).InfoS("Closed iptables portals for service", "servicePortName", service)
	} else {
		klog.ErrorS(nil, "Some errors closing iptables portals for service", "servicePortName", service)
	}
	return utilerrors.NewAggregate(el)
}

func (proxier *UserspaceLinux) closeOnePortal(portal portal, protocol localv1.Protocol, proxyIP net.IP, proxyPort int, name iptables.ServicePortName) []error {
	el := []error{}
	if proxier.localAddrs.Has(portal.ip) {
		if err := proxier.releaseNodePort(portal.ip, portal.port, protocol, name); err != nil {
			el = append(el, err)
		}
	}

	// Handle traffic from containers.
	args := proxier.iptablesContainerPortalArgs(portal.ip, portal.isExternal, false, portal.port, protocol, proxyIP, proxyPort, name)
	if err := proxier.iptables.DeleteRule(iptablesutil.TableNAT, iptablesContainerPortalChain, args...); err != nil {
		klog.ErrorS(err, "Failed to delete iptables rule for service", "chain", iptablesContainerPortalChain, "servicePortName", name)
		el = append(el, err)
	}

	if portal.isExternal {
		args := proxier.iptablesContainerPortalArgs(portal.ip, false, true, portal.port, protocol, proxyIP, proxyPort, name)
		if err := proxier.iptables.DeleteRule(iptablesutil.TableNAT, iptablesContainerPortalChain, args...); err != nil {
			klog.ErrorS(err, "Failed to delete iptables rule for service", "chain", iptablesContainerPortalChain, "servicePortName", name)
			el = append(el, err)
		}

		args = proxier.iptablesHostPortalArgs(portal.ip, true, portal.port, protocol, proxyIP, proxyPort, name)
		if err := proxier.iptables.DeleteRule(iptablesutil.TableNAT, iptablesHostPortalChain, args...); err != nil {
			klog.ErrorS(err, "Failed to delete iptables rule for service", "chain", iptablesHostPortalChain, "servicePortName", name)
			el = append(el, err)
		}
		return el
	}

	// Handle traffic from the host (portalIP is not external).
	args = proxier.iptablesHostPortalArgs(portal.ip, false, portal.port, protocol, proxyIP, proxyPort, name)
	if err := proxier.iptables.DeleteRule(iptablesutil.TableNAT, iptablesHostPortalChain, args...); err != nil {
		klog.ErrorS(err, "Failed to delete iptables rule for service", "chain", iptablesHostPortalChain, "servicePortName", name)
		el = append(el, err)
	}

	return el
}

func (proxier *UserspaceLinux) closeNodePort(nodePort int, protocol localv1.Protocol, proxyIP net.IP, proxyPort int, name iptables.ServicePortName) []error {
	el := []error{}

	// Handle traffic from containers.
	args := proxier.iptablesContainerPortalArgs(nil, false, false, nodePort, protocol, proxyIP, proxyPort, name)
	if err := proxier.iptables.DeleteRule(iptablesutil.TableNAT, iptablesContainerNodePortChain, args...); err != nil {
		klog.ErrorS(err, "Failed to delete iptables rule for service", "chain", iptablesContainerNodePortChain, "servicePortName", name)
		el = append(el, err)
	}

	// Handle traffic from the host.
	args = proxier.iptablesHostNodePortArgs(nodePort, protocol, proxyIP, proxyPort, name)
	if err := proxier.iptables.DeleteRule(iptablesutil.TableNAT, iptablesHostNodePortChain, args...); err != nil {
		klog.ErrorS(err, "Failed to delete iptables rule for service", "chain", iptablesHostNodePortChain, "servicePortName", name)
		el = append(el, err)
	}

	// Handle traffic not local to the host
	args = proxier.iptablesNonLocalNodePortArgs(nodePort, protocol, proxyIP, proxyPort, name)
	if err := proxier.iptables.DeleteRule(iptablesutil.TableFilter, iptablesNonLocalNodePortChain, args...); err != nil {
		klog.ErrorS(err, "Failed to delete iptables rule for service", "chain", iptablesNonLocalNodePortChain, "servicePortName", name)
		el = append(el, err)
	}

	if err := proxier.releaseNodePort(nil, nodePort, protocol, name); err != nil {
		el = append(el, err)
	}

	return el
}

// See comments in the *PortalArgs() functions for some details about why we
// use two chains for portals.
var iptablesContainerPortalChain iptablesutil.Chain = "KUBE-PORTALS-CONTAINER"
var iptablesHostPortalChain iptablesutil.Chain = "KUBE-PORTALS-HOST"

// Chains for NodePort services
var iptablesContainerNodePortChain iptablesutil.Chain = "KUBE-NODEPORT-CONTAINER"
var iptablesHostNodePortChain iptablesutil.Chain = "KUBE-NODEPORT-HOST"
var iptablesNonLocalNodePortChain iptablesutil.Chain = "KUBE-NODEPORT-NON-LOCAL"

// Ensure that the iptables infrastructure we use is set up.  This can safely be called periodically.
func iptablesInit(ipt Interface) error {
	// TODO: There is almost certainly room for optimization here.  E.g. If
	// we knew the service-cluster-ip-range CIDR we could fast-track outbound packets not
	// destined for a service. There's probably more, help wanted.

	// Danger - order of these rules matters here:
	//
	// We match portal rules first, then NodePort rules.  For NodePort rules, we filter primarily on --dst-type LOCAL,
	// because we want to listen on all local addresses, but don't match internet traffic with the same dst port number.
	//
	// There is one complication (per thockin):
	// -m addrtype --dst-type LOCAL is what we want except that it is broken (by intent without foresight to our usecase)
	// on at least GCE. Specifically, GCE machines have a daemon which learns what external IPs are forwarded to that
	// machine, and configure a local route for that IP, making a match for --dst-type LOCAL when we don't want it to.
	// Removing the route gives correct behavior until the daemon recreates it.
	// Killing the daemon is an option, but means that any non-kubernetes use of the machine with external IP will be broken.
	//
	// This applies to IPs on GCE that are actually from a load-balancer; they will be categorized as LOCAL.
	// _If_ the chains were in the wrong order, and the LB traffic had dst-port == a NodePort on some other service,
	// the NodePort would take priority (incorrectly).
	// This is unlikely (and would only affect outgoing traffic from the cluster to the load balancer, which seems
	// doubly-unlikely), but we need to be careful to keep the rules in the right order.
	args := []string{ /* service-cluster-ip-range matching could go here */ }
	args = append(args, "-m", "comment", "--comment", "handle ClusterIPs; NOTE: this must be before the NodePort rules")
	if _, err := ipt.EnsureChain(iptablesutil.TableNAT, iptablesContainerPortalChain); err != nil {
		return err
	}
	if _, err := ipt.EnsureRule(iptablesutil.Prepend, iptablesutil.TableNAT, iptablesutil.ChainPrerouting, append(args, "-j", string(iptablesContainerPortalChain))...); err != nil {
		return err
	}
	if _, err := ipt.EnsureChain(iptablesutil.TableNAT, iptablesHostPortalChain); err != nil {
		return err
	}
	if _, err := ipt.EnsureRule(iptablesutil.Prepend, iptablesutil.TableNAT, iptablesutil.ChainOutput, append(args, "-j", string(iptablesHostPortalChain))...); err != nil {
		return err
	}

	// This set of rules matches broadly (addrtype & destination port), and therefore must come after the portal rules
	args = []string{"-m", "addrtype", "--dst-type", "LOCAL"}
	args = append(args, "-m", "comment", "--comment", "handle service NodePorts; NOTE: this must be the last rule in the chain")
	if _, err := ipt.EnsureChain(iptablesutil.TableNAT, iptablesContainerNodePortChain); err != nil {
		return err
	}
	if _, err := ipt.EnsureRule(iptablesutil.Append, iptablesutil.TableNAT, iptablesutil.ChainPrerouting, append(args, "-j", string(iptablesContainerNodePortChain))...); err != nil {
		return err
	}
	if _, err := ipt.EnsureChain(iptablesutil.TableNAT, iptablesHostNodePortChain); err != nil {
		return err
	}
	if _, err := ipt.EnsureRule(iptablesutil.Append, iptablesutil.TableNAT, iptablesutil.ChainOutput, append(args, "-j", string(iptablesHostNodePortChain))...); err != nil {
		return err
	}

	// Create a chain intended to explicitly allow non-local NodePort
	// traffic to work around default-deny iptables configurations
	// that would otherwise reject such traffic.
	args = []string{"-m", "comment", "--comment", "Ensure that non-local NodePort traffic can flow"}
	if _, err := ipt.EnsureChain(iptablesutil.TableFilter, iptablesNonLocalNodePortChain); err != nil {
		return err
	}
	if _, err := ipt.EnsureRule(iptablesutil.Prepend, iptablesutil.TableFilter, iptablesutil.ChainInput, append(args, "-j", string(iptablesNonLocalNodePortChain))...); err != nil {
		return err
	}

	// TODO: Verify order of rules.
	return nil
}

// Flush all of our custom iptables rules.
func iptablesFlush(ipt iptablesutil.Interface) error {
	el := []error{}
	if err := ipt.FlushChain(iptablesutil.TableNAT, iptablesContainerPortalChain); err != nil {
		el = append(el, err)
	}
	if err := ipt.FlushChain(iptablesutil.TableNAT, iptablesHostPortalChain); err != nil {
		el = append(el, err)
	}
	if err := ipt.FlushChain(iptablesutil.TableNAT, iptablesContainerNodePortChain); err != nil {
		el = append(el, err)
	}
	if err := ipt.FlushChain(iptablesutil.TableNAT, iptablesHostNodePortChain); err != nil {
		el = append(el, err)
	}
	if err := ipt.FlushChain(iptablesutil.TableFilter, iptablesNonLocalNodePortChain); err != nil {
		el = append(el, err)
	}
	if len(el) != 0 {
		klog.ErrorS(utilerrors.NewAggregate(el), "Some errors flushing old iptables portals")
	}
	return utilerrors.NewAggregate(el)
}

// Used below.
var zeroIPv4 = net.ParseIP("0.0.0.0")
var localhostIPv4 = net.ParseIP("127.0.0.1")

var zeroIPv6 = net.ParseIP("::")
var localhostIPv6 = net.ParseIP("::1")

// Build a slice of iptables args that are common to from-container and from-host portal rules.
func iptablesCommonPortalArgs(destIP net.IP, addPhysicalInterfaceMatch bool, addDstLocalMatch bool, destPort int, protocol localv1.Protocol, service iptables.ServicePortName) []string {
	// This list needs to include all fields as they are eventually spit out
	// by iptables-save.  This is because some systems do not support the
	// 'iptables -C' arg, and so fall back on parsing iptables-save output.
	// If this does not match, it will not pass the check.  For example:
	// adding the /32 on the destination IP arg is not strictly required,
	// but causes this list to not match the final iptables-save output.
	// This is fragile and I hope one day we can stop supporting such old
	// iptables versions.
	args := []string{
		"-m", "comment",
		"--comment", service.String(),
		"-p", strings.ToLower(protocol.String()),
		"-m", strings.ToLower(protocol.String()),
		"--dport", fmt.Sprintf("%d", destPort),
	}

	if destIP != nil {
		args = append(args, "-d", ToCIDR(destIP))
	}

	if addPhysicalInterfaceMatch {
		args = append(args, "-m", "physdev", "!", "--physdev-is-in")
	}

	if addDstLocalMatch {
		args = append(args, "-m", "addrtype", "--dst-type", "LOCAL")
	}

	return args
}

// Build a slice of iptables args for a from-container portal rule.
func (proxier *UserspaceLinux) iptablesContainerPortalArgs(destIP net.IP, addPhysicalInterfaceMatch bool, addDstLocalMatch bool, destPort int, protocol localv1.Protocol, proxyIP net.IP, proxyPort int, service iptables.ServicePortName) []string {
	args := iptablesCommonPortalArgs(destIP, addPhysicalInterfaceMatch, addDstLocalMatch, destPort, protocol, service)

	// This is tricky.
	//
	// If the proxy is bound (see Proxier.listenIP) to 0.0.0.0 ("any
	// interface") we want to use REDIRECT, which sends traffic to the
	// "primary address of the incoming interface" which means the container
	// bridge, if there is one.  When the response comes, it comes from that
	// same interface, so the NAT matches and the response packet is
	// correct.  This matters for UDP, since there is no per-connection port
	// number.
	//
	// The alternative would be to use DNAT, except that it doesn't work
	// (empirically):
	//   * DNAT to 127.0.0.1 = Packets just disappear - this seems to be a
	//     well-known limitation of iptables.
	//   * DNAT to eth0's IP = Response packets come from the bridge, which
	//     breaks the NAT, and makes things like DNS not accept them.  If
	//     this could be resolved, it would simplify all of this code.
	//
	// If the proxy is bound to a specific IP, then we have to use DNAT to
	// that IP.  Unlike the previous case, this works because the proxy is
	// ONLY listening on that IP, not the bridge.
	//
	// Why would anyone bind to an address that is not inclusive of
	// localhost?  Apparently some cloud environments have their public IP
	// exposed as a real network interface AND do not have firewalling.  We
	// don't want to expose everything out to the world.
	//
	// Unfortunately, I don't know of any way to listen on some (N > 1)
	// interfaces but not ALL interfaces, short of doing it manually, and
	// this is simpler than that.
	//
	// If the proxy is bound to localhost only, all of this is broken.  Not
	// allowed.
	if proxyIP.Equal(zeroIPv4) || proxyIP.Equal(zeroIPv6) {
		// TODO: Can we REDIRECT with IPv6?
		args = append(args, "-j", "REDIRECT", "--to-ports", fmt.Sprintf("%d", proxyPort))
	} else {
		// TODO: Can we DNAT with IPv6?
		args = append(args, "-j", "DNAT", "--to-destination", net.JoinHostPort(proxyIP.String(), strconv.Itoa(proxyPort)))
	}
	return args
}

// Build a slice of iptables args for a from-host portal rule.
func (proxier *UserspaceLinux) iptablesHostPortalArgs(destIP net.IP, addDstLocalMatch bool, destPort int, protocol localv1.Protocol, proxyIP net.IP, proxyPort int, service iptables.ServicePortName) []string {
	args := iptablesCommonPortalArgs(destIP, false, addDstLocalMatch, destPort, protocol, service)

	// This is tricky.
	//
	// If the proxy is bound (see Proxier.listenIP) to 0.0.0.0 ("any
	// interface") we want to do the same as from-container traffic and use
	// REDIRECT.  Except that it doesn't work (empirically).  REDIRECT on
	// local packets sends the traffic to localhost (special case, but it is
	// documented) but the response comes from the eth0 IP (not sure why,
	// truthfully), which makes DNS unhappy.
	//
	// So we have to use DNAT.  DNAT to 127.0.0.1 can't work for the same
	// reason.
	//
	// So we do our best to find an interface that is not a loopback and
	// DNAT to that.  This works (again, empirically).
	//
	// If the proxy is bound to a specific IP, then we have to use DNAT to
	// that IP.  Unlike the previous case, this works because the proxy is
	// ONLY listening on that IP, not the bridge.
	//
	// If the proxy is bound to localhost only, this should work, but we
	// don't allow it for now.
	if proxyIP.Equal(zeroIPv4) || proxyIP.Equal(zeroIPv6) {
		proxyIP = proxier.hostIP
	}
	// TODO: Can we DNAT with IPv6?
	args = append(args, "-j", "DNAT", "--to-destination", net.JoinHostPort(proxyIP.String(), strconv.Itoa(proxyPort)))
	return args
}

// Build a slice of iptables args for a from-host public-port rule.
// See iptablesHostPortalArgs
// TODO: Should we just reuse iptablesHostPortalArgs?
func (proxier *UserspaceLinux) iptablesHostNodePortArgs(nodePort int, protocol localv1.Protocol, proxyIP net.IP, proxyPort int, service iptables.ServicePortName) []string {
	args := iptablesCommonPortalArgs(nil, false, false, nodePort, protocol, service)

	if proxyIP.Equal(zeroIPv4) || proxyIP.Equal(zeroIPv6) {
		proxyIP = proxier.hostIP
	}
	// TODO: Can we DNAT with IPv6?
	args = append(args, "-j", "DNAT", "--to-destination", net.JoinHostPort(proxyIP.String(), strconv.Itoa(proxyPort)))
	return args
}

// Build a slice of iptables args for an from-non-local public-port rule.
func (proxier *UserspaceLinux) iptablesNonLocalNodePortArgs(nodePort int, protocol localv1.Protocol, proxyIP net.IP, proxyPort int, service iptables.ServicePortName) []string {
	args := iptablesCommonPortalArgs(nil, false, false, proxyPort, protocol, service)
	args = append(args, "-m", "state", "--state", "NEW", "-j", "ACCEPT")
	return args
}

func isTooManyFDsError(err error) bool {
	return strings.Contains(err.Error(), "too many open files")
}

func isClosedError(err error) bool {
	// A brief discussion about handling closed error here:
	// https://code.google.com/p/go/issues/detail?id=4373#c14
	// TODO: maybe create a stoppable TCP listener that returns a StoppedError
	return strings.HasSuffix(err.Error(), "use of closed network connection")
}
