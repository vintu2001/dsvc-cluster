FROM golang:alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-w -s" -o /dsvc-node ./cmd/storagenode

FROM alpine:latest

RUN apk add --no-cache ca-certificates wget

COPY --from=builder /dsvc-node /dsvc-node

EXPOSE 8080
EXPOSE 7000

ENTRYPOINT ["/dsvc-node"]
