package main

import (
	"os"
	"strings"

	go_flag "flag"
	flag "github.com/spf13/pflag"
	"github.com/golang/glog"
	"time"
	"k8s.io/client-go/pkg/api"
	"strconv"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	defaultErrorPage         = "file:///etc/haproxy/errors/404.http"
	lbAPIPort = 8181
)

var (
	flags = flag.NewFlagSet("", flag.ContinueOnError)

	// See https://cbonte.github.io/haproxy-dconv/configuration-1.5.html#4.2-balance
	// In brief:
	//  * roundrobin: backend with the highest weight (how is this set?) receives new connection
	//  * leastconn: backend with least connections receives new connection
	//  * first: first server sorted by server id, with an available slot receives connection
	//  * source: connection given to backend based on hash of source ip
	supportedAlgorithms = []string{"roundrobin", "leastconn", "first", "source"}

	inCluster = flags.Bool("use-kubernetes-cluster-service", true, `If true, use the built in kubernetes
                cluster for creating the client`)

	apiServerHost = flags.String("apiserver-host", "", "The address of the Kubernetes Apiserver "+
			"to connect to in the format of protocol://address:port, e.g., "+
			"http://localhost:8080. If not specified, the assumption is that the binary runs inside a "+
			"Kubernetes cluster and local discovery is attempted.")
	kubeConfigFile = flags.String("kubeconfig", "", "Path to kubeconfig file with authorization and master location information.")

	// If you have pure tcp services or https services that need L3 routing, you
	// must specify them by name. Note that you are responsible for:
	// 1. Making sure there is no collision between the service ports of these services.
	//      - You can have multiple <mysql svc name>:3306 specifications in this map, and as
	//        long as the service ports of your mysql service don't clash, you'll get
	//        loadbalancing for each one.
	// 2. Exposing the service ports as node ports on a pod.
	// 3. Adding firewall rules so these ports can ingress traffic.
	//
	// Any service not specified in this map is treated as an http:80 service,
	// unless TargetService dictates otherwise.

	tcpServices = flags.String("tcp-services", "", `Comma separated list of tcp/https
                serviceName:servicePort pairings. This assumes you've opened up the right
                hostPorts for each service that serves ingress traffic.`)

	//statsPort = flags.Int("stats-port", 1936, `Port for loadbalancer stats,
     //           Used in the loadbalancer liveness probe.`)

	startSyslog = flags.Bool("syslog", false, `if set, it will start a syslog server
                that will forward haproxy logs to stdout.`)

	//sslCert   = flags.String("ssl-cert", "", `if set, it will load the certificate.`)
	//sslCertKey = flags.String("ssl-cert-key", "", `if set, it will load the certificate from which
	//	to load CA certificates used to verify client's certificate.`)

	errorPage = flags.String("error-page", "", `if set, it will try to load the content
                as a web page and use the content as error page. Is required that the URL returns
                200 as a status code`)

	defaultReturnCode = flags.Int("default-return-code", 404, `if set, this HTTP code is written
				out for requests that don't match other rules.`)

	lbDefAlgorithm = flags.String("balance-algorithm", "roundrobin", `if set, it allows a custom
                default balance algorithm.`)

	//defaultSvc = flags.String("default-backend-service", "", `if set, use this service for / path.`)
	defaultHttpPort = flags.Int("http-port", 80, `default Http port.`)
)

func main() {
	go_flag.Lookup("logtostderr").Value.Set("true")
	//go_flag.Set("v", "3")
	glog.Info("start haproxy ingress controller")

	flags.Parse(os.Args)
	//cfg := parseCfg(*config, *lbDefAlgorithm, *sslCert, *sslCaCert)

	defErrorPage := newStaticPageHandler(*errorPage, defaultErrorPage, *defaultReturnCode)
	if defErrorPage == nil {
		glog.Fatalf("Failed to load the default error page")
	}
	go registerHandlers(lbAPIPort, defErrorPage)

	var tcpSvcs map[string]int
	if *tcpServices != "" {
		tcpSvcs = parseTCPServices(*tcpServices)
	} else {
		glog.Infof("No tcp/https services specified")
	}

	//glog.Infof("default backend service:",*defaultSvc)
	if *startSyslog {
		//cfg.startSyslog = true
		_, err := newSyslogServer("/var/run/haproxy.log.socket")
		if err != nil {
			glog.Fatalf("Failed to start syslog server:", err)
		}
	}

	kubeClient,err := createClient(*inCluster, *apiServerHost, *kubeConfigFile)
	if err != nil {
		glog.Fatalf("Failed to create kubernetes client: %v", err)
	}
	//sslCertFile := *sslCert
	//if *sslCertKey != "" {
	//	sslCertFile = mergeSslCert(*sslCert, *sslCertKey)
	//}
	params := haproxyParams {
		startSyslog: *startSyslog,
		httpPort: *defaultHttpPort,
		//defaultHttpSvc: *defaultSvc,
		lbDefAlgorithm: *lbDefAlgorithm,
		tcpSvcs: tcpSvcs,
		//sslCert: sslCertFile,
	}
	lbc := newHaproxyController(kubeClient, api.NamespaceAll, &params)

	lbc.Run()
	glog.Infof("Running...")
	for {
		time.Sleep(30 * time.Second)
	}
}

func createClient(inCluster bool, apiServerHost string, kubeConfigFile string) (*kubernetes.Clientset, error) {
	var config *rest.Config
	var err error
	if inCluster {
		if config, err = rest.InClusterConfig(); err != nil {
			glog.Fatalf("error creating client configuration: %v", err)
		}
	} else {
		if apiServerHost == "" {
			glog.Fatalf("please specify the api server address using the flag --apiserver-host")
		}

		config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeConfigFile},
			&clientcmd.ConfigOverrides{
				ClusterInfo: clientcmdapi.Cluster{
					Server: apiServerHost,
				},
			}).ClientConfig()
		if err != nil {
			glog.Fatalf("error creating client configuration: %v", err)
		}
	}

	return kubernetes.NewForConfig(config)
}

func parseTCPServices(tcpServices string) map[string]int {
	tcpSvcs := make(map[string]int)
	for _, service := range strings.Split(tcpServices, ",") {
		portSplit := strings.Split(service, ":")
		if len(portSplit) != 2 {
			glog.Errorf("Ignoring misconfigured TCP service %v", service)
			continue
		}
		if port, err := strconv.Atoi(portSplit[1]); err != nil {
			glog.Errorf("Ignoring misconfigured TCP service %v: %v", service, err)
			continue
		} else {
			glog.Infof("Adding TCP service %v", service)
			tcpSvcs[portSplit[0]] = port
		}
	}

	return tcpSvcs
}
