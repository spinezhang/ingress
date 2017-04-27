package main

import (
	"fmt"

	"github.com/golang/glog"

	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	api_v1 "k8s.io/client-go/pkg/api/v1"
	extensions "k8s.io/client-go/pkg/apis/extensions/v1beta1"
)

type TLSCerts struct {
	// Key is private key.
	Key string
	// Cert is a public key.
	Cert string
	// Chain is a certificate chain.
	Chain string
}

// secretLoaders returns a type containing all the secrets of an Ingress.
type tlsLoader interface {
	load(ing *extensions.Ingress) (*TLSCerts, error)
	validate(certs *TLSCerts) error
}

// TODO: Add better cert validation.
type noOPValidator struct{}

func (n *noOPValidator) validate(certs *TLSCerts) error {
	return nil
}

// apiServerTLSLoader loads TLS certs from the apiserver.
type apiServerTLSLoader struct {
	noOPValidator
	client kubernetes.Interface
}

func (t *apiServerTLSLoader) load(ing *extensions.Ingress) (*TLSCerts, error) {
	if len(ing.Spec.TLS) == 0 {
		return nil, nil
	}
	// GCE L7s currently only support a single cert.
	if len(ing.Spec.TLS) > 1 {
		glog.Warningf("Ignoring %d certs and taking the first for ingress %v/%v",
			len(ing.Spec.TLS)-1, ing.Namespace, ing.Name)
	}
	secretName := ing.Spec.TLS[0].SecretName
	// TODO: Replace this for a secret watcher.
	glog.V(3).Infof("Retrieving secret for ing %v with name %v", ing.Name, secretName)
	secret, err := t.client.Core().Secrets(ing.Namespace).Get(secretName, meta_v1.GetOptions{})
	if err != nil {
		return nil, err
	}
	cert, ok := secret.Data[api_v1.TLSCertKey]
	if !ok {
		return nil, fmt.Errorf("secret %v has no private key", secretName)
	}
	key, ok := secret.Data[api_v1.TLSPrivateKeyKey]
	if !ok {
		return nil, fmt.Errorf("secret %v has no cert", secretName)
	}
	certs := &TLSCerts{Key: string(key), Cert: string(cert)}
	if err := t.validate(certs); err != nil {
		return nil, err
	}
	return certs, nil
}

// TODO: Add support for file loading so we can support HTTPS default backends.

// fakeTLSSecretLoader fakes out TLS loading.
type fakeTLSSecretLoader struct {
	noOPValidator
	fakeCerts map[string]*TLSCerts
}

func (f *fakeTLSSecretLoader) load(ing *extensions.Ingress) (*TLSCerts, error) {
	if len(ing.Spec.TLS) == 0 {
		return nil, nil
	}
	for name, cert := range f.fakeCerts {
		if ing.Spec.TLS[0].SecretName == name {
			return cert, nil
		}
	}
	return nil, fmt.Errorf("couldn't find secret for ingress %v", ing.Name)
}
