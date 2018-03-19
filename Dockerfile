FROM alpine:3.7 AS organization
RUN apk -U upgrade && \
    apk add --no-cache -U git
ARG CACHE_DATE=a_date
RUN git clone https://jenkins-ro:YrerrGLoNE9fcZ9Vn99YHqrN@gerrit.int.brkt.net/a/brkt-infrastructure /src/brkt-infrastructure && \
    cp /src/brkt-infrastructure/organization/organization.json /src/organization.json && \
    rm -rf /src/brkt-infrastructure

FROM golang:1.9-alpine3.7

RUN apk -U upgrade && \
    apk add --no-cache -U git

RUN mkdir -p $GOPATH/src/brkt/cloudsweeper
ADD . $GOPATH/src/brkt/cloudsweeper/
WORKDIR $GOPATH/src/brkt/cloudsweeper

RUN go get ./...

COPY --from=organization /src/organization.json ./organization.json
RUN go build -o cloudsweeper cmd/*.go
ENTRYPOINT [ "./cloudsweeper" ]
