package main

import (
	"reflect"
	"time"

	"github.com/golang/glog"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	api_v1 "k8s.io/client-go/pkg/api/v1"
	extensions "k8s.io/client-go/pkg/apis/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/util/wait"
	"fmt"
	"os/exec"
	"k8s.io/ingress/core/pkg/ingress/store"
	"k8s.io/ingress/core/pkg/task"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/ingress/core/pkg/ingress/status/leaderelection/resourcelock"
	"sort"
	"strconv"
	"k8s.io/apimachinery/pkg/util/intstr"
	"strings"
	"os"
	"text/template"
	"io"
)

const (
	resyncPeriod  = 20 * time.Second
	reloadCmd = "/haproxy_reload"
	configFile = "/etc/haproxy/haproxy.cfg"
	templateFile = "/haproxy.tmpl"
)

var (
	keyFunc = cache.DeletionHandlingMetaNamespaceKeyFunc

	// Frequency to poll on local stores to sync.
	storeSyncPollPeriod = 5 * time.Second
)

// LoadBalancerController watches the kubernetes api and adds/removes services
// from the loadbalancer, via loadBalancerConfig.
type haproxyController struct {
	client             	kubernetes.Interface

	ingController      	cache.Controller
	nodeController     	cache.Controller
	svcController      	cache.Controller
	endpController      cache.Controller
	ingLister  			store.IngressLister
	svcLister  			store.ServiceLister
	nodeLister 			store.NodeLister
	endpLister 			store.EndpointLister

	syncRateLimiter flowcontrol.RateLimiter

	syncQueue 			*task.Queue
	hasSynced func() 	bool

	template           string
	tcpServices        map[string]int
	defaultHttpService string
	httpPort			int
	lbDefAlgorithm		string
	sslCert        		string
	startSyslog			bool

	httpSvc 			[]haService
	httpsSvc 			[]haService
	tcpSvc 				[]haService
}

type haproxyParams struct {
	startSyslog    	bool
	httpPort 		int
	defaultHttpSvc 	string
	lbDefAlgorithm 	string
	tcpSvcs 		map[string]int
	//sslCert        	string
}

func newHaproxyController(kubeClient kubernetes.Interface, namespace string, params *haproxyParams) *haproxyController {
	lbc := haproxyController{
		client: kubeClient,
		tcpServices:     params.tcpSvcs,
		defaultHttpService: params.defaultHttpSvc,
		httpPort: params.httpPort,
		lbDefAlgorithm: params.lbDefAlgorithm,
		//sslCert: params.sslCert,
		startSyslog: params.startSyslog,
		syncRateLimiter: flowcontrol.NewTokenBucketRateLimiter(0.1, 1),
	}

	lbc.syncQueue = task.NewTaskQueue(lbc.syncIngress)
	lbc.hasSynced = lbc.storesSynced

	ingEventHandler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			addIng := obj.(*extensions.Ingress)
			if !isHaproxyIngress(addIng) {
				glog.Infof("Ignoring add for ingress %v based on annotation %v", addIng.Name, ingressClassKey)
				return
			}
			glog.Infof("Add notification received for Ingress %v/%v", addIng.Namespace, addIng.Name)
			lbc.syncQueue.Enqueue(obj)
		},
		DeleteFunc: func(obj interface{}) {
			delIng := obj.(*extensions.Ingress)
			if !isHaproxyIngress(delIng) {
				glog.Infof("Ignoring delete for ingress %v based on annotation %v", delIng.Name, ingressClassKey)
				return
			}
			glog.Infof("Delete notification received for Ingress %v/%v", delIng.Namespace, delIng.Name)
			lbc.syncQueue.Enqueue(obj)
		},
		UpdateFunc: func(old, cur interface{}) {
			curIng := cur.(*extensions.Ingress)
			if !isHaproxyIngress(curIng) {
				return
			}
			glog.Infof("Update notification received for Ingress %v/%v", curIng.Namespace, curIng.Name)
			if !reflect.DeepEqual(old, cur) {
				glog.Infof("Ingress %v changed, syncing", curIng.Name)
				lbc.syncQueue.Enqueue(cur)
			}
		},
	}

	lbc.ingLister.Store, lbc.ingController = cache.NewInformer(
		cache.NewListWatchFromClient(kubeClient.Extensions().RESTClient(), "ingresses", namespace, fields.Everything()),
		&extensions.Ingress{}, resyncPeriod, ingEventHandler)

	eventHandler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ep := obj.(*api_v1.Endpoints)
			_, found := ep.Annotations[resourcelock.LeaderElectionRecordAnnotationKey]
			if found {
				return
			}
			lbc.syncQueue.Enqueue(obj)
		},
		DeleteFunc: func(obj interface{}) {
			lbc.syncQueue.Enqueue(obj)
		},
		UpdateFunc: func(old, cur interface{}) {
			if !reflect.DeepEqual(old, cur) {
				ep := cur.(*api_v1.Endpoints)
				_, found := ep.Annotations[resourcelock.LeaderElectionRecordAnnotationKey]
				if found {
					return
				}
				lbc.syncQueue.Enqueue(cur)
			}
		},
	}
	lbc.endpLister.Store, lbc.endpController = cache.NewInformer(
		cache.NewListWatchFromClient(kubeClient.Core().RESTClient(), "endpoints", namespace, fields.Everything()),
		&api_v1.Endpoints{}, resyncPeriod, eventHandler)

	lbc.svcLister.Store, lbc.svcController = cache.NewInformer(
		cache.NewListWatchFromClient(
			kubeClient.Core().RESTClient(), "services", namespace, fields.Everything()),
		&api_v1.Service{}, resyncPeriod, cache.ResourceEventHandlerFuncs{})

	lbc.nodeLister.Store, lbc.nodeController = cache.NewInformer(
		cache.NewListWatchFromClient(kubeClient.Core().RESTClient(), "nodes", api_v1.NamespaceAll, fields.Everything()),
		&api_v1.Node{},
		resyncPeriod,
		cache.ResourceEventHandlerFuncs{},
	)

	return &lbc
}

