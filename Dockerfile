FROM golang:alpine as builder

ADD . /usr/src/k8s-net-attach-def-controller

ENV HTTP_PROXY $http_proxy
ENV HTTPS_PROXY $https_proxy
RUN apk add --update make && \
    cd /usr/src/k8s-net-attach-def-controller && \
    make clean && \
    make build

RUN ls -al /usr/src/k8s-net-attach-def-controller

FROM alpine
COPY --from=builder /usr/src/k8s-net-attach-def-controller/build/k8s-net-attach-def-controller /usr/bin/
WORKDIR /

LABEL io.k8s.display-name="Network Attachment Definitions Controller"

ENTRYPOINT ["/usr/bin/k8s-net-attach-def-controller", "--alsologtostderr"]

