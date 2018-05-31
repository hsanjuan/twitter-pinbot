FROM golang:1.10-stretch
MAINTAINER Hector Sanjuan <hector@protocol.ai>

# This dockerfile builds and runs twitter-bot

ENV GOPATH /go
ENV SRC_PATH $GOPATH/src/github.com/hsanjuan/twitter-pinbot

COPY . $SRC_PATH
WORKDIR $SRC_PATH

RUN go get ./... \
    && go install

VOLUME /data

WORKDIR $GOPATH /bin
ENTRYPOINT ["twitter-pinbot"]
CMD ["-config", "/data/config.json"]