func (lbc *haproxyController) Run() {
	go lbc.reload()

	go lbc.ingController.Run(wait.NeverStop)
	go lbc.nodeController.Run(wait.NeverStop)
	go lbc.svcController.Run(wait.NeverStop)
	go lbc.endpController.Run(wait.NeverStop)

	go lbc.syncQueue.Run(10*time.Second, wait.NeverStop)
}

// storesSynced returns true if all the sub-controllers have finished their
// first sync with apiserver.
func (lbc *haproxyController) storesSynced() bool {
	return lbc.ingController.HasSynced() &&
			lbc.svcController.HasSynced() &&
			lbc.endpController.HasSynced()
}

// sync manages Ingress create/updates/deletes.
func (lbc *haproxyController) syncIngress(key interface{}) error {
	glog.Infof("sync, key:",key)
	lbc.syncRateLimiter.Accept()
	if lbc.syncQueue.IsShuttingDown() {
		glog.Infof("sync, syncQueue.IsShuttingDown...")
		return nil
	}

	if !lbc.hasSynced() {
		time.Sleep(storeSyncPollPeriod)
		return fmt.Errorf("waiting for stores to sync")
	}

	httpSvc, httpsSvc, tcpSvc := lbc.getServices()

	isChanged := false
	if !reflect.DeepEqual(httpSvc, lbc.httpSvc) {
		glog.Info("Http services is changed:",httpSvc)
		lbc.httpSvc = httpSvc
		isChanged = true
	}
	if !reflect.DeepEqual(httpsSvc, lbc.httpsSvc) {
		glog.Info("Https services is changed:",httpsSvc)
		lbc.httpsSvc = httpsSvc
		isChanged = true
	}
	if !reflect.DeepEqual(tcpSvc, lbc.tcpSvc) {
		glog.Info("TCP services is changed:",tcpSvc)
		lbc.tcpSvc = tcpSvc
		isChanged = true
	}

	if isChanged {
		lbc.writeConfig(httpSvc, httpsSvc, tcpSvc)
		lbc.reload()
	}

	return nil
}

