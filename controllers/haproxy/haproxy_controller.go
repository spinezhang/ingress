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
	hash "github.com/mitchellh/hashstructure"
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
	secretController 	cache.Controller
	ingLister  			store.IngressLister
	svcLister  			store.ServiceLister
	nodeLister 			store.NodeLister
	endpLister 			store.EndpointLister
	secretLister 		store.SecretLister
	sslCertTracker 		*sslCertTracker
	secretTracker 		*secretTracker

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
	tcpSvc 				[]haService

	rootCertHash		uint64
	haveHttps			bool
	pendingSsl			bool
	redirects			[]httpRedirect
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
		tcpServices: params.tcpSvcs,
		httpPort: params.httpPort,
		lbDefAlgorithm: params.lbDefAlgorithm,
		startSyslog: params.startSyslog,
		syncRateLimiter: flowcontrol.NewTokenBucketRateLimiter(0.1, 1),
		sslCertTracker: newSSLCertTracker(),
		secretTracker:  newSecretTracker(),
		rootCertHash: uint64(0),
		haveHttps: false,
		pendingSsl: false,
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
			lbc.extractSecretNames(addIng)
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
				lbc.extractSecretNames(curIng)
			}
		},
	}

	lbc.secretLister.Store, lbc.secretController = cache.NewInformer(
		cache.NewListWatchFromClient(kubeClient.Core().RESTClient(), "secrets", api_v1.NamespaceAll, fields.Everything()),
		&api_v1.Secret{}, resyncPeriod, cache.ResourceEventHandlerFuncs{})

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
	go lbc.secretController.Run(wait.NeverStop)
	go lbc.nodeController.Run(wait.NeverStop)
	go lbc.svcController.Run(wait.NeverStop)
	go lbc.endpController.Run(wait.NeverStop)

	go wait.Forever(lbc.syncSecret, 10*time.Second)
	go lbc.syncQueue.Run(10*time.Second, wait.NeverStop)
}

// storesSynced returns true if all the sub-controllers have finished their
// first sync with apiserver.
func (lbc *haproxyController) storesSynced() bool {
	return lbc.ingController.HasSynced() &&
			lbc.svcController.HasSynced() &&
			lbc.endpController.HasSynced() &&
			lbc.secretController.HasSynced()
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

	httpSvc := lbc.getIngServices()
	tcpSvc := lbc.getTcpServices()

	isChanged := false
	if !reflect.DeepEqual(httpSvc, lbc.httpSvc) {
		glog.Info("Http services is changed:",httpSvc)
		lbc.httpSvc = httpSvc
		isChanged = true
	}
	if !reflect.DeepEqual(tcpSvc, lbc.tcpSvc) {
		glog.Info("TCP services is changed:",tcpSvc)
		lbc.tcpSvc = tcpSvc
		isChanged = true
	}

	if isChanged {
		lbc.writeConfig(httpSvc, tcpSvc)
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

func (lbc *haproxyController) getKubeService(name string) *api_v1.Service {
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

func (lbc *haproxyController) getIngServices() (httpSvc []haService) {
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
		lbc.redirects = getIngressRedirects(ing)
		if lbc.redirects != nil {
			glog.Infof("redirects:%v\n", lbc.redirects)
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
				newSvc.SslTerm = false
				if val, ok := serviceAnnotations(kubeService.ObjectMeta.Annotations).getSslTerm(); ok {
					b, err := strconv.ParseBool(val)
					if err == nil {
						newSvc.SslTerm = b
					}
				}
				if newSvc.SslTerm {
					lbc.haveHttps = true
				}
				newSvc.CookieStickySession = false
				if val, ok := serviceAnnotations(kubeService.ObjectMeta.Annotations).getCookieStickySession(); ok {
					b, err := strconv.ParseBool(val)
					if err == nil {
						newSvc.CookieStickySession = b
					}
				}

				newSvc.Name = path.Backend.ServiceName
				if path.Backend.ServicePort.IntValue() != 80 {
					newSvc.Name = fmt.Sprintf("%s:%s",path.Backend.ServiceName, path.Backend.ServicePort.String())
				}
				if newSvc.SslTerm {
					newSvc.FrontendPort = 443
				} else {
					newSvc.FrontendPort = lbc.httpPort
				}
				httpSvc = append(httpSvc, newSvc)

				if defaultBackend == path.Backend.ServiceName {
					lbc.defaultHttpService = defaultBackend
				}
			}
		}
	}

	sort.Sort(serviceByName(httpSvc))
	return httpSvc
}

func (lbc *haproxyController) getTcpServices() (tcpSvc []haService) {
	newServices := []*api_v1.Service{}

	for name,_ := range lbc.tcpServices {
		if !strings.Contains(name, "/") {
			name = fmt.Sprintf("default/%s",name)
		}
		service := lbc.getKubeService(name)
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
			newSvc.FrontendPort = int(servicePort.Port)
			tcpSvc = append(tcpSvc, newSvc)
		}
	}
	return tcpSvc
}

