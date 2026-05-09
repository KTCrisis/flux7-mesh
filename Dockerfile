FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /mesh7 ./cmd/mesh7

FROM alpine:3.21
COPY --from=builder /mesh7 /usr/local/bin/mesh7
RUN echo 'listen: ":9090"' > /etc/mesh7.yaml
ENTRYPOINT ["mesh7", "--mcp", "--config", "/etc/mesh7.yaml"]