func (lbc *haproxyController) reload() error {
	glog.Info("Reload haproxy...")
	output, err := exec.Command("sh", "-c", reloadCmd).CombinedOutput()
	msg := fmt.Sprintf("%v -- %v", reloadCmd, string(output))
	if err != nil {
		return fmt.Errorf("error restarting %v: %v", msg, err)
	}
	glog.Infof(msg)
	return nil
}

func (lbc *haproxyController) getEndpoints(s *api_v1.Service, servicePort *api_v1.ServicePort) (endpoints []string) {
	ep, err := lbc.endpLister.GetServiceEndpoints(s)
	if err != nil {
		return
	}

	// The intent here is to create a union of all subsets that match a targetPort.
	// We know the endpoint already matches the service, so all pod ips that have
	// the target port are capable of service traffic for it.
	for _, ss := range ep.Subsets {
		for _, epPort := range ss.Ports {
			var targetPort int
			switch servicePort.TargetPort.Type {
			case intstr.Int:
				if int(epPort.Port) == getTargetPort(servicePort) {
					targetPort = int(epPort.Port)
				}
			case intstr.String:
				if epPort.Name == servicePort.TargetPort.StrVal {
					targetPort = int(epPort.Port)
				}
			}
			if targetPort == 0 {
				continue
			}
			for _, epAddress := range ss.Addresses {
				endpoints = append(endpoints, fmt.Sprintf("%v:%v", epAddress.IP, targetPort))
			}
		}
	}
	endpoints = removeDuplicate(endpoints)
	return
}

func (lbc *haproxyController) getServices() (httpSvc []haService, httpsSvc []haService, tcpSvc []haService) {
	services := []*api_v1.Service{}
	defaultService := lbc.getKubeService(lbc.defaultHttpService)
	if defaultSvc != nil {
		services = append(services,defaultService)
	}

	services = append(services, lbc.getIngServices(services)...)
	services = append(services, lbc.getTcpServices(services)...)

	ep := []string{}
	for _, s := range services {
		if s.Spec.Type == api_v1.ServiceTypeLoadBalancer {
			glog.Infof("Ignoring service %v, it already has a loadbalancer", s.Name)
			continue
		}
		for _, servicePort := range s.Spec.Ports {
			// TODO: headless services?
			sName := s.Name
			if servicePort.Protocol == api_v1.ProtocolUDP {
				glog.Infof("Ignoring UDP port %v: %+v", sName, servicePort)
				continue
			}

			ep = lbc.getEndpoints(s, &servicePort)
			if len(ep) == 0 {
				glog.Infof("No endpoints found for service %v, port %+v",sName, servicePort)
				continue
			}
			newSvc := haService{
				Name:        getServiceNameForLBRule(s, int(servicePort.Port)),
				Ep:          ep,
				BackendPort: getTargetPort(&servicePort),
			}

			if val, ok := serviceAnnotations(s.ObjectMeta.Annotations).getHost(); ok {
				newSvc.Host = val
			}

			if val, ok := serviceAnnotations(s.ObjectMeta.Annotations).getAlgorithm(); ok {
				for _, current := range supportedAlgorithms {
					if val == current {
						newSvc.Algorithm = val
						break
					}
				}
			} else {
				newSvc.Algorithm = lbc.lbDefAlgorithm
			}

			// By default sticky session is disabled
			newSvc.SessionAffinity = false
			if s.Spec.SessionAffinity != "" {
				newSvc.SessionAffinity = true
			}

			// By default sslTerm is disabled
			newSvc.SslByPass = false
			if val, ok := serviceAnnotations(s.ObjectMeta.Annotations).getSslByPass(); ok {
				b, err := strconv.ParseBool(val)
				if err == nil && b && servicePort.Name == "https" {
					newSvc.SslByPass = true
				}
			}

			if val, ok := serviceAnnotations(s.ObjectMeta.Annotations).getAclMatch(); ok {
				newSvc.AclMatch = val
			}

			if port, ok := lbc.tcpServices[sName]; ok && port == int(servicePort.Port) {
				newSvc.FrontendPort = int(servicePort.Port)
				tcpSvc = append(tcpSvc, newSvc)
			} else {
				if val, ok := serviceAnnotations(s.ObjectMeta.Annotations).getCookieStickySession(); ok {
					b, err := strconv.ParseBool(val)
					if err == nil {
						newSvc.CookieStickySession = b
					}
				}

				newSvc.FrontendPort = lbc.httpPort
				if newSvc.SslByPass == true {
					httpsSvc = append(httpsSvc, newSvc)
				} else {
					httpSvc = append(httpSvc, newSvc)
				}

				if  lbc.defaultHttpService =="" && newSvc.BackendPort == 80 {
					lbc.defaultHttpService = newSvc.Name
					glog.Infof("No default root http is set, set to the first 80 svc:%s\n",lbc.defaultHttpService)
				}

			}
		}
	}

	sort.Sort(serviceByName(httpSvc))
	sort.Sort(serviceByName(httpsSvc))
	sort.Sort(serviceByName(tcpSvc))

	return
}

