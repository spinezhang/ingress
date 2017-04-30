package main

import (
	"net/http"
	"io/ioutil"
	"github.com/golang/glog"
	"fmt"
)

type staticPageHandler struct {
	pagePath     string
	pageContents []byte
	returnCode   int
	c            *http.Client
}

func (s *staticPageHandler) Getfunc(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(s.returnCode)
	w.Write(s.pageContents)
}

func (s *staticPageHandler) loadUrl(url string) error {
	res, err := s.c.Get(url)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		glog.Errorf("%v", err)
		return err
	}
	glog.V(2).Infof("Error page:%v", string(body))
	s.pagePath = url
	s.pageContents = body

	return nil
}

// newStaticPageHandler returns a staticPageHandles with the contents of pagePath loaded and ready to serve
// page is a url to the page to load.
// defaultPage is the page to load if page is unreachable.
// returnCode is the HTTP code to write along with the page/defaultPage.
func newStaticPageHandler(page string, defaultPage string, returnCode int) *staticPageHandler {
	t := &http.Transport{}
	t.RegisterProtocol("file", http.NewFileTransport(http.Dir("/")))
	c := &http.Client{Transport: t}
	s := &staticPageHandler{c: c}
	if err := s.loadUrl(page); err != nil {
		s.loadUrl(defaultPage)
	}
	s.returnCode = returnCode
	return s
}

// registerHandlers  services liveness probes.
func registerHandlers(port int, s *staticPageHandler) {
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	})

	// handler for not matched traffic
	//http.HandleFunc("/", s.Getfunc)

	glog.Fatal(http.ListenAndServe(fmt.Sprintf(":%v", port), nil))
}
