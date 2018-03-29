all: push

# 0.0 shouldn't clobber any released builds
TAG ?= 0.9
PREFIX ?= registry.cn-beijing.aliyuncs.com/dhms-p/haproxy
#PREFIX ?= lan.dhms.cloud:5000/haproxy
SRC = main.go haproxy_controller.go utils.go tls.go static_page.go haproxy_log.go templ_service.go

rootfs/haproxy-ingress-controller: $(SRC) Makefile
	CGO_ENABLED=0 GOOS=linux godep go build -a -installsuffix cgo -ldflags '-w -s' -o $@  $(SRC)

container: rootfs/haproxy-ingress-controller
	docker build --pull -t $(PREFIX):$(TAG) rootfs

push: container
	docker push $(PREFIX):$(TAG)

clean:
	docker rmi -f $(PREFIX):$(TAG) || true
	rm -f rootfs/haproxy-ingress-controller

.PHONY: container push