func (lbc *haproxyController) getKubeService(name string) *api_v1.Service {
	array := strings.Split(name,"/")
	if len(array) == 1 {
		name = fmt.Sprintf("default/%s",name)
	}
	svcObj, svcExists, err := lbc.svcLister.Store.GetByKey(name)
	if err != nil {
		glog.Warningf("unexpected error searching the default backend %v: %v", name, err)
		return nil
	}

	if !svcExists {
		glog.Warningf("service %v does not exist:%v", name,svcObj)
		return nil
	}

	glog.Infof("found service:", name)
	return svcObj.(*api_v1.Service)
}

func (lbc *haproxyController) getIngServices(services []*api_v1.Service) []*api_v1.Service {
	newServices := []*api_v1.Service{}

	//ings := lbc.ingLister.Store.List()
	//sort.Sort(ingressByRevision(ings))
	//
	//for _, ingIf := range ings {
	//	ing := ingIf.(*extensions.Ingress)
	//
	//	if !isHaproxyIngress(ing) {
	//		continue
	//	}
	//
	//	secUpstream := ic.annotations.SecureUpstream(ing)
	//	hz := ic.annotations.HealthCheck(ing)
	//	affinity := ic.annotations.SessionAffinity(ing)
	//
	//	var defBackend string
	//	if ing.Spec.Backend != nil {
	//		defBackend = fmt.Sprintf("%v-%v-%v",
	//			ing.GetNamespace(),
	//			ing.Spec.Backend.ServiceName,
	//			ing.Spec.Backend.ServicePort.String())
	//
	//		glog.V(3).Infof("creating upstream %v", defBackend)
	//		upstreams[defBackend] = newUpstream(defBackend)
	//
	//		svcKey := fmt.Sprintf("%v/%v", ing.GetNamespace(), ing.Spec.Backend.ServiceName)
	//		endps, err := ic.serviceEndpoints(svcKey, ing.Spec.Backend.ServicePort.String(), hz)
	//		upstreams[defBackend].Endpoints = append(upstreams[defBackend].Endpoints, endps...)
	//		if err != nil {
	//			glog.Warningf("error creating upstream %v: %v", defBackend, err)
	//		}
	//	}
	//
	//	for _, rule := range ing.Spec.Rules {
	//		if rule.HTTP == nil {
	//			continue
	//		}
	//
	//		for _, path := range rule.HTTP.Paths {
	//			name := fmt.Sprintf("%v-%v-%v",
	//				ing.GetNamespace(),
	//				path.Backend.ServiceName,
	//				path.Backend.ServicePort.String())
	//
	//			if _, ok := upstreams[name]; ok {
	//				continue
	//			}
	//
	//			glog.V(3).Infof("creating upstream %v", name)
	//			upstreams[name] = newUpstream(name)
	//			if !upstreams[name].Secure {
	//				upstreams[name].Secure = secUpstream
	//			}
	//			if upstreams[name].SessionAffinity.AffinityType == "" {
	//				upstreams[name].SessionAffinity.AffinityType = affinity.AffinityType
	//				if affinity.AffinityType == "cookie" {
	//					upstreams[name].SessionAffinity.CookieSessionAffinity.Name = affinity.CookieConfig.Name
	//					upstreams[name].SessionAffinity.CookieSessionAffinity.Hash = affinity.CookieConfig.Hash
	//				}
	//			}
	//
	//			svcKey := fmt.Sprintf("%v/%v", ing.GetNamespace(), path.Backend.ServiceName)
	//			endp, err := ic.serviceEndpoints(svcKey, path.Backend.ServicePort.String(), hz)
	//			if err != nil {
	//				glog.Warningf("error obtaining service endpoints: %v", err)
	//				continue
	//			}
	//			upstreams[name].Endpoints = endp
	//
	//			s, exists, err := ic.svcLister.Store.GetByKey(svcKey)
	//			if err != nil {
	//				glog.Warningf("error obtaining service: %v", err)
	//				continue
	//			}
	//
	//			if exists {
	//				upstreams[name].Service = s.(*api.Service)
	//			} else {
	//				glog.Warningf("service %v does not exists", svcKey)
	//			}
	//			upstreams[name].Port = path.Backend.ServicePort
	//		}
	//	}
	//}

	return newServices
}