func (lbc *haproxyController) writeConfig(httpSvc []haService, tcpSvc []haService) {
	services := map[string][]haService {
		"http":  httpSvc,
		"tcp":   tcpSvc,
	}
	httpHosts := serviceHostArray(httpSvc)
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
	conf["httpHosts"] = httpHosts
	conf["httpRedirects"] = lbc.redirects
	if lbc.rootCertHash != 0 {
		lbc.pendingSsl = false
		conf["haveHttps"] = strconv.FormatBool(lbc.haveHttps)
	} else {
		if lbc.haveHttps {
			lbc.pendingSsl = true
		} else {
			lbc.pendingSsl = false
		}
		conf["haveHttps"] = strconv.FormatBool(false)
	}

	conf["defLbAlgorithm"] = lbDefAlgorithm
	if lbc.lbDefAlgorithm != "" {
		conf["defLbAlgorithm"] = lbc.lbDefAlgorithm
	}

	defaultHttpName := lbc.defaultHttpService
	if defaultHttpName == "" && len(httpSvc) > 0 {
		defaultHttpName = httpSvc[0].Name
	}
	array := strings.Split(lbc.defaultHttpService, "/")
	if len(array) == 2 {
		defaultHttpName = array[1]
	}
	conf["defaultHttpService"] = defaultHttpName

	sslHosts := []string{}
	for _,svc := range httpSvc {
		if svc.SslTerm {
			sslHosts = append(sslHosts, svc.Host)
		}
	}
	if len(sslHosts) > 0 {
		conf["sslHosts"] = strings.Join(sslHosts, " ")
	}

	if err = t.Execute(w, conf); err != nil {
		glog.Errorf("Error write haproxy config file:",err)
	} else {
		glog.Info("Success write haproxy config file")
	}
}

func (lbc *haproxyController) syncSecret() {
	glog.Infof("starting syncing of secrets")

	if !lbc.hasSynced() {
		time.Sleep(storeSyncPollPeriod)
		glog.Warningf("deferring sync till endpoints controller has synced")
		return
	}

	keys := lbc.secretTracker.List()
	if len(keys) < 1 {
		glog.Info("No secret key is found...")
	}
	for _, k := range keys {
		key := k.(string)
		sslCert,sslKey,err := lbc.getPemCertificate(key)
		if err != nil {
			glog.Warningf("error obtaining PEM from secret %v: %v", key, err)
			continue
		}
		newHash, err := hash.Hash(sslCert, nil)
		if err != nil {
			glog.Warningf("error calculate cert hash code")
			continue
		}
		if newHash != lbc.rootCertHash {
			writeHaproxyCrt(sslCert, sslKey)
			lbc.rootCertHash = newHash
			if lbc.pendingSsl {
				lbc.writeConfig(lbc.httpSvc, lbc.tcpSvc)
				lbc.reload()
				lbc.pendingSsl = false
			}
		}
	}
}

func (lbc *haproxyController) getPemCertificate(secretName string) ([]byte,[]byte,error) {
	secretInterface, exists, err := lbc.secretLister.Store.GetByKey(secretName)
	if err != nil {
		return nil,nil,fmt.Errorf("error retrieving secret %v: %v", secretName, err)
	}
	if !exists {
		return nil,nil,fmt.Errorf("secret named %v does not exist", secretName)
	}

	secret := secretInterface.(*api_v1.Secret)
	glog.Infof("found secret:",secretName)
	sslCert := secret.Data[api_v1.TLSCertKey]
	sslKey := secret.Data[api_v1.TLSPrivateKeyKey]
	for key,value := range secret.Data {
		if strings.HasSuffix(key, ".crt") {
			sslCert = value
		} else if strings.HasSuffix(key, ".key") {
			sslKey = value
		}
	}

	return sslCert,sslKey,nil
}

func (lbc *haproxyController) extractSecretNames(ing *extensions.Ingress) {
	for _, tls := range ing.Spec.TLS {
		key := fmt.Sprintf("%v/%v", ing.Namespace, tls.SecretName)
		_, exists := lbc.secretTracker.Get(key)
		if !exists {
			lbc.secretTracker.Add(key, key)
		}
	}
}

func serviceHostArray(services []haService) []httpHost {
	hosts := []httpHost{}
	hostMap := map[string][]haService {}
	for _,svc := range services {
		if svc.Host != "" {
			hostMap[svc.Host] = append(hostMap[svc.Host], svc)
		} else {
			hostMap["default"] = append(hostMap["default"], svc)
		}
	}
	for key,val := range hostMap {
		newHost := httpHost{
			Name: key,
			Services: val,
		}
		hosts = append(hosts, newHost)
	}
	return hosts
}