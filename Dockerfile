# syntax=docker/dockerfile:1
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
ARG COMMIT=
ARG DATE=
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w \
    -X main.version=${VERSION} \
    -X main.commit=${COMMIT} \
    -X main.date=${DATE}" -o /ooshare ./cmd/ooshare

FROM alpine:3.20
RUN addgroup -S ooshare && adduser -S -G ooshare ooshare
COPY --from=build /ooshare /usr/local/bin/ooshare
USER ooshare
EXPOSE 8088
ENTRYPOINT ["/usr/local/bin/ooshare"]
