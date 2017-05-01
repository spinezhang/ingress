package main

import (
	"encoding/json"
	"fmt"
	"strconv"

	api_v1 "k8s.io/client-go/pkg/api/v1"
	extensions "k8s.io/client-go/pkg/apis/extensions/v1beta1"
	"k8s.io/client-go/tools/cache"
	"os"
	"github.com/golang/glog"
	"strings"
)

const (
	// allowHTTPKey tells the Ingress controller to allow/block HTTP access.
	// If either unset or set to true, the controller will create a
	// forwarding-rule for port 80, and any additional rules based on the TLS
	// section of the Ingress. If set to false, the controller will only create
	// rules for port 443 based on the TLS section.
	allowHTTPKey = "kubernetes.io/ingress.allow-http"

	// serviceApplicationProtocolKey is a stringified JSON map of port names to
	// protocol strings. Possible values are HTTP, HTTPS
	// Example:
	// '{"my-https-port":"HTTPS","my-http-port":"HTTP"}'
	serviceApplicationProtocolKey = "service.alpha.kubernetes.io/app-protocols"

	// ingressClassKey picks a specific "class" for the Ingress. The controller
	// only processes Ingresses with this annotation either unset, or set
	// to either haproxyIngressClass or the empty string.
	ingressClassKey = "kubernetes.io/ingress.class"
	haproxyIngressClass = "haproxy"
	haproxyCrtFile = "/etc/haproxy.pem"
)

// ingAnnotations represents Ingress annotations.
type ingAnnotations map[string]string

// allowHTTP returns the allowHTTP flag. True by default.
func (ing ingAnnotations) allowHTTP() bool {
	val, ok := ing[allowHTTPKey]
	if !ok {
		return true
	}
	v, err := strconv.ParseBool(val)
	if err != nil {
		return true
	}
	return v
}

func (ing ingAnnotations) ingressClass() string {
	val, ok := ing[ingressClassKey]
	if !ok {
		return ""
	}
	return val
}

// svcAnnotations represents Service annotations.
type svcAnnotations map[string]string

func (svc svcAnnotations) ApplicationProtocols() (map[string]string, error) {
	val, ok := svc[serviceApplicationProtocolKey]
	if !ok {
		return map[string]string{}, nil
	}

	var portToProtos map[string]string
	err := json.Unmarshal([]byte(val), &portToProtos)

	// Verify protocol is an accepted value
	for _, proto := range portToProtos {
		switch proto {
		case "HTTP", "HTTPS":
		default:
			return nil, fmt.Errorf("invalid port application protocol: %v", proto)
		}
	}

	return portToProtos, err
}

// isGCEIngress returns true if the given Ingress either doesn't specify the
// ingress.class annotation, or it's set to "gce".
func isHaproxyIngress(ing *extensions.Ingress) bool {
	class := ingAnnotations(ing.ObjectMeta.Annotations).ingressClass()
	return class == "" || class == haproxyIngressClass
}

// errorNodePortNotFound is an implementation of error.
type errorNodePortNotFound struct {
	backend extensions.IngressBackend
	origErr error
}

func (e errorNodePortNotFound) Error() string {
	return fmt.Sprintf("Could not find nodeport for backend %+v: %v",
		e.backend, e.origErr)
}

type errorSvcAppProtosParsing struct {
	svc     *api_v1.Service
	origErr error
}

func (e errorSvcAppProtosParsing) Error() string {
	return fmt.Sprintf("could not parse %v annotation on Service %v/%v, err: %v", serviceApplicationProtocolKey, e.svc.Namespace, e.svc.Name, e.origErr)
}

type ingressByRevision []interface{}

func (c ingressByRevision) Len() int      { return len(c) }
func (c ingressByRevision) Swap(i, j int) { c[i], c[j] = c[j], c[i] }
func (c ingressByRevision) Less(i, j int) bool {
	ir := c[i].(*extensions.Ingress).ResourceVersion
	jr := c[j].(*extensions.Ingress).ResourceVersion
	return ir < jr
}

func removeDuplicate(endpoints []string) []string {
	var result []string = []string{}
	for _, item := range endpoints {
		if len(result) == 0 {
			result = append(result, item)
		} else {
			for k, v := range result {
				if item == v {
					break
				}
				if k == len(result)-1 {
					result = append(result, item)
				}
			}
		}
	}
	return result
}

type sslCertTracker struct {
	cache.ThreadSafeStore
}

func newSSLCertTracker() *sslCertTracker {
	return &sslCertTracker{
		cache.NewThreadSafeStore(cache.Indexers{}, cache.Indices{}),
	}
}

type secretTracker struct {
	cache.ThreadSafeStore
}

func newSecretTracker() *secretTracker {
	return &secretTracker{
		cache.NewThreadSafeStore(cache.Indexers{}, cache.Indices{}),
	}
}

func writeHaproxyCrt(cert []byte, key []byte) error {
	file, err := os.OpenFile(haproxyCrtFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		glog.Errorf("Cannot open file to write:", err)
		return err
	}
	if _,err := file.Write(pureData(cert)); err != nil {
		glog.Errorf("Error writting cert:", err)
		return err
	}
	file.Write([]byte("\n"))
	if _,err := file.Write(pureData(key)); err != nil {
		glog.Errorf("Error writting key:", err)
		return err
	}
	file.Close()
	glog.Infof("Success write ssl cert file:", haproxyCrtFile)

	return nil
}

func pureData(data []byte) []byte {
	str := string(data)
	return []byte(strings.Replace(str, "\r", "", -1))
}