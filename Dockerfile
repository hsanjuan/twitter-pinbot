FROM golang:1.12-stretch
MAINTAINER Hector Sanjuan <hector@protocol.ai>

# This dockerfile builds and runs twitter-bot

ENV GOPATH /go
ENV SRC_PATH $GOPATH/src/github.com/hsanjuan/twitter-pinbot
ENV GO111MODULE on
ENV GOPROXY https://proxy.golang.org

COPY . $SRC_PATH
WORKDIR $SRC_PATH

RUN go install

VOLUME /data

WORKDIR $GOPATH /bin
ENTRYPOINT ["twitter-pinbot"]
CMD ["-config", "/data/config.json"]