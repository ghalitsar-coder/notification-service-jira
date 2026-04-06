FROM golang:1.24-alpine AS builder
WORKDIR /app

COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -o /notification-service ./main.go

FROM alpine:3.20
WORKDIR /app
RUN apk add --no-cache ca-certificates sqlite
COPY --from=builder /notification-service /notification-service
RUN mkdir -p /app/data
EXPOSE 8081
ENTRYPOINT ["/notification-service"]
