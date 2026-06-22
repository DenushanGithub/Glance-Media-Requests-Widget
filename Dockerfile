FROM golang:1.25-alpine AS builder
WORKDIR /app
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o seerr-app .

FROM alpine:latest
WORKDIR /app
RUN apk add --no-cache tzdata ca-certificates
COPY --from=builder /app/seerr-app .
EXPOSE 5000
CMD ["./seerr-app"]