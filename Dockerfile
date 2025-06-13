FROM golang:1.23-alpine AS builder

WORKDIR /app

COPY go.mod ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o proxy

FROM alpine:latest

WORKDIR /app

COPY --from=builder /app/proxy .

EXPOSE 8089

CMD ["./proxy"]
