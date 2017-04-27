# Copyright 2015 The Kubernetes Authors. All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

FROM registry.cn-hangzhou.aliyuncs.com/dhms/haproxy:1.7-alpine

RUN mkdir -p /etc/haproxy/errors /var/state/haproxy
COPY errors /etc/haproxy/errors
COPY haproxy.cfg /etc/haproxy/haproxy.cfg
COPY haproxy-ingress-controller /haproxy-ingress-controller
RUN chmod +x /haproxy-ingress-controller
COPY haproxy.tmpl haproxy.tmpl
COPY nginx.tmpl nginx.tmpl
COPY haproxy_reload /haproxy_reload
RUN chmod +x /haproxy_reload

ENTRYPOINT ["/haproxy-ingress-controller"]

RUN touch /var/run/haproxy.pid
#RUN apk add --no-cache bash
#RUN apk --no-cache add socat