# syntax=docker/dockerfile:1.4

FROM golang:1.25-alpine AS build-dev
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
COPY --link --from=build-dev /go/src/app/ignores.txt /go/bin/ignores.txt
COPY --link --from=build-dev /go/src/app/badwords.txt /go/bin/badwords.txt
COPY --link --from=build-dev /go/src/app/Koruri-Regular.ttf /go/bin/Koruri-Regular.ttf
COPY --from=build-dev /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
ENV USERDIC=/go/bin/userdic.txt
ENV IGNORES=/go/bin/ignores.txt
ENV BADWORDS=/go/bin/badwords.txt
ENV FONTFILE=/go/bin/Koruri-Regular.ttf
CMD ["/go/bin/nostr-buzzword"]
