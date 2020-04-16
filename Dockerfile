# Build executable
FROM golang:1.14-alpine AS builder
WORKDIR /go/src/docker-platformify
COPY . .
RUN go get -d -v
RUN go build -o /docker-platformify

# Create small runtime image from Alpine
FROM alpine:latest
COPY --from=builder /docker-platformify /usr/local/bin/docker-platformify

ENTRYPOINT ["docker-platformify"]
