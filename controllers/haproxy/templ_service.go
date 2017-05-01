package main

const (
	lbAlgorithmKey           = "serviceloadbalancer/lb.algorithm"
	lbHostKey                = "serviceloadbalancer/lb.host"
	lbSslTerm                = "serviceloadbalancer/lb.sslTerm"
	//lbAclMatch               = "serviceloadbalancer/lb.aclMatch"
	lbCookieStickySessionKey = "serviceloadbalancer/lb.cookie-sticky-session"
)

type haService struct {
	Name string
	Ep   []string

	// Kubernetes endpoint port. The application must serve a 200 page on this port.
	BackendPort int

	// FrontendPort is the port that the loadbalancer listens on for traffic
	// for this service. For http, it's always :80, for each tcp service it
	// is the service port of any service matching a name in the tcpServices set.
	FrontendPort int

	// Host if not empty it will add a new haproxy acl to route traffic using the
	// host header inside the http request. It only applies to http traffic.
	Host string

	SslTerm bool

	// if set use this to match the path rule
	Path string

	// Algorithm
	Algorithm string

	// If SessionAffinity is set and without CookieStickySession, requests are routed to
	// a backend based on client ip. If both SessionAffinity and CookieStickSession are
	// set, a SERVERID cookie is inserted by the loadbalancer and used to route subsequent
	// requests. If neither is set, requests are routed based on the algorithm.

	// Indicates if the service must use sticky sessions
	// http://cbonte.github.io/haproxy-dconv/configuration-1.5.html#stick-table
	// Enabled using the attribute service.spec.sessionAffinity
	// https://github.com/kubernetes/kubernetes/blob/master/docs/user-guide/services.md#virtual-ips-and-service-proxies
	SessionAffinity bool

	// CookieStickySession use a cookie to enable sticky sessions.
	// The name of the cookie is SERVERID
	// This only can be used in http services
	CookieStickySession bool
}

type serviceByName []haService

func (s serviceByName) Len() int {
	return len(s)
}

func (s serviceByName) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s serviceByName) Less(i, j int) bool {
	return s[i].Name < s[j].Name
}

type serviceAnnotations map[string]string

func (s serviceAnnotations) getAlgorithm() (string, bool) {
	val, ok := s[lbAlgorithmKey]
	return val, ok
}

//func (s serviceAnnotations) getHost() (string, bool) {
//	val, ok := s[lbHostKey]
//	return val, ok
//}

func (s serviceAnnotations) getCookieStickySession() (string, bool) {
	val, ok := s[lbCookieStickySessionKey]
	return val, ok
}

func (s serviceAnnotations) getSslTerm() (string, bool) {
	val, ok := s[lbSslTerm]
	return val, ok
}

//func (s serviceAnnotations) getSslByPass() (string, bool) {
//	val, ok := s[lbSslByPass]
//	return val, ok
//}

//func (s serviceAnnotations) getAclMatch() (string, bool) {
//	val, ok := s[lbAclMatch]
//	return val, ok
//}
