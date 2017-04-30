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
	//keyFunc = cache.DeletionHandlingMetaNamespaceKeyFunc
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
	//defaultHttpSvc 	string
	lbDefAlgorithm 	string
	tcpSvcs 		map[string]int
}

func newHaproxyController(kubeClient kubernetes.Interface, namespace string, params *haproxyParams) *haproxyController {
	lbc := haproxyController{
		client: kubeClient,
		tcpServices:     params.tcpSvcs,
		//defaultHttpService: params.defaultHttpSvc,
		httpPort: params.httpPort,
		lbDefAlgorithm: params.lbDefAlgorithm,
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

	httpSvc, httpsSvc := lbc.getIngServices()
	tcpSvc := lbc.getTcpServices()

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

func (lbc *haproxyController) getEndpoints(s *api_v1.Service, servicePort int) (endpoints []string) {
	ep, err := lbc.endpLister.GetServiceEndpoints(s)
	if err != nil {
		return
	}

	for _, ss := range ep.Subsets {
		for _, epPort := range ss.Ports {
			if  int(epPort.Port) == servicePort {
				for _, epAddress := range ss.Addresses {
					endpoints = append(endpoints, fmt.Sprintf("%v:%d", epAddress.IP, servicePort))
				}
			}
		}
	}
	endpoints = removeDuplicate(endpoints)
	return
}

//func (lbc *haproxyController) getServices() (httpSvc []haService, httpsSvc []haService, tcpSvc []haService) {
//	services := []*api_v1.Service{}
//	//defaultService := lbc.getKubeService(lbc.defaultHttpService)
//	//if defaultSvc != nil {
//	//	services = append(services,defaultService)
//	//}
//
//	services = append(services, lbc.getIngServices(services)...)
//	services = append(services, lbc.getTcpServices(services)...)
//
//	ep := []string{}
//	for _, s := range services {
//		if s.Spec.Type == api_v1.ServiceTypeLoadBalancer {
//			glog.Infof("Ignoring service %v, it already has a loadbalancer", s.Name)
//			continue
//		}
//		for _, servicePort := range s.Spec.Ports {
//			// TODO: headless services?
//			sName := s.Name
//			if servicePort.Protocol == api_v1.ProtocolUDP {
//				glog.Infof("Ignoring UDP port %v: %+v", sName, servicePort)
//				continue
//			}
//
//			ep = lbc.getEndpoints(s, &servicePort)
//			if len(ep) == 0 {
//				glog.Infof("No endpoints found for service %v, port %+v",sName, servicePort)
//				continue
//			}
//			newSvc := haService{
//				Name:        getServiceNameForLBRule(s, int(servicePort.Port)),
//				Ep:          ep,
//				BackendPort: getTargetPort(&servicePort),
//			}
//
//			if val, ok := serviceAnnotations(s.ObjectMeta.Annotations).getHost(); ok {
//				newSvc.Host = val
//			}
//
//			if val, ok := serviceAnnotations(s.ObjectMeta.Annotations).getAlgorithm(); ok {
//				for _, current := range supportedAlgorithms {
//					if val == current {
//						newSvc.Algorithm = val
//						break
//					}
//				}
//			} else {
//				newSvc.Algorithm = lbc.lbDefAlgorithm
//			}
//
//			// By default sticky session is disabled
//			newSvc.SessionAffinity = false
//			if s.Spec.SessionAffinity != "" {
//				newSvc.SessionAffinity = true
//			}
//
//			// By default sslTerm is disabled
//			newSvc.SslByPass = false
//			if val, ok := serviceAnnotations(s.ObjectMeta.Annotations).getSslByPass(); ok {
//				b, err := strconv.ParseBool(val)
//				if err == nil && b && servicePort.Name == "https" {
//					newSvc.SslByPass = true
//				}
//			}
//
//			if val, ok := serviceAnnotations(s.ObjectMeta.Annotations).getAclMatch(); ok {
//				newSvc.AclMatch = val
//			}
//
//			if port, ok := lbc.tcpServices[sName]; ok && port == int(servicePort.Port) {
//				newSvc.FrontendPort = int(servicePort.Port)
//				tcpSvc = append(tcpSvc, newSvc)
//			} else {
//				if val, ok := serviceAnnotations(s.ObjectMeta.Annotations).getCookieStickySession(); ok {
//					b, err := strconv.ParseBool(val)
//					if err == nil {
//						newSvc.CookieStickySession = b
//					}
//				}
//
//				newSvc.FrontendPort = lbc.httpPort
//				if newSvc.SslByPass == true {
//					httpsSvc = append(httpsSvc, newSvc)
//				} else {
//					httpSvc = append(httpSvc, newSvc)
//				}
//
//				//if  lbc.defaultHttpService =="" && newSvc.BackendPort == 80 {
//				//	lbc.defaultHttpService = newSvc.Name
//				//	glog.Infof("No default root http is set, set to the first 80 svc:%s\n",lbc.defaultHttpService)
//				//}
//			}
//		}
//	}
//
//	sort.Sort(serviceByName(httpSvc))
//	sort.Sort(serviceByName(httpsSvc))
//	sort.Sort(serviceByName(tcpSvc))
//
//	return
//}

func (lbc *haproxyController) getKubeService(name string) *api_v1.Service {
	//array := strings.Split(name,"/")
	//if len(array) == 1 {
	//	name = fmt.Sprintf("default/%s",name)
	//}
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

func (lbc *haproxyController) getIngServices() (httpSvc []haService, httpsSvc []haService) {
	ings := lbc.ingLister.Store.List()
	sort.Sort(ingressByRevision(ings))

	for _, ingIf := range ings {
		ing := ingIf.(*extensions.Ingress)

		if !isHaproxyIngress(ing) {
			continue
		}
		defaultBackend := ""
		glog.Infof("Found ingress:",*ing)
		if ing.Spec.Backend != nil {
			defaultBackend = ing.Spec.Backend.ServiceName
		}

		for _, rule := range ing.Spec.Rules {
			if rule.HTTP == nil {
				continue
			}
			host := rule.Host
			for _, path := range rule.HTTP.Paths {
				kubeService := lbc.getKubeService(fmt.Sprintf("%v/%v", ing.GetNamespace(), path.Backend.ServiceName))
				if kubeService == nil {
					glog.Infof("No serivces found for service %v, port %+v",path.Backend.ServiceName, path.Backend.ServicePort.String())
					continue
				}
				endp := lbc.getEndpoints(kubeService, path.Backend.ServicePort.IntValue())
				if len(endp) == 0 {
					glog.Infof("No endpoints found for service %v, port %+v",path.Backend.ServiceName, path.Backend.ServicePort.String())
					continue
				}

				newSvc := haService{
					Ep: endp,
					BackendPort: path.Backend.ServicePort.IntValue(),
				}
				if val, ok := serviceAnnotations(kubeService.ObjectMeta.Annotations).getAlgorithm(); ok {
					for _, current := range supportedAlgorithms {
						if val == current {
							newSvc.Algorithm = val
							break
						}
					}
				} else {
					newSvc.Algorithm = lbc.lbDefAlgorithm
				}
				if path.Path != "" {
					newSvc.Path = path.Path
				}
				if host != "" {
					newSvc.Host = host
				}
				newSvc.SessionAffinity = false
				if kubeService.Spec.SessionAffinity != "" {
					newSvc.SessionAffinity = true
				}
				//newSvc.SslByPass = false
				if val, ok := serviceAnnotations(kubeService.ObjectMeta.Annotations).getCookieStickySession(); ok {
					b, err := strconv.ParseBool(val)
					if err == nil {
						newSvc.CookieStickySession = b
					}
				}

				if path.Backend.ServicePort.String() == "80" {
					newSvc.FrontendPort = lbc.httpPort
					newSvc.Name = path.Backend.ServiceName
					httpSvc = append(httpSvc, newSvc)
				} else {
					newSvc.FrontendPort = path.Backend.ServicePort.IntValue()
					newSvc.Name = fmt.Sprintf("%s:%s",path.Backend.ServiceName, path.Backend.ServicePort.String())
					httpsSvc = append(httpsSvc, newSvc)
				}

				if defaultBackend == path.Backend.ServiceName &&
						(path.Backend.ServicePort.String() == "80" || path.Backend.ServicePort.String() == "443") {
					lbc.defaultHttpService = defaultBackend
				}
			}
		}
	}

	sort.Sort(serviceByName(httpSvc))
	sort.Sort(serviceByName(httpsSvc))
	return httpSvc,httpsSvc
}

func (lbc *haproxyController) getTcpServices() (tcpSvc []haService) {
	newServices := []*api_v1.Service{}

	for name,_ := range lbc.tcpServices {
		service := lbc.getKubeService(fmt.Sprintf("default/%s",name))
		if  service != nil {
			newServices = append(newServices, service)
		}
	}

	for _, service := range newServices {
		for _, servicePort := range service.Spec.Ports {
			if servicePort.Protocol == api_v1.ProtocolUDP {
				glog.Infof("Ignoring UDP port %v: %+v", service.Name, servicePort)
				continue
			}

			endp := lbc.getEndpoints(service, int(servicePort.Port))
			if len(endp) == 0 {
				glog.Infof("No endpoints found for service %v, port %d",service.Name, servicePort)
				continue
			}
			newSvc := haService{
				Name:        fmt.Sprintf("%v:%d", service.Name, servicePort.Port),
				Ep:          endp,
				BackendPort: servicePort.TargetPort.IntValue(),
			}

			if val, ok := serviceAnnotations(service.ObjectMeta.Annotations).getAlgorithm(); ok {
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
			if service.Spec.SessionAffinity != "" {
				newSvc.SessionAffinity = true
			}

			// By default sslTerm is disabled
			//newSvc.SslByPass = false
			newSvc.FrontendPort = int(servicePort.Port)
			tcpSvc = append(tcpSvc, newSvc)
		}
	}
	return tcpSvc
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
