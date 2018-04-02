FROM golang:1.9-alpine3.7

RUN apk -U upgrade && \
    apk add --no-cache -U git

RUN mkdir -p $GOPATH/src/brkt/cloudsweeper
ADD . $GOPATH/src/brkt/cloudsweeper/
WORKDIR $GOPATH/src/brkt/cloudsweeper

RUN go get ./...

RUN go build -o cloudsweeper cmd/*.go

ADD https://s3-us-west-2.amazonaws.com/packages.int.brkt.net/org/latest/organization.json ./organization.json

ENTRYPOINT [ "./cloudsweeper" ]
