# syntax=docker/dockerfile:1.4

FROM golang:1.21-alpine AS build-dev
WORKDIR /go/src/app
COPY --link go.mod go.sum ./
RUN apk add --no-cache upx || \
    go version && \
    go mod download
COPY --link . .
RUN CGO_ENABLED=0 go install -buildvcs=false -trimpath -ldflags '-w -s'
RUN [ -e /usr/bin/upx ] && upx /go/bin/nostr-buzzword || echo
FROM scratch
COPY --link --from=build-dev /go/bin/nostr-buzzword /go/bin/nostr-buzzword
COPY --link --from=build-dev /go/src/app/userdic.txt /go/bin/userdic.txt
COPY --from=build-dev /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
ENV USERDIC=/go/bin/userdic.txt
CMD ["/go/bin/nostr-buzzword"]