func (lbc *haproxyController) getTcpServices(services []*api_v1.Service) []*api_v1.Service {
	newServices := []*api_v1.Service{}

	for name,_ := range lbc.tcpServices {
		service := lbc.getKubeService(name)
		if  service != nil {
			exist := false
			for _,item := range services {
				if reflect.DeepEqual(service,item) {
					glog.Infof("TCP service:%v exist",name)
					exist = true
					break
				}
			}
			if !exist {
				glog.Infof("append TCP service:",name)
				newServices = append(newServices, service)
			}
		}
	}

	return newServices
}

func (lbc *haproxyController) writeConfig(httpSvc []haService, httpsSvc []haService, tcpSvc []haService) {
	services := map[string][]haService {
		"http":  httpSvc,
		"https": httpsSvc,
		"tcp":   tcpSvc,
	}

	var w io.Writer
	w, err := os.Create(configFile)
	if err != nil {
		return
	}
	var t *template.Template
	t, err = template.ParseFiles(templateFile)
	if err != nil {
		return
	}

	conf := make(map[string]interface{})
	conf["startSyslog"] = strconv.FormatBool(lbc.startSyslog)
	conf["services"] = services
	if len(httpsSvc) > 0 {
		conf["httpsByPass"] = strconv.FormatBool(true)
	} else {
		conf["httpsByPass"] = strconv.FormatBool(false)
	}

	//var sslConfig string
	//if lbc.sslCert != "" {
	//	sslConfig = "crt " + lbc.sslCert
	//}
	//conf["sslCert"] = sslConfig

	// default load balancer algorithm is roundrobin
	conf["defLbAlgorithm"] = lbDefAlgorithm
	if lbc.lbDefAlgorithm != "" {
		conf["defLbAlgorithm"] = lbc.lbDefAlgorithm
	}

	defaultHttpName := lbc.defaultHttpService
	array := strings.Split(lbc.defaultHttpService, "/")
	if len(array) == 2 {
		defaultHttpName = array[1]
	}
	conf["defaultHttpService"] = defaultHttpName

	for _,svc := range httpsSvc {
		httpsName := svc.Name
		tempArray := strings.Split(svc.Name, ":")
		if len(tempArray) == 2 {
			httpsName = tempArray[0]
		}
		if httpsName == defaultHttpName {
			conf["defaultHttps"] = svc.Name
			glog.Infof("defaultHttps:",svc.Name)
			break
		}
	}


	if err = t.Execute(w, conf); err != nil {
		glog.Errorf("Error write haproxy config file:",err)
	} else {
		glog.Info("Success write haproxy config file")
	}
}

// encapsulates all the hacky convenience type name modifications for lb rules.
// - :80 services don't need a :80 postfix
// - default ns should be accessible without /ns/name (when we have /ns support)
func getServiceNameForLBRule(s *api_v1.Service, servicePort int) string {
	if servicePort == 80 {
		return s.Name
	}
	return fmt.Sprintf("%v:%v", s.Name, servicePort)
}

func getTargetPort(servicePort *api_v1.ServicePort) int {
	return servicePort.TargetPort.IntValue()
}
