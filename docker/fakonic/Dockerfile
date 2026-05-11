FROM golang:1.22-alpine AS builder

WORKDIR /src
COPY go.mod main.go ./
RUN go build -o /fakonic .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates
COPY --from=builder /fakonic /usr/local/bin/fakonic

ENTRYPOINT ["/usr/local/bin/fakonic"]
